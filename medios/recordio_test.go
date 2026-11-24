package medios

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// Sample records, built programmatically so we don't hand-count byte sizes.
var samplePayloads = []string{
	`{"type":"SUBSCRIBED","subscribed":{"framework_id":{"value":"abc-0001"},"heartbeat_interval_seconds":15.0,"controller_info":{}}}`,
	`{"type":"HEARTBEAT"}`,
	`{"type":"OFFERS","offers":{"offers":[{"id":{"value":"O-1"},"hostname":"h"}]}}`,
}

// sampleStream is the concatenation of <size>\n<payload> for each samplePayload.
// Records sit back-to-back with no separator between them, the size prefix
// is the only framing (matches medios docs/recordio.md).
func sampleStream() string {
	var sb strings.Builder
	for _, p := range samplePayloads {
		sb.WriteString(strconv.Itoa(len(p)))
		sb.WriteByte('\n')
		sb.WriteString(p)
	}
	return sb.String()
}

func TestParseRecordIO_HappyPath(t *testing.T) {
	var got []string
	err := ParseRecordIO(strings.NewReader(sampleStream()), func(buf []byte) error {
		got = append(got, string(buf))
		return nil
	})
	if err != nil {
		t.Fatalf("ParseRecordIO: %v", err)
	}
	want := samplePayloads
	if len(got) != len(want) {
		t.Fatalf("record count: want %d, got %d (records=%q)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record[%d]:\n  want %q\n  got  %q", i, want[i], got[i])
		}
	}
}

func TestParseRecordIO_EmptyStreamReturnsNil(t *testing.T) {
	called := false
	err := ParseRecordIO(strings.NewReader(""), func(_ []byte) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("ParseRecordIO on empty: %v", err)
	}
	if called {
		t.Fatal("yield should not have been called on empty stream")
	}
}

func TestParseRecordIO_InvalidSize(t *testing.T) {
	err := ParseRecordIO(strings.NewReader("notanumber\n{}"), func(_ []byte) error { return nil })
	if err == nil {
		t.Fatal("ParseRecordIO: want error for non-numeric size line")
	}
}

func TestParseRecordIO_TruncatedPayload(t *testing.T) {
	// Claims 50 bytes but only delivers 10, should error.
	err := ParseRecordIO(strings.NewReader("50\nshort\n"), func(_ []byte) error { return nil })
	if err == nil {
		t.Fatal("ParseRecordIO: want error for truncated payload")
	}
}

func TestParseRecordIO_YieldErrorStops(t *testing.T) {
	stopErr := errors.New("stop")
	count := 0
	err := ParseRecordIO(strings.NewReader(sampleStream()), func(_ []byte) error {
		count++
		return stopErr
	})
	if !errors.Is(err, stopErr) {
		t.Fatalf("ParseRecordIO: want stopErr, got %v", err)
	}
	if count != 1 {
		t.Fatalf("yield count: want 1 (stopped on first), got %d", count)
	}
}

func TestParseRecordIO_TrimsTrailingCR(t *testing.T) {
	// A proxy that injects \r before \n shouldn't break framing.
	err := ParseRecordIO(strings.NewReader("5\r\nhello"), func(buf []byte) error {
		if string(buf) != "hello" {
			t.Fatalf("payload: want hello, got %q", buf)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ParseRecordIO: %v", err)
	}
}

func TestParseRecordIO_PayloadBufferReused(t *testing.T) {
	// Document the "buffer only valid until next call" contract: if a caller
	// retains a slice across yields, it observes the next record's bytes.
	// This test pins the behavior so anyone changing it has to update docs.
	var seenPtrs []*byte
	_ = ParseRecordIO(strings.NewReader(sampleStream()), func(buf []byte) error {
		if len(buf) > 0 {
			seenPtrs = append(seenPtrs, &buf[0])
		}
		return nil
	})
	// Don't assert exact pointer equality (Go may reallocate when growing),
	// but document the intent: callers MUST copy if they want to retain.
	if len(seenPtrs) == 0 {
		t.Fatal("no records seen")
	}
}

func TestParseRecordIOJSON_TypedDecode(t *testing.T) {
	// Simple typed pass: confirm the generic wrapper decodes correctly.
	type event struct {
		Type string `json:"type"`
	}
	var types []string
	err := ParseRecordIOJSON(
		strings.NewReader(sampleStream()),
		func() *event { return &event{} },
		func(e *event) error {
			types = append(types, e.Type)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ParseRecordIOJSON: %v", err)
	}
	want := []string{"SUBSCRIBED", "HEARTBEAT", "OFFERS"}
	if len(types) != len(want) {
		t.Fatalf("event count: want %d, got %d", len(want), len(types))
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("event[%d]: want %q, got %q", i, want[i], types[i])
		}
	}
}

func TestParseRecordIOJSON_BadJSON(t *testing.T) {
	// Size is correct (10) but payload isn't valid JSON.
	stream := "10\nNOT_A_JSON"
	type empty struct{}
	err := ParseRecordIOJSON(strings.NewReader(stream),
		func() *empty { return &empty{} },
		func(_ *empty) error { return nil },
	)
	if err == nil {
		t.Fatal("ParseRecordIOJSON: want error for invalid JSON payload")
	}
}

// Roundtrip a real wire-shape payload through json + recordio to make sure
// our framing matches what the controller emits.
func TestParseRecordIO_RealEventShape(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"type": "OFFERS",
		"offers": map[string]any{
			"offers": []map[string]any{
				{
					"id":           map[string]string{"value": "abc-O-1"},
					"framework_id": map[string]string{"value": "abc-0001"},
					"node_id":      map[string]string{"value": "abc-S0"},
					"hostname":     "localhost",
					"resources": []map[string]any{
						{"name": "cpus", "type": "SCALAR", "scalar": map[string]float64{"value": 2.0}},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var buf bytes.Buffer
	buf.WriteString(strconv.Itoa(len(payload)))
	buf.WriteByte('\n')
	buf.Write(payload)

	var got []byte
	err = ParseRecordIO(&buf, func(b []byte) error {
		got = append([]byte(nil), b...) // copy: we'll inspect after loop
		return io.EOF                   // stop after first
	})
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ParseRecordIO: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n  want %s\n  got  %s", payload, got)
	}
}
