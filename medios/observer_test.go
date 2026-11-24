package medios

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire shape, field name by field name. A rename on the medios side has
// to fail here rather than at a node.
func TestTaskObserver_DecodeWireShape(t *testing.T) {
	const raw = `{
		"name": "shard-7",
		"task_id": {"value": "T-7"},
		"node_id": {"value": "N-1"},
		"principal": "team:research",
		"resources": [],
		"command": {"value": "train.sh"},
		"observers": [
			{"name": "haiku", "command": {"shell": true, "value": "medios-observer-haiku --once"}},
			{"name": "gpu-watch", "command": {"shell": false, "value": "/usr/bin/gpu-watch"}}
		]
	}`

	var task TaskInfo
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(task.Observers) != 2 {
		t.Fatalf("observers: %+v", task.Observers)
	}
	if task.Observers[0].Name != "haiku" {
		t.Errorf("observers[0].name: %q", task.Observers[0].Name)
	}
	if got := task.Observers[0].Command; got == nil || !got.Shell ||
		got.Value != "medios-observer-haiku --once" {
		t.Errorf("observers[0].command: %+v", got)
	}
	if task.Observers[1].Name != "gpu-watch" {
		t.Errorf("observers[1].name: %q", task.Observers[1].Name)
	}
	if got := task.Observers[1].Command; got == nil || got.Shell ||
		got.Value != "/usr/bin/gpu-watch" {
		t.Errorf("observers[1].command: %+v", got)
	}
}

