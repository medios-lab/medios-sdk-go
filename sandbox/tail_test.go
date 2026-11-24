package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newInMemoryResolver(
	controllerHandler http.HandlerFunc,
	nodeHandler http.HandlerFunc,
) *Resolver {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := req.Context().Err(); err != nil {
				return nil, err
			}
			rec := httptest.NewRecorder()
			switch req.URL.Host {
			case "controller.local:5050":
				controllerHandler(rec, req)
			case "node1.local:5151":
				nodeHandler(rec, req)
			default:
				return nil, fmt.Errorf("connection refused to %s", req.URL.Host)
			}
			return rec.Result(), nil
		}),
	}
	return &Resolver{
		ControllerURL: "http://controller.local:5050",
		Client:        client,
	}
}

func TestClassifyError(t *testing.T) {
	if got := ClassifyError(nil); got != "" {
		t.Errorf("ClassifyError(nil) = %q, want empty", got)
	}
	cases := []struct {
		err  error
		want string
	}{
		{ErrNodeUnknown, ReasonNodeUnknown},
		{fmt.Errorf("wrapped: %w", ErrNodeUnknown), ReasonNodeUnknown},
		{ErrControllerUnreachable, ReasonControllerUnreachable},
		{fmt.Errorf("wrapped: %w", ErrControllerUnreachable), ReasonControllerUnreachable},
		{ErrNodeUnreachable, ReasonNodeUnreachable},
		{fmt.Errorf("wrapped: %w", ErrNodeUnreachable), ReasonNodeUnreachable},
		{ErrNotFound, ReasonFileMissing},
		{fmt.Errorf("wrapped: %w", fs.ErrNotExist), ReasonFileMissing},
		{errors.New("sandbox: node returned 500"), ReasonReadFailed},
	}
	for _, tc := range cases {
		if got := ClassifyError(tc.err); got != tc.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestTrimUTF8Tail(t *testing.T) {
	// 4-byte UTF-8 rune '🚀' is 0xF0 0x9F 0x9A 0x80.
	// Slicing after the leading byte leaves 3 continuation bytes: 0x9F 0x9A 0x80.
	rocket := "🚀ok"
	split3 := rocket[1:] // 3 leading continuation bytes + "ok"
	if got := TrimUTF8Tail(split3, true); got != "ok" {
		t.Errorf("TrimUTF8Tail(3 cont bytes, truncated=true) = %q, want %q", got, "ok")
	}

	split2 := rocket[2:] // 2 leading continuation bytes + "ok"
	if got := TrimUTF8Tail(split2, true); got != "ok" {
		t.Errorf("TrimUTF8Tail(2 cont bytes, truncated=true) = %q, want %q", got, "ok")
	}

	split1 := rocket[3:] // 1 leading continuation byte + "ok"
	if got := TrimUTF8Tail(split1, true); got != "ok" {
		t.Errorf("TrimUTF8Tail(1 cont byte, truncated=true) = %q, want %q", got, "ok")
	}

	// When truncated is false, leading continuation bytes are NOT dropped;
	// they are replaced with U+FFFD.
	if got := TrimUTF8Tail(split1, false); got != "\uFFFDok" {
		t.Errorf("TrimUTF8Tail(1 cont byte, truncated=false) = %q, want %q", got, "\uFFFDok")
	}

	// At most 3 leading continuation bytes are dropped when truncated is true;
	// a 4th continuation byte is replaced with U+FFFD.
	fourCont := "\x80\x80\x80\x80ok"
	if got := TrimUTF8Tail(fourCont, true); got != "\uFFFDok" {
		t.Errorf("TrimUTF8Tail(4 cont bytes, truncated=true) = %q, want %q", got, "\uFFFDok")
	}

	// Interior invalid UTF-8 bytes are replaced with U+FFFD.
	interiorBad := "hello\xffworld"
	if got := TrimUTF8Tail(interiorBad, false); got != "hello\uFFFDworld" {
		t.Errorf("TrimUTF8Tail(interior invalid) = %q, want %q", got, "hello\uFFFDworld")
	}
}

func TestCaptureTail_ValidationAndEmptyFile(t *testing.T) {
	r := newInMemoryResolver(
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []map[string]string{{"id": "S1", "pid": "node(1)@node1.local:5151"}},
			})
		},
		func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/state":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"flags": map[string]string{"work_dir": "/var/lib/medios"},
				})
			case "/files/read":
				_ = json.NewEncoder(w).Encode(ReadResult{Data: "", Offset: 0})
			}
		},
	)

	if _, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", 0); err == nil {
		t.Fatal("CaptureTail with maxBytes=0 should return an error")
	}
	if _, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", -10); err == nil {
		t.Fatal("CaptureTail with negative maxBytes should return an error")
	}

	res, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", 256)
	if err != nil {
		t.Fatalf("CaptureTail(empty): %v", err)
	}
	if res.Data != "" || res.Size != 0 || res.Truncated {
		t.Errorf("CaptureTail(empty) = %+v, want empty non-truncated", res)
	}
}

