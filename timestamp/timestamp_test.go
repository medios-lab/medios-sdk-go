package timestamp

import (
	"testing"
	"time"
)

func TestTimestamp_Ordering_HasCorrectOrder(t *testing.T) {
	t1 := TimestampMillis(1024)
	t2 := TimestampMillis(2048)
	if c := t1.Compare(t2); c >= 0 {
		t.Fatalf("t1.Compare(t2): want < 0, got %d", c)
	}
}

func TestTimestamp_Ordering_Before(t *testing.T) {
	t1 := TimestampMillis(1024)
	t2 := TimestampMillis(2048)
	if !t1.Before(t2) {
		t.Fatal("t1.Before(t2): want true")
	}
}

func TestTimestamp_Ordering_After(t *testing.T) {
	t1 := TimestampMillis(1024)
	t2 := TimestampMillis(2048)
	if !t2.After(t1) {
		t.Fatal("t2.After(t1): want true")
	}
}

func TestTimestamp_Ordering_Older(t *testing.T) {
	t1 := TimestampMillis(1024)
	t2 := TimestampMillis(2048)
	if !t1.OlderThan(t2) {
		t.Fatal("t1.OlderThan(t2): want true")
	}
}

func TestTimestamp_Ordering_Younger(t *testing.T) {
	t1 := TimestampMillis(1024)
	t2 := TimestampMillis(2048)
	if !t2.YoungerThan(t1) {
		t.Fatal("t2.YoungerThan(t1): want true")
	}
}

func TestTimestamp_Ordering_Expired(t *testing.T) {
	t1 := TimestampMillis(1024)
	t2 := TimestampMillis(2048)
	// gap between t1 and t2 is 1024 ms ~= 1 second
	if t1.Expired(t2, 100*time.Minute) {
		t.Errorf("t1.Expired(t2, 100min): want false, got true")
	}
	if !t1.Expired(t2, 10*time.Millisecond) {
		t.Errorf("t1.Expired(t2, 10ms): want true, got false")
	}
}

// Two timestamps for the same instant must be equal regardless of the source
// timezone, and must be map-key equal.
func TestTimestamp_Ordering_IndependentOfTimezone(t *testing.T) {
	t1 := TimestampMillis(1024)
	plus2 := time.FixedZone("UTC+2", 2*60*60)
	t2 := NewTimestamp(time.UnixMilli(1024).In(plus2))

	if !t1.Equal(t2) {
		t.Fatalf("Equal: want true; t1=%v t2=%v", t1, t2)
	}
	m := map[Timestamp]int{t1: 1}
	if m[t2] != 1 {
		t.Fatalf("t1 and t2 must be map-key equal")
	}
}

func TestTimestamp_Ordering_FailForIncorrectString(t *testing.T) {
	_, err := ParseTimestamp("20:39:32.972Z")
	if err == nil {
		t.Fatal("ParseTimestamp(time-only): want error, got nil")
	}
}

// A controller timestamp arrives as a float64 of seconds since epoch; convert
// it to millis, truncated to second resolution.
func TestTimestamp_FromTaskStatus_ResolvesCorrectly(t *testing.T) {
	now := time.Now()
	taskStatusSecs := float64(now.Unix())
	ts := TimestampSeconds(taskStatusSecs)
	want := now.Truncate(time.Second).UnixMilli()
	if got := ts.Millis(); got != want {
		t.Fatalf("TimestampSeconds.Millis: want %d, got %d", want, got)
	}
}
