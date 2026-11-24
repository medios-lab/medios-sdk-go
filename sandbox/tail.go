package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"
)

// Error classification reasons for sandbox reads and tail captures.
const (
	ReasonNodeUnknown           = "node_unknown"
	ReasonControllerUnreachable = "controller_unreachable"
	ReasonNodeUnreachable       = "node_unreachable"
	ReasonFileMissing           = "file_missing"
	ReasonReadFailed            = "read_failed"
)

// TailResult is the outcome of a bounded tail read from a task's sandbox file.
type TailResult struct {
	Data      string
	Size      int64
	Truncated bool
}

// Default retry delays for transient CaptureTail failures.
var defaultTailRetryDelays = []time.Duration{
	250 * time.Millisecond,
	1 * time.Second,
}

// ClassifyError maps a sandbox read or locate error to one of the five
// stable failure reason codes:
//   - ErrNodeUnknown           -> ReasonNodeUnknown ("node_unknown")
//   - ErrControllerUnreachable -> ReasonControllerUnreachable ("controller_unreachable")
//   - ErrNodeUnreachable       -> ReasonNodeUnreachable ("node_unreachable")
//   - ErrNotFound              -> ReasonFileMissing ("file_missing")
//   - any other non-nil error  -> ReasonReadFailed ("read_failed")
//
// Returns "" when err is nil.
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrNodeUnknown):
		return ReasonNodeUnknown
	case errors.Is(err, ErrControllerUnreachable):
		return ReasonControllerUnreachable
	case errors.Is(err, ErrNodeUnreachable):
		return ReasonNodeUnreachable
	case errors.Is(err, ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return ReasonFileMissing
	default:
		return ReasonReadFailed
	}
}

// TrimUTF8Tail sanitizes a captured byte slice into valid UTF-8 text.
// When truncated is true, up to 3 leading UTF-8 continuation bytes
// ((b & 0xC0) == 0x80) left over from a multi-byte rune split by the
// offset boundary are dropped before replacing any remaining invalid UTF-8
// sequences with U+FFFD.
func TrimUTF8Tail(raw string, truncated bool) string {
	if truncated {
		dropped := 0
		for len(raw) > 0 && dropped < utf8.UTFMax-1 && (raw[0]&0xC0) == 0x80 {
			raw = raw[1:]
			dropped++
		}
	}
	return strings.ToValidUTF8(raw, "\uFFFD")
}

// alignStartOffset advances start (by 0..3 bytes) past any trailing
// continuation bytes of a multi-byte UTF-8 rune that straddles start,
// before the main chunk read loop begins.
//
// Because Go's encoding/json replaces invalid UTF-8 continuation bytes at
// the start of a JSON string with 3-byte U+FFFD runes during Decode, reading
// directly at start would lose the distinction between a split multi-byte
// rune and a literal U+FFFD rune in the file. By inspecting at most the
// 6-byte window [max(0, start-3), min(size, start+3)] around start,
// alignStartOffset includes the starter byte of any rune straddling start
// and returns the exact post-rune file offset.
func (r *Resolver) alignStartOffset(
	ctx context.Context,
	node Node,
	sandboxPath string,
	start, size int64,
) (int64, error) {
	if start <= 0 || start >= size {
		return start, nil
	}
	fetchStart := start - int64(utf8.UTFMax-1)
	if fetchStart < 0 {
		fetchStart = 0
	}
	winEnd := start + int64(utf8.UTFMax-1)
	if winEnd > size {
		winEnd = size
	}
	winLen := int(winEnd - fetchStart)
	win, err := r.Read(ctx, node, sandboxPath, fetchStart, int64(winLen))
	if err != nil {
		return 0, err
	}
	// Fast path: when no byte in [fetchStart, winEnd) was expanded to U+FFFD
	// by JSON decoding, win.Data has exact byte-for-byte correspondence with
	// the file slice [fetchStart, winEnd).
	if len(win.Data) == winLen {
		tailSlice := win.Data[int(start-fetchStart):]
		dropped := 0
		for dropped < len(tailSlice) && dropped < utf8.UTFMax-1 && (tailSlice[dropped]&0xC0) == 0x80 {
			dropped++
		}
		return start + int64(dropped), nil
	}

	// Slow path: an earlier split rune at fetchStart or an invalid byte in
	// [fetchStart, winEnd) expanded to U+FFFD. A multi-byte UTF-8 rune
	// straddles start from k in [fetchStart, start-1] to e in [start+1, min(size, k+4)]
	// iff [k, e) decodes as a single valid rune of exact byte length e-k.
	for k := fetchStart; k < start; k++ {
		maxEnd := k + int64(utf8.UTFMax)
		if maxEnd > size {
			maxEnd = size
		}
		for e := start + 1; e <= maxEnd; e++ {
			spanLen := int(e - k)
			probe, err := r.Read(ctx, node, sandboxPath, k, int64(spanLen))
			if err != nil {
				return 0, err
			}
			if len(probe.Data) == spanLen && utf8.RuneCountInString(probe.Data) == 1 {
				return e, nil
			}
		}
	}
	return start, nil
}

