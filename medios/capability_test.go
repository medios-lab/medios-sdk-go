package medios

import (
	"encoding/json"
	"strings"
	"testing"
)

// Capabilities reach the wire as the names the controller expects, and an
// unknown one passes through, since which capabilities a controller honors is
// its contract to state.
func TestCapabilities_ReachTheWireAsNames(t *testing.T) {
	info := FrameworkInfo{
		Name: "f",
		Capabilities: []Capability{
			{Type: CapabilityGPUResources},
			{Type: CapabilityPartitionAware},
			{Type: "SOMETHING_LATER"},
		},
	}
	blob, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"GPU_RESOURCES"`, `"PARTITION_AWARE"`, `"SOMETHING_LATER"`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("missing %s in %s", want, blob)
		}
	}
	var back FrameworkInfo
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Capabilities) != 3 || back.Capabilities[1].Type != CapabilityPartitionAware {
		t.Fatalf("capabilities did not round-trip: %+v", back.Capabilities)
	}
}