func TestCaptureTail_FullAndTruncated(t *testing.T) {
	content := "head-prefix-{\"answer\":42}\n"
	size := int64(len(content))

	r := newInMemoryResolver(
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []map[string]string{{"id": "S1", "pid": "node(1)@node1.local:5151"}},
			})
		},
		func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/state":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"flags": map[string]string{"work_dir": "/var/lib/medios"},
				})
			case "/files/read":
				q := req.URL.Query()
				off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
				if off == -1 {
					_ = json.NewEncoder(w).Encode(ReadResult{Data: "", Offset: size})
					return
				}
				length, _ := strconv.ParseInt(q.Get("length"), 10, 64)
				end := off + length
				if end > size {
					end = size
				}
				_ = json.NewEncoder(w).Encode(ReadResult{
					Data:   content[off:end],
					Offset: off,
				})
			}
		},
	)

	// 1. Full read (maxBytes >= size)
	full, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", 1024)
	if err != nil {
		t.Fatalf("CaptureTail(full): %v", err)
	}
	if full.Data != content || full.Size != size || full.Truncated {
		t.Errorf("CaptureTail(full) = %+v, want Data=%q Size=%d Truncated=false", full, content, size)
	}

	// 2. Truncated read (maxBytes < size)
	tailJSON := "{\"answer\":42}\n"
	maxBytes := int64(len(tailJSON))
	trunc, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", maxBytes)
	if err != nil {
		t.Fatalf("CaptureTail(truncated): %v", err)
	}
	if !trunc.Truncated || trunc.Size != size || trunc.Data != tailJSON {
		t.Errorf("CaptureTail(truncated) = %+v, want Data=%q Size=%d Truncated=true", trunc, tailJSON, size)
	}
}

func TestCaptureTail_SplitUTF8ThroughJSONAndLiteralFFFD(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		maxBytes int64
		wantData string
		wantSize int64
	}{
		{
			name:     "4-byte rocket split at byte 1 (maxBytes=5)",
			content:  "🚀ok",
			maxBytes: 5,
			wantData: "ok",
			wantSize: 6,
		},
		{
			name:     "4-byte rocket split at byte 2 (maxBytes=4)",
			content:  "🚀ok",
			maxBytes: 4,
			wantData: "ok",
			wantSize: 6,
		},
		{
			name:     "4-byte rocket split at byte 3 (maxBytes=3)",
			content:  "🚀ok",
			maxBytes: 3,
			wantData: "ok",
			wantSize: 6,
		},
		{
			name:     "adjacent multi-byte emojis where fetchStart also lands mid-rune",
			content:  "🎉🚀ok",
			maxBytes: 5,
			wantData: "ok",
			wantSize: 10,
		},
		{
			name:     "legitimate literal U+FFFD at cut boundary is preserved",
			content:  "abc\uFFFDok",
			maxBytes: 5,
			wantData: "\uFFFDok",
			wantSize: 8,
		},
		{
			name:     "invalid single byte later in tail does not drop rune at start after literal U+FFFD",
			content:  "ab\uFFFDX\xffok",
			maxBytes: 4,
			wantData: "X\uFFFDok",
			wantSize: 9,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.content
			size := int64(len(content))
			r := newInMemoryResolver(
				func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"nodes": []map[string]string{{"id": "S1", "pid": "node(1)@node1.local:5151"}},
					})
				},
				func(w http.ResponseWriter, req *http.Request) {
					switch req.URL.Path {
					case "/state":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"flags": map[string]string{"work_dir": "/var/lib/medios"},
						})
					case "/files/read":
						q := req.URL.Query()
						off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
						if off == -1 {
							_ = json.NewEncoder(w).Encode(ReadResult{Data: "", Offset: size})
							return
						}
						length, _ := strconv.ParseInt(q.Get("length"), 10, 64)
						end := off + length
						if end > size {
							end = size
						}
						_ = json.NewEncoder(w).Encode(ReadResult{
							Data:   content[off:end],
							Offset: off,
						})
					}
				},
			)
			got, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", tc.maxBytes)
			if err != nil {
				t.Fatalf("CaptureTail: %v", err)
			}
			if !got.Truncated || got.Size != tc.wantSize || got.Data != tc.wantData {
				t.Errorf("CaptureTail = %+v, want Data=%q Size=%d Truncated=true", got, tc.wantData, tc.wantSize)
			}
		})
	}
}

