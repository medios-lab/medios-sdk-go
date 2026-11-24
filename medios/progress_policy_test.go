package medios

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The wire shape, pinned field name by field name. A rename on the medios
// side has to fail here rather than at a node.
func TestProgressPolicy_DecodeWireShape(t *testing.T) {
	const raw = `{
		"name": "shard-7",
		"task_id": {"value": "T-7"},
		"node_id": {"value": "N-1"},
		"principal": "team:research",
		"resources": [],
		"command": {"value": "train.sh"},
		"progress_policy": {
			"source": "FILE",
			"path": "logs/progress.txt",
			"pattern": "done ([0-9]+) of ([0-9]+)",
			"sample_interval": {"nanoseconds": 500000000},
			"stall_window": {"nanoseconds": 120000000000},
			"action": "KILL"
		}
	}`

	var task TaskInfo
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := task.ProgressPolicy
	if p == nil {
		t.Fatal("progress_policy: nil")
	}
	if p.Source != ProgressSourceFile {
		t.Errorf("source: %q", p.Source)
	}
	if p.Path != "logs/progress.txt" {
		t.Errorf("path: %q", p.Path)
	}
	if p.Pattern != "done ([0-9]+) of ([0-9]+)" {
		t.Errorf("pattern: %q", p.Pattern)
	}
	if got := p.SampleInterval.Duration(); got != 500*time.Millisecond {
		t.Errorf("sample_interval: %v", got)
	}
	if got := p.StallWindow.Duration(); got != 120*time.Second {
		t.Errorf("stall_window: %v", got)
	}
	if p.Action != StallActionKill {
		t.Errorf("action: %q", p.Action)
	}
}

