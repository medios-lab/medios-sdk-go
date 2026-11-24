package medios

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskStatus_Progress_RoundTrip(t *testing.T) {
	const raw = `{
		"task_id":  {"value": "shard-0"},
		"state":    "TASK_RUNNING",
		"node_id":  {"value": "node-1"},
		"progress": {
			"fraction":    0.42,
			"step":        42,
			"total":       100,
			"message":     "epoch 42/100",
			"sequence":    7,
			"observed_at": 1760000000.25
		}
	}`
	var st TaskStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Progress == nil {
		t.Fatal("progress: nil")
	}
	if st.Progress.Fraction == nil || *st.Progress.Fraction != 0.42 {
		t.Errorf("fraction: %v", st.Progress.Fraction)
	}
	if st.Progress.Step == nil || *st.Progress.Step != 42 {
		t.Errorf("step: %v", st.Progress.Step)
	}
	if st.Progress.Total == nil || *st.Progress.Total != 100 {
		t.Errorf("total: %v", st.Progress.Total)
	}
	if st.Progress.Message != "epoch 42/100" {
		t.Errorf("message: %q", st.Progress.Message)
	}
	if st.Progress.Sequence != 7 {
		t.Errorf("sequence: %d", st.Progress.Sequence)
	}
	if st.Progress.ObservedAt == nil || *st.Progress.ObservedAt != 1760000000.25 {
		t.Errorf("observed_at: %v", st.Progress.ObservedAt)
	}

	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again TaskStatus
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if *again.Progress.Fraction != 0.42 || again.Progress.Sequence != 7 {
		t.Errorf("progress did not survive the round trip: %s", b)
	}
	if again.Progress.ObservedAt == nil || *again.Progress.ObservedAt != 1760000000.25 {
		t.Errorf("observed_at did not survive the round trip: %s", b)
	}
}

func TestProgress_ObservedAtIsDistinctFromTheStatusTimestamp(t *testing.T) {
	// The two answer different questions. Timestamp is when the status was
	// made, which a reconciliation reply sets afresh; observed_at is when
	// the value was read off the job, which the controller never rewrites.
	// A reader comparing them learns the age of the observation.
	const raw = `{
		"task_id":   {"value": "shard-0"},
		"state":     "TASK_RUNNING",
		"node_id":   {"value": "node-1"},
		"timestamp": 1760003600,
		"reason":    "REASON_RECONCILIATION",
		"progress":  {"fraction": 0.31, "sequence": 12, "observed_at": 1760000000}
	}`
	var st TaskStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Timestamp != 1760003600 {
		t.Errorf("timestamp: %v", st.Timestamp)
	}
	if st.Progress.ObservedAt == nil || *st.Progress.ObservedAt != 1760000000 {
		t.Fatalf("observed_at: %v", st.Progress.ObservedAt)
	}
	if age := st.Timestamp - *st.Progress.ObservedAt; age != 3600 {
		t.Errorf("the observation is %v seconds old, want 3600", age)
	}
}

