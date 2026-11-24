package medios

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func finishTask(t *testing.T) TaskInfo {
	t.Helper()
	ti, err := NewTaskInfo("team:research", "train", ID{Value: "train-1"}, ID{Value: "S0"}, nil,
		CommandInfo{Shell: true, Value: "python train.py"})
	if err != nil {
		t.Fatal(err)
	}
	return ti
}

// The wire shape, field name by field name, from DR-042: `finish` on
// TaskInfo with `command` and `cap`. A rename on the medios side fails here
// rather than at a node.
func TestWithFinish_WireShape(t *testing.T) {
	ti := finishTask(t)
	ti.MaxCompletionTime = NewDurationInfo(time.Hour)
	if err := ti.WithFinish(CommandInfo{Shell: true, Value: "medios-outputs publish /out/ns/job/a1 manifest.json"}, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	got := string(mustMarshal(t, ti))
	mustContain(t, got, `"max_completion_time":{"nanoseconds":3600000000000}`)
	mustContain(t, got, `"finish":{"command":{"shell":true,"value":"medios-outputs publish /out/ns/job/a1 manifest.json"},"cap":{"nanoseconds":120000000000}}`)

	var back TaskInfo
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatal(err)
	}
	if back.Finish == nil || back.Finish.Cap.Duration() != 2*time.Minute || back.Finish.Command.Value == "" {
		t.Fatalf("round trip: %+v", back.Finish)
	}
	if err := ValidateTaskFinish(&back); err != nil {
		t.Fatal(err)
	}
}

func TestTaskInfo_OmitsAbsentFinishAndCeiling(t *testing.T) {
	ti := finishTask(t)
	got := string(mustMarshal(t, ti))
	for _, absent := range []string{`"finish"`, `"max_completion_time"`} {
		if strings.Contains(got, absent) {
			t.Fatalf("a task without %s must not encode it: %s", absent, got)
		}
	}
	if err := ValidateTaskFinish(&ti); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTaskFinish(nil); err != nil {
		t.Fatal(err)
	}
}