func TestTaskObserver_RoundTrip(t *testing.T) {
	task := TaskInfo{
		Name:      "train-0",
		TaskID:    ID{Value: "train-0"},
		NodeID:    ID{Value: "N-1"},
		Principal: "team:research",
		Command:   CommandInfo{Shell: true, Value: "train.sh"},
	}
	if err := task.WithObserver("haiku", CommandInfo{
		Shell: true, Value: "medios-observer-haiku --credential-file /etc/keys/anthropic",
	}); err != nil {
		t.Fatalf("WithObserver(haiku): %v", err)
	}
	if err := task.WithObserver("gpu-watch", CommandInfo{
		Value: "/usr/bin/gpu-watch",
	}); err != nil {
		t.Fatalf("WithObserver(gpu-watch): %v", err)
	}

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"observers":[`) {
		t.Fatalf("encoded under another key: %s", encoded)
	}

	var got TaskInfo
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Observers) != len(task.Observers) {
		t.Fatalf("observers: %+v", got.Observers)
	}
	for i, want := range task.Observers {
		if got.Observers[i].Name != want.Name {
			t.Errorf("observers[%d].name: %q, want %q", i, got.Observers[i].Name, want.Name)
		}
		if got.Observers[i].Command == nil {
			t.Fatalf("observers[%d].command: nil", i)
		}
		if got.Observers[i].Command.Value != want.Command.Value ||
			got.Observers[i].Command.Shell != want.Command.Shell {
			t.Errorf("observers[%d].command: %+v, want %+v",
				i, *got.Observers[i].Command, *want.Command)
		}
	}
	if err := ValidateTaskObservers(got.Observers); err != nil {
		t.Errorf("ValidateTaskObservers after round trip: %v", err)
	}
}

// A task with no observers encodes without the key, so a controller that
// predates the field sees the shape it saw before.
func TestTaskObserver_OmittedWhenUnobserved(t *testing.T) {
	task := TaskInfo{Name: "train-0", Command: CommandInfo{Value: "train.sh"}}

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "observers") {
		t.Errorf("unobserved task encodes observers: %s", encoded)
	}
	if err := ValidateTaskObservers(task.Observers); err != nil {
		t.Errorf("ValidateTaskObservers(nil): %v", err)
	}
}

func TestWithObserver_RefusesBadName(t *testing.T) {
	// The controller's wording, one refusal per shape: a missing name, a
	// name over the byte limit, and a name outside the charset are three
	// different sentences, and it checks the length before the charset.
	cases := []struct {
		name  string
		named string
		want  string
	}{
		{"empty", "", "Invalid observers: 'name' is required on each observer"},
		{"uppercase", "Haiku", "Invalid observers: 'name' 'Haiku' is outside [a-z0-9-]"},
		{"underscore", "gpu_watch", "Invalid observers: 'name' 'gpu_watch' is outside [a-z0-9-]"},
		{"dot", "gpu.watch", "Invalid observers: 'name' 'gpu.watch' is outside [a-z0-9-]"},
		{"space", "gpu watch", "Invalid observers: 'name' 'gpu watch' is outside [a-z0-9-]"},
		{"slash", "a/b", "Invalid observers: 'name' 'a/b' is outside [a-z0-9-]"},
		{
			"too long",
			strings.Repeat("a", 33),
			"Invalid observers: 'name' '" + strings.Repeat("a", 33) +
				"' is 33 bytes; the limit is 32",
		},
		{
			// Bytes, not runes, as the controller counts them: thirteen
			// runes are 37 bytes, so the length refusal fires rather than
			// the charset one.
			"multibyte over the byte limit",
			strings.Repeat("\u00e9\u00e9", 1) + strings.Repeat("\u4e16", 11),
			"' is 37 bytes; the limit is 32",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := TaskInfo{Name: "train-0"}
			err := task.WithObserver(c.named, CommandInfo{Value: "watch"})
			if err == nil {
				t.Fatalf("WithObserver(%q) accepted", c.named)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error:\n got %q\nwant it to contain %q", err, c.want)
			}
			if len(task.Observers) != 0 {
				t.Errorf("refused observer was appended: %+v", task.Observers)
			}
		})
	}

	// The longest legal name is accepted, so the bound is 32 and not 31.
	task := TaskInfo{Name: "train-0"}
	if err := task.WithObserver(strings.Repeat("a", 32), CommandInfo{Value: "watch"}); err != nil {
		t.Errorf("WithObserver(32 bytes): %v", err)
	}
}

func TestWithObserver_RefusesDuplicateName(t *testing.T) {
	task := TaskInfo{Name: "train-0"}
	if err := task.WithObserver("haiku", CommandInfo{Value: "watch"}); err != nil {
		t.Fatalf("first WithObserver: %v", err)
	}

	err := task.WithObserver("haiku", CommandInfo{Value: "other-watch"})
	if err == nil {
		t.Fatal("duplicate name accepted")
	}
	const want = "Invalid observers: 'name' 'haiku' is used by more than one observer " +
		"on this task, so their lines could not be told apart"
	if err.Error() != want {
		t.Errorf("error:\n got %q\nwant %q", err, want)
	}
	if len(task.Observers) != 1 {
		t.Errorf("duplicate was appended: %+v", task.Observers)
	}
}

func TestWithObserver_RefusesNinth(t *testing.T) {
	task := TaskInfo{Name: "train-0"}
	for _, name := range []string{"o-1", "o-2", "o-3", "o-4", "o-5", "o-6", "o-7", "o-8"} {
		if err := task.WithObserver(name, CommandInfo{Value: "watch"}); err != nil {
			t.Fatalf("WithObserver(%s): %v", name, err)
		}
	}

	err := task.WithObserver("o-9", CommandInfo{Value: "watch"})
	if err == nil {
		t.Fatal("ninth observer accepted")
	}
	const want = "Invalid observers: the task carries 9 observers; the limit is 8"
	if err.Error() != want {
		t.Errorf("error:\n got %q\nwant %q", err, want)
	}
	if len(task.Observers) != 8 {
		t.Errorf("observers: %d", len(task.Observers))
	}
}

func TestValidateTaskObservers_RefusesMissingCommand(t *testing.T) {
	cases := map[string][]TaskObserver{
		"nil command":   {{Name: "haiku"}},
		"empty command": {{Name: "haiku", Command: &CommandInfo{}}},
	}

	for name, observers := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateTaskObservers(observers)
			if err == nil {
				t.Fatal("accepted an observer with no command")
			}
			const want = "Invalid observers: 'command' is required on observer 'haiku'"
			if err.Error() != want {
				t.Errorf("error:\n got %q\nwant %q", err, want)
			}
		})
	}
}

// The flag's unit is whole seconds. A zero interval omits the flag so the
// binary's own default applies, and a sub-second one is sent as one second
// rather than as `--every 0`, which would ask for an unbounded loop.
// DR-041: a profile expands into progress settings and no observer, because
// an observer costs tokens the job author did not ask for.
func TestProgressPolicyForProfile_AddsNoObserver(t *testing.T) {
	for _, profile := range []Profile{ProfileTraining, ProfileService} {
		policy, err := ProgressPolicyForProfile(profile)
		if err != nil {
			t.Fatalf("ProgressPolicyForProfile(%q): %v", profile, err)
		}

		task := TaskInfo{Name: "train-0", ProgressPolicy: policy}
		if len(task.Observers) != 0 {
			t.Errorf("profile %q left observers on the task: %+v", profile, task.Observers)
		}
	}
}
