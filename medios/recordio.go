// Package scheduler is the Medios HTTP v1 scheduler client for the
// medios-sdk-go.
//
// The wire protocol is specified in the Medios HTTP v1 scheduler API:
// medios docs/scheduler-http-api.md and docs/recordio.md. This package
// provides:
//
//   - RecordIO: a streaming parser for the framing format Medios uses on
//     the SUBSCRIBE response.
//   - Types: JSON-tagged Go structs for the wire messages.
//   - Client: the Subscribe / Accept / Decline / Kill / Acknowledge /
//     Teardown call surface, with Medios-Stream-Id header threading.
//
// Wire-protocol requirements that are easy to miss are baked into Client
// defaults, FrameworkInfo.user is required (not blank), absolute paths for
// task commands, container_id captured from the first TASK_RUNNING update.
package medios

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// RecordIO framing per medios docs/recordio.md:
//
//	records      = *record
//	record       = record-size LF record-data
//	record-size  = 1*DIGIT     (ASCII decimal, parsed as uint64)
//	record-data  = record-size bytes (one of: JSON event, protobuf event)
//
// This client only speaks JSON, so each record-data is a JSON object.

// ParseRecordIO reads RecordIO records from r and delivers each record as a
// raw byte slice via the yield callback. yield returns an error to stop
// reading; ParseRecordIO returns that error or any I/O / framing error.
// io.EOF at a record boundary is treated as normal end-of-stream (nil).
//
// The callback's []byte is only valid until the next call; copy if you need it.
func ParseRecordIO(r io.Reader, yield func([]byte) error) error {
	// bufio.Reader gives us ReadString('\n') for the length line and a
	// growable internal buffer for the payload reads. We don't need bufio's
	// peek/scan beyond that.
	br := bufio.NewReader(r)

	var payload []byte // reused across records
	for {
		// Read the size line (ASCII digits terminated by \n).
		line, err := br.ReadString('\n')
		if err == io.EOF && line == "" {
			return nil // clean end at record boundary
		}
		if err != nil {
			return fmt.Errorf("recordio: read size line: %w", err)
		}
		// Strip the trailing LF (and a stray CR if a proxy added one).
		size, parseErr := strconv.ParseUint(trimNewline(line), 10, 64)
		if parseErr != nil {
			return fmt.Errorf("recordio: invalid size line %q: %w", line, parseErr)
		}

		// Grow buffer as needed.
		if uint64(cap(payload)) < size {
			payload = make([]byte, size)
		} else {
			payload = payload[:size]
		}

		if _, err := io.ReadFull(br, payload); err != nil {
			return fmt.Errorf("recordio: read %d-byte payload: %w", size, err)
		}

		if err := yield(payload); err != nil {
			return err
		}
	}
}

// ParseRecordIOJSON is a typed convenience: it parses each record as JSON
// into the value the constructor returns. The constructor is called per
// record so callers can return a fresh value each time (closures over
// shared state are fine but uncommon).
//
// Stops on the first JSON decode error.
func ParseRecordIOJSON[T any](r io.Reader, make func() *T, yield func(*T) error) error {
	return ParseRecordIO(r, func(buf []byte) error {
		v := make()
		if err := json.Unmarshal(buf, v); err != nil {
			return fmt.Errorf("recordio: decode JSON record: %w", err)
		}
		return yield(v)
	})
}

func trimNewline(s string) string {
	n := len(s)
	for n > 0 && (s[n-1] == '\n' || s[n-1] == '\r') {
		n--
	}
	return s[:n]
}
