package medios

import (
	"encoding/json"
	"testing"
)

// SUBSCRIBED carries the controller's own description. Before this was
// modelled the SDK decoded framework_id and the heartbeat interval and dropped
// the rest, so a framework had no way to tell which controller had accepted it.

func TestSubscribed_ControllerInfoDecodes(t *testing.T) {
	const raw = `{"type":"SUBSCRIBED","subscribed":{
	  "framework_id":{"value":"abc-0001"},
	  "heartbeat_interval_seconds":15.0,
	  "controller_info":{
	    "id":"ctrl-20260910-1",
	    "version":"1.11.0",
	    "hostname":"controller-1.example",
	    "address":{"hostname":"controller-1.example","ip":"10.0.0.1","port":5050},
	    "capabilities":[{"type":"NODE_DRAINING"},{"type":"QUOTA_V2"}]}}}`

	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ci := ev.Subscribed.ControllerInfo
	if ci == nil {
		t.Fatal("controller_info was dropped")
	}
	if ci.ID != "ctrl-20260910-1" || ci.Version != "1.11.0" {
		t.Errorf("id/version = %q/%q", ci.ID, ci.Version)
	}
	if ci.Address == nil || ci.Address.IP != "10.0.0.1" || ci.Address.Port != 5050 {
		t.Errorf("address = %+v", ci.Address)
	}
	if len(ci.Capabilities) != 2 || ci.Capabilities[0].Type != "NODE_DRAINING" {
		t.Errorf("capabilities = %+v", ci.Capabilities)
	}
}

// A capability this library has never heard of survives as its name. Which
// capabilities a controller honors is its contract to state, so an unknown one
// is data to pass on rather than an error.
func TestSubscribed_UnknownControllerCapability(t *testing.T) {
	const raw = `{"type":"SUBSCRIBED","subscribed":{"framework_id":{"value":"f-1"},
	  "controller_info":{"id":"c1","capabilities":[{"type":"SOMETHING_LATER"}]}}}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := ev.Subscribed.ControllerInfo.Capabilities
	if len(got) != 1 || got[0].Type != "SOMETHING_LATER" {
		t.Fatalf("unknown capability should survive as its name, got %+v", got)
	}
}

// An event carrying no controller_info, or an empty one, decodes without
// inventing a controller. The SDK's own RecordIO fixture has been sending the
// empty form all along.
func TestSubscribed_ControllerInfoAbsentOrEmpty(t *testing.T) {
	for name, raw := range map[string]string{
		"absent": `{"type":"SUBSCRIBED","subscribed":{"framework_id":{"value":"f-1"}}}`,
		"empty":  `{"type":"SUBSCRIBED","subscribed":{"framework_id":{"value":"f-1"},"controller_info":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var ev Event
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ci := ev.Subscribed.ControllerInfo
			if name == "absent" && ci != nil {
				t.Fatalf("no controller_info should decode as nil, got %+v", ci)
			}
			if name == "empty" && (ci == nil || ci.ID != "") {
				t.Fatalf("an empty controller_info should decode as an empty one, got %+v", ci)
			}
		})
	}
}

// The controller id changes on failover, so a framework that records it across
// subscriptions can tell "reconnected to the same controller" from
// "reconnected to a different one". The framework id is identical in both
// cases and cannot make that distinction.
func TestControllerInfo_IDDistinguishesFailover(t *testing.T) {
	sub := func(controllerID string) *ControllerInfo {
		raw := `{"type":"SUBSCRIBED","subscribed":{"framework_id":{"value":"same-framework"},
		  "controller_info":{"id":"` + controllerID + `"}}}`
		var ev Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return ev.Subscribed.ControllerInfo
	}
	first, reconnect, failedOver := sub("ctrl-A"), sub("ctrl-A"), sub("ctrl-B")
	if first.ID != reconnect.ID {
		t.Error("the same controller should present the same id")
	}
	if first.ID == failedOver.ID {
		t.Error("a different controller must be distinguishable by id")
	}
}
