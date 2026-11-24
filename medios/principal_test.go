package medios

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestNewTaskInfo_PrincipalPresent(t *testing.T) {
	for _, p := range []string{"operator:alice", "team:batch", "run:research-164-s5-r3", "team:a.b_c-d"} {
		ti, err := NewTaskInfo(p, "t", ID{Value: "t-1"}, ID{Value: "S0"},
			[]Resource{{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 0.1}}},
			CommandInfo{Shell: true, Value: "echo hi"})
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		if ti.Principal != p || ti.Name != "t" || ti.TaskID.Value != "t-1" || ti.NodeID.Value != "S0" {
			t.Fatalf("%q: fields not carried: %+v", p, ti)
		}
		got := string(mustMarshal(t, ti))
		mustContain(t, got, `"principal":"`+p+`"`)
	}
}

func TestNewTaskInfo_PrincipalMissing(t *testing.T) {
	_, err := NewTaskInfo("", "t", ID{Value: "t-1"}, ID{Value: "S0"}, nil, CommandInfo{Shell: true, Value: "x"})
	if !errors.Is(err, ErrPrincipalRequired) {
		t.Fatalf("err = %v, want ErrPrincipalRequired", err)
	}
	if !strings.Contains(err.Error(), "TaskInfo.Principal") {
		t.Fatalf("error does not name the field: %v", err)
	}
}

func TestNewTaskInfo_PrincipalMalformed(t *testing.T) {
	for _, p := range []string{
		"alice",          // no kind
		"user:alice",     // unknown kind
		"TEAM:research",  // kind is case-sensitive
		"team:",          // empty name
		"team:cue eng",   // space in name
		"team:cue/eng",   // slash in name
		":research",      // empty kind
		"team:research:", // trailing separator
	} {
		_, err := NewTaskInfo(p, "t", ID{Value: "t-1"}, ID{Value: "S0"}, nil, CommandInfo{Shell: true, Value: "x"})
		if err == nil {
			t.Errorf("%q: accepted, want malformed error", p)
			continue
		}
		if errors.Is(err, ErrPrincipalRequired) {
			t.Errorf("%q: reported as missing, want malformed", p)
		}
		if !strings.Contains(err.Error(), "TaskInfo.Principal") || !strings.Contains(err.Error(), p) {
			t.Errorf("%q: error does not name the field and value: %v", p, err)
		}
	}
}

// The wire always carries the field, so an ACCEPT built without the
// constructor shows an empty principal rather than none.
func TestEncodeTaskInfo_PrincipalAlwaysPresent(t *testing.T) {
	got := string(mustMarshal(t, TaskInfo{Name: "t", TaskID: ID{Value: "t-1"}}))
	mustContain(t, got, `"principal":""`)
}

// A status update decodes the controller's record of the pair.
func TestTaskStatus_DecodesPrincipalPair(t *testing.T) {
	var st TaskStatus
	raw := `{"task_id":{"value":"t-1"},"state":"TASK_RUNNING","node_id":{"value":"S0"},` +
		`"principal":"team:batch","framework_principal":"times"}`
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if st.Principal != "team:batch" || st.FrameworkPrincipal != "times" {
		t.Fatalf("decoded %+v", st)
	}
	var old TaskStatus
	if err := json.Unmarshal([]byte(`{"task_id":{"value":"t-1"},"state":"TASK_RUNNING"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Principal != "" || old.FrameworkPrincipal != "" {
		t.Fatalf("a status without the fields must decode them empty: %+v", old)
	}
}

// The name after the kind is bounded; the limit is the shared constant so
// the SDK refuses before the wire exactly what the controller refuses at it.
func TestValidatePrincipal_NameLength(t *testing.T) {
	at := "run:" + strings.Repeat("a", PrincipalNameMaxLength)
	if err := ValidatePrincipal(at); err != nil {
		t.Fatalf("%d-character name refused: %v", PrincipalNameMaxLength, err)
	}

	over := "run:" + strings.Repeat("a", PrincipalNameMaxLength+1)
	err := ValidatePrincipal(over)
	if err == nil {
		t.Fatalf("%d-character name accepted", PrincipalNameMaxLength+1)
	}
	if errors.Is(err, ErrPrincipalRequired) {
		t.Fatalf("reported as missing, want over-length: %v", err)
	}
	if !strings.Contains(err.Error(), "TaskInfo.Principal") ||
		!strings.Contains(err.Error(), strconv.Itoa(PrincipalNameMaxLength)) {
		t.Fatalf("error does not name the field and the limit: %v", err)
	}
}