func TestCaptureTail_MultiChunkReadAbove64KiB(t *testing.T) {
	const maxNodeChunk = 65536
	const totalSize = 100003
	prefix := make([]byte, totalSize-3)
	for i := range prefix {
		prefix[i] = byte('a' + (i % 26))
	}
	// Place a 4-byte UTF-8 rune ("🚀") across the 65536-byte chunk boundary (offsets 65534..65537).
	copy(prefix[maxNodeChunk-2:maxNodeChunk+2], "🚀")
	content := string(prefix) + "END"
	size := int64(len(content))

	var dataReads atomic.Int32
	r := newInMemoryResolver(
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []map[string]string{{"id": "S1", "pid": "node(1)@node1.local:5151"}},
			})
		},
		func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/state":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"flags": map[string]string{"work_dir": "/var/lib/medios"},
				})
			case "/files/read":
				q := req.URL.Query()
				off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
				if off == -1 {
					_ = json.NewEncoder(w).Encode(ReadResult{Data: "", Offset: size})
					return
				}
				dataReads.Add(1)
				length, _ := strconv.ParseInt(q.Get("length"), 10, 64)
				if length > maxNodeChunk {
					length = maxNodeChunk
				}
				end := off + length
				if end > size {
					end = size
				}
				_ = json.NewEncoder(w).Encode(ReadResult{
					Data:   content[off:end],
					Offset: off,
				})
			}
		},
	)

	got, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", totalSize)
	if err != nil {
		t.Fatalf("CaptureTail(100003): %v", err)
	}
	if got.Truncated {
		t.Errorf("got.Truncated = true, want false")
	}
	if got.Size != totalSize {
		t.Errorf("got.Size = %d, want %d", got.Size, totalSize)
	}
	if len(got.Data) != totalSize || got.Data != content {
		t.Fatalf("len(got.Data) = %d (ends with %q), want %d ending in END",
			len(got.Data), got.Data[len(got.Data)-3:], totalSize)
	}
	if got.Data[maxNodeChunk-2:maxNodeChunk+2] != "🚀" {
		t.Errorf("split rune across 64KiB boundary = %q, want %q", got.Data[maxNodeChunk-2:maxNodeChunk+2], "🚀")
	}
	if dataReads.Load() < 2 {
		t.Errorf("dataReads = %d, want >= 2 chunks across 64KiB boundary", dataReads.Load())
	}
}

func TestCaptureTail_RetriesTransientAndSkipsPermanent(t *testing.T) {
	origDelays := defaultTailRetryDelays
	defaultTailRetryDelays = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}
	defer func() { defaultTailRetryDelays = origDelays }()

	var readAttempts atomic.Int32
	r := newInMemoryResolver(
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []map[string]string{{"id": "S1", "pid": "node(1)@node1.local:5151"}},
			})
		},
		func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/state":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"flags": map[string]string{"work_dir": "/var/lib/medios"},
				})
			case "/files/read":
				n := readAttempts.Add(1)
				if n <= 2 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if req.URL.Query().Get("offset") == "-1" {
					_ = json.NewEncoder(w).Encode(ReadResult{Data: "", Offset: 2})
					return
				}
				_ = json.NewEncoder(w).Encode(ReadResult{Data: "ok", Offset: 0})
			}
		},
	)

	res, err := r.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", 64)
	if err != nil {
		t.Fatalf("CaptureTail after 2 transient 500s should succeed on 3rd attempt: %v", err)
	}
	if res.Data != "ok" || res.Size != 2 || res.Truncated {
		t.Errorf("unexpected result after retry: %+v", res)
	}
	// 2 failed size reads + 1 successful size read + 1 data read = 4 /files/read requests
	if readAttempts.Load() != 4 {
		t.Errorf("readAttempts = %d, want 4", readAttempts.Load())
	}

	// Permanent error: ErrNotFound (404) must NOT be retried.
	var notFoundAttempts atomic.Int32
	r404 := newInMemoryResolver(
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []map[string]string{{"id": "S1", "pid": "node(1)@node1.local:5151"}},
			})
		},
		func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/state":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"flags": map[string]string{"work_dir": "/var/lib/medios"},
				})
			case "/files/read":
				notFoundAttempts.Add(1)
				w.WriteHeader(http.StatusNotFound)
			}
		},
	)
	_, err = r404.CaptureTail(context.Background(), "S1", "fw", "t1", "c1", "stdout", 64)
	if !errors.Is(err, ErrNotFound) || ClassifyError(err) != ReasonFileMissing {
		t.Fatalf("want ErrNotFound (file_missing), got %v (%s)", err, ClassifyError(err))
	}
	if notFoundAttempts.Load() != 1 {
		t.Errorf("ErrNotFound was retried: attempts = %d, want 1", notFoundAttempts.Load())
	}

	// Permanent error: ErrNodeUnknown must NOT be retried.
	var ctlAttempts atomic.Int32
	rUnknown := newInMemoryResolver(
		func(w http.ResponseWriter, _ *http.Request) {
			ctlAttempts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []map[string]string{}})
		},
		func(http.ResponseWriter, *http.Request) {},
	)
	_, err = rUnknown.CaptureTail(context.Background(), "S-missing", "fw", "t1", "c1", "stdout", 64)
	if !errors.Is(err, ErrNodeUnknown) || ClassifyError(err) != ReasonNodeUnknown {
		t.Fatalf("want ErrNodeUnknown (node_unknown), got %v (%s)", err, ClassifyError(err))
	}
	if ctlAttempts.Load() != 1 {
		t.Errorf("ErrNodeUnknown was retried: ctlAttempts = %d, want 1", ctlAttempts.Load())
	}
}