func TestWithFinish_Refusals(t *testing.T) {
	ti := finishTask(t)
	if err := ti.WithFinish(CommandInfo{}, time.Minute); err == nil || !strings.Contains(err.Error(), "'command.value' is required") {
		t.Errorf("empty command: %v", err)
	}
	for _, cap := range []time.Duration{0, -time.Second} {
		if err := ti.WithFinish(CommandInfo{Value: "publish"}, cap); err == nil || !strings.Contains(err.Error(), "'cap' must be positive") {
			t.Errorf("cap %s: %v", cap, err)
		}
	}
	ti.MaxCompletionTime = NewDurationInfo(10 * time.Minute)
	err := ti.WithFinish(CommandInfo{Value: "publish"}, 11*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "'cap' 11m0s is larger than the task's max_completion_time 10m0s") {
		t.Errorf("cap over ceiling: %v", err)
	}
	if ti.Finish != nil {
		t.Fatal("a refused finish was set")
	}
	if err := ti.WithFinish(CommandInfo{Value: "publish"}, 10*time.Minute); err != nil {
		t.Fatalf("cap equal to the ceiling must be accepted: %v", err)
	}
	if err := ti.WithFinish(CommandInfo{Value: "publish again"}, time.Minute); err == nil || !strings.Contains(err.Error(), "already has a finish command") {
		t.Errorf("second finish: %v", err)
	}
	if ti.Finish.Command.Value != "publish" {
		t.Fatal("the second finish replaced the first")
	}

	// No ceiling on the task: any positive cap is accepted.
	free := finishTask(t)
	if err := free.WithFinish(CommandInfo{Value: "publish"}, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestValidateTaskFinish_Refusals(t *testing.T) {
	cmd := &CommandInfo{Value: "p"}
	cases := []struct {
		name    string
		f       *TaskFinish
		ceiling *DurationInfo
		want    string
	}{
		{"nil command", &TaskFinish{Cap: NewDurationInfo(time.Minute)}, nil, "'command.value' is required"},
		{"empty command", &TaskFinish{Command: &CommandInfo{}, Cap: NewDurationInfo(time.Minute)}, nil, "'command.value' is required"},
		{"nil cap", &TaskFinish{Command: cmd}, nil, "'cap' is required"},
		{"zero cap", &TaskFinish{Command: cmd, Cap: NewDurationInfo(0)}, nil, "'cap' must be positive"},
		{"negative cap", &TaskFinish{Command: cmd, Cap: NewDurationInfo(-time.Second)}, nil, "'cap' must be positive"},
		{"cap over ceiling", &TaskFinish{Command: cmd, Cap: NewDurationInfo(2 * time.Minute)}, NewDurationInfo(time.Minute), "larger than the task's max_completion_time"},
	}
	for _, c := range cases {
		ti := finishTask(t)
		ti.Finish, ti.MaxCompletionTime = c.f, c.ceiling
		if err := ValidateTaskFinish(&ti); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// The finish outcome rides on the terminal status apart from the state,
// with DR-042's field names and enum names.
func TestTaskStatus_DecodesFinishStatus(t *testing.T) {
	cases := []struct {
		raw       string
		state     FinishState
		exit, sig int32
		ran, sigd bool
	}{
		{`{"state":"RAN","exit_code":0,"limit":{"nanoseconds":120000000000},"duration":{"nanoseconds":4000000000}}`, FinishRan, 0, 0, true, false},
		{`{"state":"RAN","exit_code":3}`, FinishRan, 3, 0, true, false},
		{`{"state":"RAN","signal":15}`, FinishRan, 0, 15, false, true},
		{`{"state":"TIMED_OUT","limit":{"nanoseconds":120000000000}}`, FinishTimedOut, 0, 0, false, false},
		{`{"state":"KILLED"}`, FinishKilled, 0, 0, false, false},
		{`{"state":"DID_NOT_START","reason":"the work spent the task's ceiling"}`, FinishDidNotStart, 0, 0, false, false},
		{`{"state":"LOST","reason":"wait status unreadable: EINTR","limit":{"nanoseconds":120000000000},"duration":{"nanoseconds":4000000000}}`, FinishLost, 0, 0, false, false},
	}
	for _, c := range cases {
		var st TaskStatus
		raw := `{"task_id":{"value":"t-1"},"state":"TASK_FAILED","reason":"REASON_COMMAND_EXECUTOR_FAILED","finish":` + c.raw + `}`
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		if st.State != TaskFailed || st.Finish == nil || st.Finish.State != c.state {
			t.Fatalf("%s: decoded %+v", c.raw, st)
		}
		if exit, ran, err := st.Finish.Ran(); ran != c.ran || exit != c.exit || err != nil {
			t.Errorf("%s: Ran() = %d,%v,%v want %d,%v", c.raw, exit, ran, err, c.exit, c.ran)
		}
		if sig, ok, err := st.Finish.Signalled(); ok != c.sigd || sig != c.sig || err != nil {
			t.Errorf("%s: Signalled() = %d,%v,%v want %d,%v", c.raw, sig, ok, err, c.sig, c.sigd)
		}
		if err := ValidateFinishOutcome(st.Finish); err != nil {
			t.Errorf("%s: validate = %v", c.raw, err)
		}
		if err := ValidateFinishOutcome(st.Finish); err != nil {
			t.Errorf("%s: %v", c.raw, err)
		}
	}
	var first TaskStatus
	json.Unmarshal([]byte(`{"task_id":{"value":"t"},"state":"TASK_FINISHED","finish":`+cases[0].raw+`}`), &first)
	if first.Finish.Limit.Duration() != 2*time.Minute || first.Finish.Duration.Duration() != 4*time.Second {
		t.Fatalf("limit/duration: %+v", first.Finish)
	}

	var old TaskStatus
	if err := json.Unmarshal([]byte(`{"task_id":{"value":"t-1"},"state":"TASK_FINISHED"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Finish != nil {
		t.Fatal("a status without finish must decode nil")
	}
	if _, ran, err := old.Finish.Ran(); ran || err != nil {
		t.Fatal("nil must not read as ran")
	}
}

// A malformed finish on the wire does not cost the status: the task id and
// state decode, the finish decodes as sent, and every accessor refuses it
// with the reason. A batch with one bad element yields all.
func TestTaskStatus_DecodesAMalformedFinishAsSentAndTheAccessorsRefuse(t *testing.T) {
	for _, raw := range []string{`{"state":"RAN"}`, `{"state":"RAN","exit_code":0,"signal":9}`, `{"state":"ran","exit_code":0}`, `{"state":"DID_NOT_START"}`, `{"state":"SKIPPED"}`} {
		var st TaskStatus
		if err := json.Unmarshal([]byte(`{"task_id":{"value":"t-9"},"state":"TASK_FINISHED","finish":`+raw+`}`), &st); err != nil {
			t.Fatalf("%s: the status must still decode: %v", raw, err)
		}
		if st.TaskID.Value != "t-9" || st.State != TaskFinished || st.Finish == nil {
			t.Fatalf("%s: status lost: %+v", raw, st)
		}
		if ValidateFinishOutcome(st.Finish) == nil {
			t.Errorf("%s: validator accepted", raw)
		}
		if _, ran, err := st.Finish.Ran(); ran || !errors.Is(err, ErrFinishMalformed) {
			t.Errorf("%s: Ran() = %v,%v", raw, ran, err)
		}
		if _, ok, err := st.Finish.Signalled(); ok || !errors.Is(err, ErrFinishMalformed) {
			t.Errorf("%s: Signalled() = %v,%v", raw, ok, err)
		}
	}
	var batch []TaskStatus
	raw := `[{"task_id":{"value":"a"},"state":"TASK_FINISHED","finish":{"state":"RAN","exit_code":0}},` +
		`{"task_id":{"value":"b"},"state":"TASK_FAILED","finish":{"state":"SKIPPED"}},` +
		`{"task_id":{"value":"c"},"state":"TASK_FINISHED","finish":{"state":"TIMED_OUT"}}]`
	if err := json.Unmarshal([]byte(raw), &batch); err != nil || len(batch) != 3 {
		t.Fatalf("batch: %v %d", err, len(batch))
	}
	if ValidateFinishOutcome(batch[0].Finish) != nil || ValidateFinishOutcome(batch[1].Finish) == nil || ValidateFinishOutcome(batch[2].Finish) != nil {
		t.Fatalf("batch: %v %v %v", batch[0].Finish, batch[1].Finish, batch[2].Finish)
	}
	if batch[1].Finish.State != "SKIPPED" {
		t.Fatalf("the unknown state must decode as sent: %+v", batch[1].Finish)
	}
}

// Ran and Signalled refuse every hand-built record the validator refuses:
// RAN with neither, RAN with both.
func TestFinishStatus_AccessorsAgreeWithTheValidator(t *testing.T) {
	zero, sig := int32(0), int32(15)
	for _, f := range []*FinishStatus{
		{State: FinishRan},
		{State: FinishRan, ExitCode: &zero, Signal: &sig},
		{State: FinishDidNotStart},
		{State: "SKIPPED"},
	} {
		if ValidateFinishOutcome(f) == nil {
			t.Fatalf("%+v: validator accepted", f)
		}
		if _, ran, err := f.Ran(); ran || !errors.Is(err, ErrFinishMalformed) {
			t.Errorf("%+v: Ran() = %v,%v", f, ran, err)
		}
		if _, ok, err := f.Signalled(); ok || !errors.Is(err, ErrFinishMalformed) {
			t.Errorf("%+v: Signalled() = %v,%v", f, ok, err)
		}
	}
	if _, ran, err := (&FinishStatus{State: FinishRan, Signal: &sig}).Ran(); ran || err != nil {
		t.Fatalf("signalled: Ran() = %v,%v", ran, err)
	}
	if _, ran, err := (&FinishStatus{State: FinishTimedOut}).Ran(); ran || err != nil {
		t.Fatalf("timed out: Ran() = %v,%v", ran, err)
	}
}

// WithFinish keeps a snapshot of the command: the caller's later edits do
// not reach the task.
func TestWithFinish_DeepCopiesTheCommand(t *testing.T) {
	ti := finishTask(t)
	extract := true
	cmd := CommandInfo{Shell: true, Value: "publish",
		Environment: &Environment{Variables: []EnvVar{{Name: "A", Value: "1"}}},
		URIs:        []URI{{Value: "http://x/y", Extract: &extract}, {Value: "http://x/z"}}}
	if err := ti.WithFinish(cmd, time.Minute); err != nil {
		t.Fatal(err)
	}
	cmd.Environment.Variables[0].Value = "changed"
	cmd.Environment.Variables = append(cmd.Environment.Variables, EnvVar{Name: "B", Value: "2"})
	cmd.URIs[0].Value = "http://changed"
	*cmd.URIs[0].Extract = false
	cmd.Value = "changed"
	got := ti.Finish.Command
	if got.Value != "publish" || len(got.Environment.Variables) != 1 || got.Environment.Variables[0].Value != "1" || got.URIs[0].Value != "http://x/y" {
		t.Fatalf("the task's finish command followed the caller's edits: %+v", got)
	}
	if got.URIs[0].Extract == nil || *got.URIs[0].Extract != true {
		t.Fatalf("Extract followed the caller's edit: %+v", got.URIs[0])
	}
	if got.URIs[1].Extract != nil {
		t.Fatalf("a nil Extract must stay nil: %+v", got.URIs[1])
	}
}

func TestValidateFinishOutcome_Refusals(t *testing.T) {
	zero, sig := int32(0), int32(9)
	cases := []struct {
		name string
		f    *FinishStatus
		want string
	}{
		{"no state", &FinishStatus{}, "has no state"},
		{"unknown state", &FinishStatus{State: "SKIPPED"}, `state "SKIPPED" is unknown`},
		{"lower-case state", &FinishStatus{State: "ran", ExitCode: &zero}, `state "ran" is unknown`},
		{"ran with neither", &FinishStatus{State: FinishRan}, "exactly one of exit_code and signal"},
		{"ran with both", &FinishStatus{State: FinishRan, ExitCode: &zero, Signal: &sig}, "exactly one of exit_code and signal"},
		{"did_not_start without reason", &FinishStatus{State: FinishDidNotStart}, "carries no reason"},
		{"lost without reason", &FinishStatus{State: FinishLost}, "LOST carries no reason"},
		{"lost with exit code", &FinishStatus{State: FinishLost, Reason: "unreadable", ExitCode: &zero}, "LOST carries an exit_code or signal"},
		{"did_not_start with signal", &FinishStatus{State: FinishDidNotStart, Reason: "ceiling", Signal: &sig}, "DID_NOT_START carries an exit_code or signal"},
	}
	for _, c := range cases {
		if err := ValidateFinishOutcome(c.f); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if err := ValidateFinishOutcome(&FinishStatus{State: FinishRan, ExitCode: &zero}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFinishOutcome(&FinishStatus{State: FinishRan, Signal: &sig}); err != nil {
		t.Fatal(err)
	}
	lost := &FinishStatus{State: FinishLost, Reason: "unreadable"}
	if err := ValidateFinishOutcome(lost); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := lost.Ran(); ran || err != nil {
		t.Fatalf("LOST: Ran() = %v,%v", ran, err)
	}
	if _, ok, err := lost.Signalled(); ok || err != nil {
		t.Fatalf("LOST: Signalled() = %v,%v", ok, err)
	}
	if err := ValidateFinishOutcome(nil); err != nil {
		t.Fatal(err)
	}
}