func TestProgress_AbsentObservedAtStaysAbsent(t *testing.T) {
	// Nil covers a reporter that set none and every controller that predates
	// the field. Zero would be 1970, which is a claim about when the value
	// was read.
	b, err := json.Marshal(&Progress{Message: "loading", Sequence: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "observed_at") {
		t.Errorf("an unreported observed_at should be absent: %s", b)
	}

	var p Progress
	if err := json.Unmarshal([]byte(`{"sequence":1}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ObservedAt != nil {
		t.Errorf("observed_at: %v, want nil", p.ObservedAt)
	}
}

func TestTaskStatus_NoProgress_IsNilNotZero(t *testing.T) {
	// A controller that predates the field, or a task that reported nothing,
	// must read as unknown. Zero would mean "has not started" and complete
	// would mean the opposite; both are claims the task did not make.
	const raw = `{"task_id":{"value":"t-1"},"state":"TASK_RUNNING","node_id":{"value":"n-1"}}`
	var st TaskStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Progress != nil {
		t.Errorf("progress should be nil when the status carried none: %+v", st.Progress)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "progress") {
		t.Errorf("no progress reported, so no `progress` key: %s", b)
	}
}

func TestProgress_AbsentFieldsStayAbsent(t *testing.T) {
	// A job reporting only a message and a sequence must not have a
	// fraction, a step or a total invented for it.
	b, err := json.Marshal(&Progress{Message: "loading shards", Sequence: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, absent := range []string{"fraction", "step", "total"} {
		if strings.Contains(string(b), absent) {
			t.Errorf("unreported %s should be absent: %s", absent, b)
		}
	}
	// Sequence is always present: a reader needs it to order reports.
	if !strings.Contains(string(b), `"sequence":1`) {
		t.Errorf("sequence must always encode: %s", b)
	}
}

func TestProgress_ZeroFractionIsReportable(t *testing.T) {
	// A task that reports 0 has said something, and that differs from
	// saying nothing. omitempty on a *float64 keeps the two apart.
	zero := 0.0
	b, err := json.Marshal(&Progress{Fraction: &zero, Sequence: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"fraction":0`) {
		t.Errorf("a reported zero must survive: %s", b)
	}
}

func TestStalledStatus_DecodesWithItsReason(t *testing.T) {
	const raw = `{
		"task_id":   {"value": "shard-7"},
		"state":     "TASK_FAILED",
		"node_id":   {"value": "node-1"},
		"source":    "SOURCE_EXECUTOR",
		"reason":    "REASON_PROGRESS_STALLED",
		"message":   "no progress for 912s; last reported fraction 0.31 step 31",
		"progress":  {"fraction": 0.31, "step": 31, "total": 100, "sequence": 12,
		              "observed_at": 1760000000}
	}`
	var st TaskStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Reason != ReasonProgressStalled {
		t.Errorf("reason: %q, want %q", st.Reason, ReasonProgressStalled)
	}
	// The last value seen rides along with the kill, so a reader can tell a
	// wedge from a crash without opening a log.
	if st.Progress == nil || st.Progress.Fraction == nil || *st.Progress.Fraction != 0.31 {
		t.Errorf("the last progress value should accompany the stall kill: %+v", st.Progress)
	}
	if st.Progress.ObservedAt == nil {
		t.Error("a stall kill should say when the executor observed the stall")
	}
}

func progressAt(seq uint64, fraction float64) *Progress {
	return &Progress{Fraction: &fraction, Sequence: seq}
}

func TestFold_NothingHeldAndNothingReported(t *testing.T) {
	st := TaskStatus{State: TaskRunning}
	var held *Progress
	if got := held.Fold(&st); got != nil {
		t.Errorf("got %+v, want nil: neither side said anything", got)
	}
}

func TestFold_NothingHeldTakesTheFirstReport(t *testing.T) {
	// Sequence 0 is a real first report, not an absent one.
	st := TaskStatus{State: TaskRunning, Progress: progressAt(0, 0.0)}
	var held *Progress
	got := held.Fold(&st)
	if got == nil || got.Sequence != 0 || got.Fraction == nil || *got.Fraction != 0.0 {
		t.Fatalf("the first report should be adopted as held: %+v", got)
	}
}

func TestFold_UpdateWithoutProgressKeepsWhatIsHeld(t *testing.T) {
	// A reconciliation reply or a plain state transition carries no progress.
	// Clearing here makes a working task's progress blink out.
	held := progressAt(7, 0.42)
	st := TaskStatus{State: TaskRunning, Reason: "REASON_RECONCILIATION"}
	got := held.Fold(&st)
	if got != held {
		t.Errorf("got %+v, want the held value %+v", got, held)
	}
}

func TestFold_HigherSequenceReplaces(t *testing.T) {
	held := progressAt(7, 0.42)
	st := TaskStatus{State: TaskRunning, Progress: progressAt(8, 0.55)}
	got := held.Fold(&st)
	if got == nil || got.Sequence != 8 || *got.Fraction != 0.55 {
		t.Errorf("a newer report should replace the held one: %+v", got)
	}
}

func TestFold_EqualSequenceIsIgnoredAsAReplay(t *testing.T) {
	// The node retransmits an unacknowledged update, so the same report
	// arriving twice is ordinary. The held value wins.
	held := progressAt(7, 0.42)
	st := TaskStatus{State: TaskRunning, Progress: progressAt(7, 0.99)}
	got := held.Fold(&st)
	if got != held {
		t.Errorf("got %+v, want the held value %+v", got, held)
	}
}

func TestFold_LowerSequenceIsIgnoredAsAReplay(t *testing.T) {
	held := progressAt(7, 0.42)
	st := TaskStatus{State: TaskRunning, Progress: progressAt(3, 0.10)}
	got := held.Fold(&st)
	if got != held {
		t.Errorf("an older report should not move progress backwards: %+v", got)
	}
}

func TestFold_TerminalStatusWithoutProgressKeepsTheLastValue(t *testing.T) {
	// The last value seen is what tells a stall kill from a crash, so it has
	// to survive the terminal status that carries none.
	held := progressAt(12, 0.31)
	st := TaskStatus{State: TaskFailed, Reason: ReasonProgressStalled}
	got := held.Fold(&st)
	if got != held {
		t.Fatalf("got %+v, want the last value %+v", got, held)
	}
	if !st.State.IsTerminal() {
		t.Error("the status under test should be terminal")
	}
}

func TestFold_TerminalStatusCarryingProgressStillApplies(t *testing.T) {
	// A stall kill carries the last value the executor read; it is a report
	// like any other and the sequence decides.
	held := progressAt(11, 0.30)
	st := TaskStatus{State: TaskFailed, Reason: ReasonProgressStalled,
		Progress: progressAt(12, 0.31)}
	got := held.Fold(&st)
	if got == nil || got.Sequence != 12 {
		t.Errorf("got %+v, want the sequence-12 report", got)
	}
}

func TestFold_NilUpdateKeepsWhatIsHeld(t *testing.T) {
	held := progressAt(7, 0.42)
	if got := held.Fold(nil); got != held {
		t.Errorf("got %+v, want the held value %+v", got, held)
	}
}

func TestFold_ReturnsOneOfItsInputs(t *testing.T) {
	// Contract: the result is the receiver or update.Progress, never a new
	// value. A caller holding progress in its own type compares the result
	// against update.Progress to learn which side won, instead of restating
	// the rule.
	held := progressAt(7, 0.42)
	taken := TaskStatus{State: TaskRunning, Progress: progressAt(8, 0.55)}
	if got := held.Fold(&taken); got != taken.Progress {
		t.Errorf("adopting a report should return that report itself: %+v", got)
	}
	replay := TaskStatus{State: TaskRunning, Progress: progressAt(7, 0.99)}
	if got := held.Fold(&replay); got != held {
		t.Errorf("ignoring a replay should return the receiver itself: %+v", got)
	}
}