func TestProgressPolicy_RoundTrip(t *testing.T) {
	want := &ProgressPolicy{
		Source:         ProgressSourceFile,
		Path:           "out/progress",
		Pattern:        `progress: ([0-9]+)`,
		SampleInterval: NewDurationInfo(2 * time.Second),
		StallWindow:    NewDurationInfo(90 * time.Second),
		Action:         StallActionReport,
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got ProgressPolicy
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Source != want.Source || got.Path != want.Path || got.Pattern != want.Pattern ||
		got.Action != want.Action {
		t.Errorf("round trip: got %+v", got)
	}
	if got.SampleInterval.Duration() != 2*time.Second {
		t.Errorf("sample_interval: %v", got.SampleInterval.Duration())
	}
	if got.StallWindow.Duration() != 90*time.Second {
		t.Errorf("stall_window: %v", got.StallWindow.Duration())
	}
}

// A task with no policy encodes no key at all, so a controller that predates
// the field sees the TaskInfo it saw before.
func TestTaskInfo_OmitsAbsentProgressPolicy(t *testing.T) {
	task, err := NewTaskInfo("team:research", "t", ID{Value: "T-1"}, ID{Value: "N-1"},
		nil, CommandInfo{Value: "true"})
	if err != nil {
		t.Fatalf("NewTaskInfo: %v", err)
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "progress_policy") {
		t.Errorf("nil policy encoded a key: %s", encoded)
	}
}

// The optional fields drop out individually: a STDOUT policy that arms no
// clock is three keys, not six empty ones.
func TestProgressPolicy_OmitsEmptyFields(t *testing.T) {
	encoded, err := json.Marshal(&ProgressPolicy{Source: ProgressSourceStdout})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `{"source":"STDOUT"}`; got != want {
		t.Errorf("encoded %s, want %s", got, want)
	}
}

func TestDurationInfo_NilReadsZero(t *testing.T) {
	var d *DurationInfo
	if got := d.Duration(); got != 0 {
		t.Errorf("nil Duration: %v", got)
	}
}

func TestValidateProgressPolicy_Accepts(t *testing.T) {
	cases := map[string]*ProgressPolicy{
		"no policy at all": nil,
		"stdout, no clock armed": {
			Source: ProgressSourceStdout,
		},
		"stdout, report on stall": {
			Source:      ProgressSourceStdout,
			StallWindow: NewDurationInfo(time.Minute),
			Action:      StallActionReport,
		},
		"file, kill on stall": {
			Source:         ProgressSourceFile,
			Path:           "a/b/progress.txt",
			SampleInterval: NewDurationInfo(time.Second),
			StallWindow:    NewDurationInfo(2 * time.Minute),
			Action:         StallActionKill,
		},
		"a path whose component merely starts with a dot-dot": {
			Source: ProgressSourceFile,
			Path:   "..data/progress",
		},
	}

	for name, policy := range cases {
		if err := ValidateProgressPolicy(policy); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateProgressPolicy_Refuses(t *testing.T) {
	cases := []struct {
		name   string
		policy *ProgressPolicy
		want   string
	}{
		{
			name:   "no source",
			policy: &ProgressPolicy{},
			want:   "Source is required",
		},
		{
			name:   "an unknown source",
			policy: &ProgressPolicy{Source: "stdout"},
			want:   `Source is "stdout"`,
		},
		{
			name:   "FILE with no path",
			policy: &ProgressPolicy{Source: ProgressSourceFile},
			want:   "Path is required",
		},
		{
			name:   "an absolute path",
			policy: &ProgressPolicy{Source: ProgressSourceFile, Path: "/var/log/p"},
			want:   "relative to the task's sandbox",
		},
		{
			name:   "a path that climbs out of the sandbox",
			policy: &ProgressPolicy{Source: ProgressSourceFile, Path: "a/../../p"},
			want:   "may not leave the task's sandbox",
		},
		{
			name:   "a path on STDOUT",
			policy: &ProgressPolicy{Source: ProgressSourceStdout, Path: "p"},
			want:   `Path is "p" with Source "STDOUT"`,
		},
		{
			name: "a zero sample interval",
			policy: &ProgressPolicy{
				Source:         ProgressSourceStdout,
				SampleInterval: NewDurationInfo(0),
			},
			want: "SampleInterval is 0 ns",
		},
		{
			name: "a negative sample interval",
			policy: &ProgressPolicy{
				Source:         ProgressSourceStdout,
				SampleInterval: NewDurationInfo(-time.Second),
			},
			want: "must be a positive duration",
		},
		{
			name: "a zero stall window",
			policy: &ProgressPolicy{
				Source:      ProgressSourceStdout,
				StallWindow: NewDurationInfo(0),
				Action:      StallActionKill,
			},
			want: "StallWindow is 0 ns",
		},
		{
			name: "an action with no window to act on",
			policy: &ProgressPolicy{
				Source: ProgressSourceStdout,
				Action: StallActionKill,
			},
			want: "without a StallWindow",
		},
		{
			name: "a window with no action",
			policy: &ProgressPolicy{
				Source:      ProgressSourceStdout,
				StallWindow: NewDurationInfo(time.Minute),
			},
			want: "set without an Action",
		},
		{
			name: "an unknown action",
			policy: &ProgressPolicy{
				Source:      ProgressSourceStdout,
				StallWindow: NewDurationInfo(time.Minute),
				Action:      "kill",
			},
			want: `Action is "kill"`,
		},
	}

	for _, c := range cases {
		err := ValidateProgressPolicy(c.policy)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
		if !strings.Contains(err.Error(), "TaskInfo.ProgressPolicy") {
			t.Errorf("%s: error %q does not name the field", c.name, err)
		}
	}
}

func TestProgressPolicyForProfile_Training(t *testing.T) {
	policy, err := ProgressPolicyForProfile(ProfileTraining)
	if err != nil {
		t.Fatalf("training: %v", err)
	}
	if policy == nil {
		t.Fatal("training: nil policy")
	}
	if policy.Source != ProgressSourceStdout {
		t.Errorf("source: %q", policy.Source)
	}
	if policy.Pattern != "" {
		t.Errorf("pattern: %q, want empty so the executor's default applies", policy.Pattern)
	}
	if got := policy.SampleInterval.Duration(); got != time.Second {
		t.Errorf("sample interval: %v", got)
	}
	if got := policy.StallWindow.Duration(); got != 120*time.Second {
		t.Errorf("stall window: %v", got)
	}
	if policy.Action != StallActionKill {
		t.Errorf("action: %q", policy.Action)
	}
	if err := ValidateProgressPolicy(policy); err != nil {
		t.Errorf("the profile's own policy does not validate: %v", err)
	}
}

// The stall window has to be reachable inside the runtime cap the batch jobs
// run under, or the setting looks armed and never fires.
func TestProgressPolicyForProfile_TrainingWindowFitsTheRuntimeCap(t *testing.T) {
	const batchRuntimeCap = 300 * time.Second

	policy, err := ProgressPolicyForProfile(ProfileTraining)
	if err != nil {
		t.Fatalf("training: %v", err)
	}
	if got := policy.StallWindow.Duration(); got >= batchRuntimeCap {
		t.Errorf("stall window %v is not below the %v runtime cap", got, batchRuntimeCap)
	}
}

func TestProgressPolicyForProfile_Service(t *testing.T) {
	policy, err := ProgressPolicyForProfile(ProfileService)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if policy != nil {
		t.Errorf("service: got a policy %+v, want none", policy)
	}
}

func TestProgressPolicyForProfile_UnknownProfile(t *testing.T) {
	policy, err := ProgressPolicyForProfile("batch")
	if err == nil {
		t.Fatal("unknown profile accepted")
	}
	if policy != nil {
		t.Errorf("unknown profile returned a policy: %+v", policy)
	}
	if !strings.Contains(err.Error(), `"batch"`) {
		t.Errorf("error %q does not name the profile", err)
	}
}

// medios discards an enum value it cannot resolve on an optional field and
// its getter then returns the first value in the enum, STDOUT. An empty
// source must therefore be absent on the wire, where the controller refuses
// it by name, rather than encoded as "".
func TestProgressPolicy_OmitsEmptySource(t *testing.T) {
	encoded, err := json.Marshal(&ProgressPolicy{Path: "p"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "source") {
		t.Errorf("an empty source encoded a key: %s", encoded)
	}
}

// The training profile's policy as the controller reads it, pinned whole
// against include/medios/medios.proto on medios node/progress-policy
// (49f65e80b): enum values by name, DurationInfo as whole nanoseconds, no
// pattern so the executor's own convention applies.
func TestProgressPolicyForProfile_TrainingWireShape(t *testing.T) {
	policy, err := ProgressPolicyForProfile(ProfileTraining)
	if err != nil {
		t.Fatalf("training: %v", err)
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"source":"STDOUT",` +
		`"sample_interval":{"nanoseconds":1000000000},` +
		`"stall_window":{"nanoseconds":120000000000},` +
		`"action":"KILL"}`
	if got := string(encoded); got != want {
		t.Errorf("encoded\n  %s\nwant\n  %s", got, want)
	}
}
