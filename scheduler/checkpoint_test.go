package scheduler

import (
	"strings"
	"testing"
)

// A node recovers a framework's executors after a restart only if the
// framework subscribed with checkpoint; otherwise the node destroys them as
// orphans.

func TestWithCheckpoint_ReachesSubscribe(t *testing.T) {
	body := subscribeBody(t, WithCheckpoint())
	if !strings.Contains(body, `"checkpoint":true`) {
		t.Fatalf("checkpoint did not reach SUBSCRIBE: %s", body)
	}
}

func TestWithCheckpoint_OmittedWhenUnset(t *testing.T) {
	if body := subscribeBody(t); strings.Contains(body, `"checkpoint"`) {
		t.Fatalf("an unset checkpoint should be omitted: %s", body)
	}
}