// CaptureTail locates nodeID and reads at most the last maxBytes of file
// inside the task's sandbox, returning a UTF-8-safe TailResult.
//
// Mechanics:
//  1. Validates maxBytes > 0.
//  2. Locates the node via r.Locate(ctx, nodeID) and computes the sandbox
//     path via node.PathFor(frameworkID, taskID, containerID, file).
//  3. Issues a two-step read: r.Read(ctx, node, path, -1, 0) to learn the
//     file's current total size (res.Offset). If size == 0, returns an empty
//     TailResult immediately. Otherwise computes start = max(0, size-maxBytes)
//     and truncated = size > maxBytes.
//  4. When truncated is true, aligns start to the next UTF-8 rune boundary
//     via alignStartOffset so any multi-byte rune straddling start is cleanly
//     dropped before encoding/json can replace its continuation bytes with U+FFFD.
//  5. Reads [alignedStart, size) in a loop to handle the node's per-request
//     read cap (64 KiB in Medios FilesProcess::_read), re-aligning if a
//     multi-byte UTF-8 rune straddles an interior chunk boundary, and stopping
//     cleanly if the file shrinks on disk (len(slice.Data) == 0).
//  6. Retries transient failures (ErrControllerUnreachable, ErrNodeUnreachable,
//     and non-404 HTTP read errors) up to 2 times with 250ms and 1s backoff
//     delays while respecting ctx.Done(). Permanent errors (ErrNodeUnknown,
//     ErrNotFound) are never retried.
func (r *Resolver) CaptureTail(
	ctx context.Context,
	nodeID, frameworkID, taskID, containerID, file string,
	maxBytes int64,
) (TailResult, error) {
	if maxBytes <= 0 {
		return TailResult{}, fmt.Errorf("sandbox: CaptureTail maxBytes must be > 0, got %d", maxBytes)
	}

	var lastErr error
	for attempt := 0; attempt <= len(defaultTailRetryDelays); attempt++ {
		if attempt > 0 {
			delay := defaultTailRetryDelays[attempt-1]
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return TailResult{}, lastErr
			case <-timer.C:
			}
		}

		res, err := r.captureTailOnce(ctx, nodeID, frameworkID, taskID, containerID, file, maxBytes)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isRetryableTailError(ctx, err) {
			return TailResult{}, err
		}
	}
	return TailResult{}, lastErr
}

func (r *Resolver) captureTailOnce(
	ctx context.Context,
	nodeID, frameworkID, taskID, containerID, file string,
	maxBytes int64,
) (TailResult, error) {
	node, err := r.Locate(ctx, nodeID)
	if err != nil {
		return TailResult{}, err
	}
	p := node.PathFor(frameworkID, taskID, containerID, file)
	end, err := r.Read(ctx, node, p, -1, 0)
	if err != nil {
		return TailResult{}, err
	}
	size := end.Offset
	if size <= 0 {
		return TailResult{Data: "", Size: 0, Truncated: false}, nil
	}
	start := size - maxBytes
	truncated := false
	if start > 0 {
		truncated = true
	} else {
		start = 0
	}

	curOffset := start
	if truncated {
		aligned, err := r.alignStartOffset(ctx, node, p, start, size)
		if err != nil {
			return TailResult{}, err
		}
		curOffset = aligned
	}

	var buf strings.Builder
	for curOffset < size {
		slice, err := r.Read(ctx, node, p, curOffset, size-curOffset)
		if err != nil {
			return TailResult{}, err
		}
		if len(slice.Data) == 0 {
			break
		}
		chunk := slice.Data
		// Check if a multi-byte UTF-8 rune was split at the end of an interior
		// chunk by the node's per-request read limit (causing the trailing 1..3
		// incomplete bytes of that rune to be decoded as U+FFFD).
		if strings.HasSuffix(chunk, "\uFFFD") {
			trailingFFFD := 0
			for trailingFFFD < utf8.UTFMax-1 &&
				len(chunk) >= (trailingFFFD+1)*3 &&
				strings.HasSuffix(chunk[:len(chunk)-trailingFFFD*3], "\uFFFD") {
				trailingFFFD++
			}
			prefixLen := len(chunk) - trailingFFFD*3
			if prefixLen > 0 && !strings.ContainsRune(chunk[:prefixLen], '\uFFFD') {
				splitPos := curOffset + int64(prefixLen)
				if splitPos < size {
					probeMax := int64(utf8.UTFMax)
					if size-splitPos < probeMax {
						probeMax = size - splitPos
					}
					if probe, perr := r.Read(ctx, node, p, splitPos, probeMax); perr == nil {
						pr, pw := utf8.DecodeRuneInString(probe.Data)
						if pr != utf8.RuneError && pw > trailingFFFD {
							buf.WriteString(chunk[:prefixLen])
							curOffset = splitPos
							continue
						}
					}
				}
			}
		}

		buf.WriteString(chunk)
		curOffset += int64(len(chunk))
	}

	return TailResult{
		Data:      TrimUTF8Tail(buf.String(), truncated),
		Size:      size,
		Truncated: truncated,
	}, nil
}

func isRetryableTailError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, ErrNodeUnknown) || errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if errors.Is(err, ErrControllerUnreachable) || errors.Is(err, ErrNodeUnreachable) {
		return true
	}
	// Non-404 HTTP status or response decode failure from node/controller.
	return strings.Contains(err.Error(), "returned ") || strings.Contains(err.Error(), "decode ")
}
