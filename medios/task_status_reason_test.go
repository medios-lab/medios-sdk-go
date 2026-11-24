package medios

import (
	"encoding/json"
	"testing"
)

// A status update's reason is what makes TASK_LOST interpretable: the same
// state means "gone" or "may still be running" depending on it.
func TestTaskStatus_ReasonDecodes(t *testing.T) {
	const raw = `{"type":"UPDATE","update":{"status":{
	  "task_id":{"value":"t-1"},"node_id":{"value":"n-1"},"state":"TASK_LOST",
	  "reason":"REASON_RECONCILIATION","message":"Reconciliation: Latest task state",
	  "uuid":"abc"}}}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	st := ev.Update.Status
	if st.Reason != "REASON_RECONCILIATION" {
		t.Errorf("reason = %q", st.Reason)
	}
	if st.State != TaskLost {
		t.Errorf("state = %q", st.State)
	}
}

// A reason this library has never heard of survives as its name. Which reasons
// a controller sends is its contract to state, not this library's to enumerate.
func TestTaskStatus_UnknownReasonPassesThrough(t *testing.T) {
	const raw = `{"type":"UPDATE","update":{"status":{
	  "task_id":{"value":"t-1"},"node_id":{"value":"n-1"},"state":"TASK_FAILED",
	  "reason":"REASON_SOMETHING_LATER"}}}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := ev.Update.Status.Reason; got != "REASON_SOMETHING_LATER" {
		t.Fatalf("unknown reason should survive as its name, got %q", got)
	}
}

// A status without a reason decodes to the empty string rather than failing.
func TestTaskStatus_ReasonAbsent(t *testing.T) {
	const raw = `{"type":"UPDATE","update":{"status":{
	  "task_id":{"value":"t-1"},"node_id":{"value":"n-1"},"state":"TASK_RUNNING"}}}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := ev.Update.Status.Reason; got != "" {
		t.Fatalf("absent reason should be empty, got %q", got)
	}
}
