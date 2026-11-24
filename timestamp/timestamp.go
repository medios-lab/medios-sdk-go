// Package timestamp provides an ordered, UTC-normalized time wrapper.
package timestamp

import (
	"encoding/json"
	"fmt"
	"time"
)

// Timestamp is an ordered, UTC-normalized wrapper around time.Time.
//
// Normalized to UTC with the monotonic clock stripped, so two Timestamps
// constructed from the same epoch instant compare equal regardless of source
// timezone.
//
// Do NOT compare Timestamps with ==. Use Equal, Before, After, Compare.
type Timestamp struct {
	t time.Time
}

// NewTimestamp returns a Timestamp at the given time, normalized to UTC
// and truncated to millisecond precision (matches the wire format we
// emit in MarshalJSON, so in-memory equality survives a JSON roundtrip).
//
// Round(0) also strips the monotonic clock reading, without it, two
// Timestamps for the same instant constructed via different paths (Now
// vs UnixMilli) can fail struct equality.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp{t: t.UTC().Round(0).Truncate(time.Millisecond)}
}

// TimestampMillis returns a Timestamp at the given epoch-millis offset.
func TimestampMillis(ms int64) Timestamp {
	return NewTimestamp(time.UnixMilli(ms))
}

// TimestampSeconds returns a Timestamp at the given epoch-seconds offset.
// Fractional seconds are truncated; controller timestamps arrive in seconds.
func TimestampSeconds(s float64) Timestamp {
	return TimestampMillis(int64(s) * 1000)
}

// ParseTimestamp parses an ISO-8601 / RFC-3339 datetime string.
// Returns an error for malformed input.
func ParseTimestamp(s string) (Timestamp, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return Timestamp{}, fmt.Errorf("invalid timestamp %q: %w", s, err)
	}
	return NewTimestamp(t), nil
}

// Now returns the current time as a Timestamp.
func Now() Timestamp { return NewTimestamp(time.Now()) }

// ZeroTimestamp returns the timestamp at the Unix epoch.
func ZeroTimestamp() Timestamp { return TimestampMillis(0) }

// Millis returns epoch-millis.
func (ts Timestamp) Millis() int64 { return ts.t.UnixMilli() }

// Time returns the underlying time.Time (always UTC, no monotonic reading).
func (ts Timestamp) Time() time.Time { return ts.t }

// Compare returns -1, 0, or +1 if ts is before, equal to, or after other.
func (ts Timestamp) Compare(other Timestamp) int { return ts.t.Compare(other.t) }

// Before reports whether ts strictly precedes other.
func (ts Timestamp) Before(other Timestamp) bool { return ts.t.Before(other.t) }

// After reports whether ts strictly follows other.
func (ts Timestamp) After(other Timestamp) bool { return ts.t.After(other.t) }

// Equal reports whether ts and other represent the same instant.
func (ts Timestamp) Equal(other Timestamp) bool { return ts.t.Equal(other.t) }

// OlderThan is an alias for Before.
func (ts Timestamp) OlderThan(other Timestamp) bool { return ts.Before(other) }

// YoungerThan is an alias for After.
func (ts Timestamp) YoungerThan(other Timestamp) bool { return ts.After(other) }

// Until returns the duration from ts to other (negative if ts > other).
func (ts Timestamp) Until(other Timestamp) time.Duration { return other.t.Sub(ts.t) }

// Expired reports whether the duration from ts to other is at least by.
func (ts Timestamp) Expired(other Timestamp, by time.Duration) bool {
	return ts.Until(other) >= by
}

// String formats as ISO-8601 with milliseconds.
func (ts Timestamp) String() string {
	return ts.t.Format("2006-01-02T15:04:05.000Z")
}

// IsZero reports whether ts is the zero value (unset). Used by the
// `omitzero` JSON tag option to skip never-populated timestamps like
// Task.StartedAt before TASK_RUNNING.
func (ts Timestamp) IsZero() bool { return ts.t.IsZero() }

// MarshalJSON emits the ISO-8601 form, matching String().
func (ts Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + ts.String() + `"`), nil
}

// UnmarshalJSON accepts either the format MarshalJSON emits, or full
// RFC3339 with sub-millisecond precision (so we tolerate timestamps
// stored before this method existed).
func (ts *Timestamp) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*ts = Timestamp{}
		return nil
	}
	parsed, err := ParseTimestamp(s)
	if err != nil {
		// Fall back to the canonical String() format with millis precision.
		t, err2 := time.Parse("2006-01-02T15:04:05.000Z", s)
		if err2 != nil {
			return fmt.Errorf("invalid timestamp %q: %w", s, err)
		}
		*ts = NewTimestamp(t)
		return nil
	}
	*ts = parsed
	return nil
}
