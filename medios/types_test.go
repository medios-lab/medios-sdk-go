package medios

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// These tests pin the JSON wire format against the medios HTTP v1 shape we
// empirically observed. The goal is "if medios changes a field name or
// shape, these tests fail loudly here rather than silently at runtime."

// ── Event decoding ────────────────────────────────────────────────────────

func TestDecodeEvent_Subscribed(t *testing.T) {
	const raw = `{"type":"SUBSCRIBED","subscribed":{"framework_id":{"value":"abc-0001"},"heartbeat_interval_seconds":15.0}}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != EventSubscribed {
		t.Fatalf("type: want %s, got %s", EventSubscribed, ev.Type)
	}
	if ev.Subscribed == nil {
		t.Fatal("subscribed body: nil")
	}
	if ev.Subscribed.FrameworkID.Value != "abc-0001" {
		t.Errorf("framework_id: %q", ev.Subscribed.FrameworkID.Value)
	}
	if ev.Subscribed.HeartbeatIntervalSeconds != 15.0 {
		t.Errorf("heartbeat: %v", ev.Subscribed.HeartbeatIntervalSeconds)
	}
}

func TestDecodeEvent_Heartbeat(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"type":"HEARTBEAT"}`), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != EventHeartbeat {
		t.Errorf("type: want %s, got %s", EventHeartbeat, ev.Type)
	}
	// No body fields should be populated.
	if ev.Subscribed != nil || ev.Offers != nil || ev.Update != nil ||
		ev.Rescind != nil || ev.Error != nil {
		t.Errorf("HEARTBEAT should leave all variant bodies nil")
	}
}

func TestDecodeEvent_Offers(t *testing.T) {
	const raw = `{
		"type": "OFFERS",
		"offers": {
			"offers": [
				{
					"id":          {"value": "O-1"},
					"framework_id":{"value": "abc-0001"},
					"node_id":     {"value": "abc-S0"},
					"hostname":    "localhost",
					"resources": [
						{"name": "cpus", "type": "SCALAR", "scalar": {"value": 2.0}},
						{"name": "mem",  "type": "SCALAR", "scalar": {"value": 1024.0}}
					]
				}
			]
		}
	}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != EventOffers || ev.Offers == nil {
		t.Fatalf("type/body: %+v", ev)
	}
	if got := len(ev.Offers.Offers); got != 1 {
		t.Fatalf("offers count: want 1, got %d", got)
	}
	o := ev.Offers.Offers[0]
	if o.ID.Value != "O-1" || o.NodeID.Value != "abc-S0" || o.Hostname != "localhost" {
		t.Errorf("offer fields: %+v", o)
	}
	if len(o.Resources) != 2 {
		t.Fatalf("resource count: %d", len(o.Resources))
	}
	cpus := o.Resources[0]
	if cpus.Name != "cpus" || cpus.Type != "SCALAR" || cpus.Scalar == nil || cpus.Scalar.Value != 2.0 {
		t.Errorf("cpus: %+v", cpus)
	}
}

func TestDecodeEvent_Update_WithContainerStatus(t *testing.T) {
	// First TASK_RUNNING update carries container_status. The scheduler MUST
	// capture container_id from here to construct the sandbox stdout path.
	const raw = `{
		"type": "UPDATE",
		"update": {
			"status": {
				"task_id":  {"value": "t-1"},
				"state":    "TASK_RUNNING",
				"node_id":  {"value": "S0"},
				"uuid":     "abcd-uuid",
				"source":   "SOURCE_EXECUTOR",
				"timestamp": 1700000000.5,
				"container_status": {"container_id": {"value": "ctr-xyz"}}
			}
		}
	}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != EventUpdate || ev.Update == nil {
		t.Fatalf("type/body: %+v", ev)
	}
	s := ev.Update.Status
	if s.State != "TASK_RUNNING" || s.TaskID.Value != "t-1" || s.NodeID.Value != "S0" {
		t.Errorf("status core: %+v", s)
	}
	if s.UUID != "abcd-uuid" || s.Source != "SOURCE_EXECUTOR" {
		t.Errorf("status ack fields: %+v", s)
	}
	if s.ContainerStatus == nil || s.ContainerStatus.ContainerID.Value != "ctr-xyz" {
		t.Errorf("container_status: %+v", s.ContainerStatus)
	}
}

func TestDecodeEvent_Error(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"type":"ERROR","error":{"message":"validation failed"}}`), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != EventError || ev.Error == nil || ev.Error.Message != "validation failed" {
		t.Errorf("error event: %+v", ev)
	}
}

// ── Call encoding ─────────────────────────────────────────────────────────

func TestEncodeCall_Subscribe(t *testing.T) {
	c := NewSubscribeCall(FrameworkInfo{User: "alice", Name: "test-fw"})
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	mustContain(t, got, `"type":"SUBSCRIBE"`)
	mustContain(t, got, `"subscribe":`)
	mustContain(t, got, `"framework_info":`)
	mustContain(t, got, `"user":"alice"`)
	mustContain(t, got, `"name":"test-fw"`)
	// A first SUBSCRIBE must NOT carry a framework_id at the top level (the
	// controller assigns it).
	if strings.Contains(got, `"framework_id"`) {
		t.Errorf("first SUBSCRIBE should not include framework_id: %s", got)
	}
}

func TestEncodeCall_Resubscribe(t *testing.T) {
	// On re-subscribe the controller validates that the top-level
	// call.framework_id is present and equal to subscribe.framework_info.id;
	// omitting it is rejected with HTTP 400 "'framework_id' differs from
	// 'subscribe.framework_info.id'" (observed live 2026-07-25, and the root
	// cause of jinkies' resubscribe crash-loop).
	id := &ID{Value: "abc-0007"}
	c := NewSubscribeCall(FrameworkInfo{User: "alice", Name: "test-fw", ID: id})
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	mustContain(t, got, `"type":"SUBSCRIBE"`)
	// Top-level framework_id, present and equal to framework_info.id.
	mustContain(t, got, `"framework_id":{"value":"abc-0007"}`)
	mustContain(t, got, `"id":{"value":"abc-0007"}`)
}

func TestEncodeCall_Accept(t *testing.T) {
	tasks := []TaskInfo{{
		Name:   "hello",
		TaskID: ID{Value: "t-1"},
		NodeID: ID{Value: "S0"},
		Resources: []Resource{
			{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 0.5}},
			{Name: "mem", Type: "SCALAR", Scalar: &Scalar{Value: 128}},
		},
		Command: CommandInfo{Shell: true, Value: "echo hello"},
	}}
	c := NewAcceptCall("fw-001", []ID{{Value: "O-1"}}, tasks)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"type":"ACCEPT"`,
		`"framework_id":{"value":"fw-001"}`,
		`"offer_ids":[{"value":"O-1"}]`,
		`"type":"LAUNCH"`,
		`"task_infos":`,
		`"name":"hello"`,
		`"task_id":{"value":"t-1"}`,
		`"node_id":{"value":"S0"}`,
		`"command":{"shell":true,"value":"echo hello"}`,
	} {
		mustContain(t, got, want)
	}
}

func TestEncodeCall_Decline(t *testing.T) {
	c := NewDeclineCall("fw-001", []ID{{Value: "O-1"}, {Value: "O-2"}}, 0)
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"DECLINE"`)
	mustContain(t, got, `"offer_ids":[{"value":"O-1"},{"value":"O-2"}]`)
	// refuseSeconds 0 omits filters entirely (controller uses its default).
	if strings.Contains(got, "filters") {
		t.Errorf("DECLINE with refuseSeconds 0 should omit filters, got %s", got)
	}
}

func TestEncodeCall_DeclineWithFilter(t *testing.T) {
	c := NewDeclineCall("fw-001", []ID{{Value: "O-1"}}, 120)
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"DECLINE"`)
	mustContain(t, got, `"filters":{"refuse_seconds":120}`)
}

func TestEncodeCall_Suppress(t *testing.T) {
	c := NewSuppressCall("fw-001")
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"SUPPRESS"`)
	mustContain(t, got, `"framework_id":{"value":"fw-001"}`)
}

func TestEncodeCall_Revive(t *testing.T) {
	c := NewReviveCall("fw-001")
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"REVIVE"`)
	mustContain(t, got, `"framework_id":{"value":"fw-001"}`)
}

func TestEncodeCall_Reconcile(t *testing.T) {
	c := NewReconcileCall("fw-001", []ReconcileTask{{TaskID: ID{Value: "t-1"}, NodeID: &ID{Value: "S0"}}})
	got := string(mustMarshal(t, c))
	mustContain(t, got, `"type":"RECONCILE"`)
	mustContain(t, got, `"reconcile":{"tasks":[{"task_id":{"value":"t-1"},"node_id":{"value":"S0"}}]}`)
}

func TestEncodeCall_ReconcileAll(t *testing.T) {
	// nil tasks → reconcile every known task; must encode "tasks":[] not null.
	c := NewReconcileCall("fw-001", nil)
	got := string(mustMarshal(t, c))
	mustContain(t, got, `"type":"RECONCILE"`)
	mustContain(t, got, `"reconcile":{"tasks":[]}`)
}

// Re-subscribe carries the persisted framework_id + a failover_timeout so the
// controller treats this as the same framework and holds its tasks.
func TestEncodeCall_SubscribeResubscribe(t *testing.T) {
	c := NewSubscribeCall(FrameworkInfo{User: "u", Name: "fw", ID: &ID{Value: "fw-prev"}, FailoverTimeout: 60})
	got := string(mustMarshal(t, c))
	mustContain(t, got, `"id":{"value":"fw-prev"}`)
	mustContain(t, got, `"failover_timeout":60`)
}

// First subscribe (no persisted id, 0 failover) omits both.
func TestEncodeCall_SubscribeFirstTimeOmitsIDAndFailover(t *testing.T) {
	c := NewSubscribeCall(FrameworkInfo{User: "u", Name: "fw"})
	got := string(mustMarshal(t, c))
	if strings.Contains(got, `"id":{`) {
		t.Errorf("first subscribe must omit framework_info.id, got %s", got)
	}
	if strings.Contains(got, "failover_timeout") {
		t.Errorf("first subscribe must omit failover_timeout, got %s", got)
	}
}

// A bare-command TaskInfo omits container/labels/environment entirely (so
// existing bare-command consumers like fennel are unaffected).
func TestEncodeTaskInfo_BareCommandOmitsNewFields(t *testing.T) {
	ti := TaskInfo{
		Name: "t", TaskID: ID{Value: "t-1"}, NodeID: ID{Value: "S0"},
		Command: CommandInfo{Shell: true, Value: "echo hi"},
	}
	got := string(mustMarshal(t, ti))
	for _, absent := range []string{"container", "labels", "environment"} {
		if strings.Contains(got, absent) {
			t.Errorf("bare command must omit %q, got %s", absent, got)
		}
	}
}

// A container stage carries Container (DOCKER + shape-1 volumes), Labels, and
// CommandInfo.Environment in the shapes the controller accepts (issue #10).
func TestEncodeTaskInfo_ContainerLabelsEnv(t *testing.T) {
	ti := TaskInfo{
		Name:   "build-compile",
		TaskID: ID{Value: "b1-compile"},
		NodeID: ID{Value: "S0"},
		Command: CommandInfo{
			Shell: true, Value: "make",
			Environment: &Environment{Variables: []EnvVar{{Name: "CI", Value: "1"}}},
		},
		Container: &ContainerInfo{
			Type:   ContainerDocker,
			Docker: &DockerInfo{Image: "alpine:latest"},
			Volumes: []Volume{
				{ContainerPath: "/abs/ws", Mode: VolumeRW,
					Source: &VolumeSource{Type: VolumeSourceStore, Store: &StoreSource{Name: "ws"}}},
			},
		},
		Labels: &Labels{Labels: []Label{
			{Key: "jinkies.build_id", Value: "b1"},
			{Key: "jinkies.stage", Value: "compile"},
		}},
	}
	got := string(mustMarshal(t, ti))
	for _, want := range []string{
		`"container":{"type":"DOCKER","docker":{"image":"alpine:latest"}`,
		`"volumes":[{"container_path":"/abs/ws","mode":"RW","source":{"type":"STORE","store":{"name":"ws"}}}]`,
		`"labels":{"labels":[{"key":"jinkies.build_id","value":"b1"},{"key":"jinkies.stage","value":"compile"}]}`,
		`"environment":{"variables":[{"name":"CI","value":"1"}]}`,
	} {
		mustContain(t, got, want)
	}
}

// A MEDIOS container carries the image under medios.image and no docker
// block, which is the shape a node running the medios containerizer accepts.
func TestTaskInfoMediosContainerWire(t *testing.T) {
	ti := TaskInfo{
		Name:    "gpu-one",
		TaskID:  ID{Value: "gpu-one"},
		NodeID:  ID{Value: "S4"},
		Command: CommandInfo{Shell: true, Value: "nvidia-smi -L"},
		Container: &ContainerInfo{
			Type: ContainerMedios,
			Medios: &MediosInfo{Image: &Image{
				Type:   ImageDocker,
				Docker: ImageDockerInfo{Name: "nvcr.io/nvidia/cuda:12.6.2-base-ubuntu24.04"},
				Cached: new(bool),
			}},
		},
	}
	got := string(mustMarshal(t, ti))
	mustContain(t, got,
		`"container":{"type":"MEDIOS","medios":{"image":{"type":"DOCKER","docker":{"name":"nvcr.io/nvidia/cuda:12.6.2-base-ubuntu24.04"},"cached":false}}}`)
	if strings.Contains(got, `"docker":{"image"`) {
		t.Fatalf("a MEDIOS container must not carry a docker block: %s", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// CommandInfo.URIs (the fetcher): a default-extract archive omits `extract`
// (so the controller applies its default, true), and a plain binary sends an
// explicit `extract:false` alongside `executable:true`.
func TestEncodeCommandInfo_URIs(t *testing.T) {
	ci := CommandInfo{
		Shell: true, Value: "./run.sh",
		URIs: []URI{
			{Value: "https://x/app.tar.gz"},
			{Value: "file:///bin/tool", Executable: true, Extract: BoolPtr(false)},
		},
	}
	got := string(mustMarshal(t, ci))
	mustContain(t, got, `{"value":"https://x/app.tar.gz"}`)
	mustContain(t, got, `{"value":"file:///bin/tool","executable":true,"extract":false}`)
}

func TestEncodeCall_Kill(t *testing.T) {
	c := NewKillCall("fw-001", "t-1", "S0")
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"KILL"`)
	mustContain(t, got, `"task_id":{"value":"t-1"}`)
	mustContain(t, got, `"node_id":{"value":"S0"}`)
}

func TestEncodeCall_Acknowledge(t *testing.T) {
	c := NewAcknowledgeCall("fw-001", "t-1", "S0", "abcd-uuid")
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"ACKNOWLEDGE"`)
	mustContain(t, got, `"task_id":{"value":"t-1"}`)
	mustContain(t, got, `"node_id":{"value":"S0"}`)
	mustContain(t, got, `"uuid":"abcd-uuid"`)
}

func TestEncodeCall_Teardown(t *testing.T) {
	c := NewTeardownCall("fw-001")
	b, _ := json.Marshal(c)
	got := string(b)
	mustContain(t, got, `"type":"TEARDOWN"`)
	mustContain(t, got, `"framework_id":{"value":"fw-001"}`)
	// No variant body for TEARDOWN.
	for _, mustNot := range []string{`"subscribe"`, `"accept"`, `"decline"`, `"kill"`, `"acknowledge"`} {
		if strings.Contains(got, mustNot) {
			t.Errorf("TEARDOWN should not include %s: %s", mustNot, got)
		}
	}
}

// ── Round trip ────────────────────────────────────────────────────────────

func TestRoundTrip_Call(t *testing.T) {
	original := NewAcceptCall("fw-001", []ID{{Value: "O-1"}}, []TaskInfo{{
		Name:   "hello",
		TaskID: ID{Value: "t-1"},
		NodeID: ID{Value: "S0"},
		Resources: []Resource{
			{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 0.5}},
		},
		Command: CommandInfo{Shell: true, Value: "echo hi"},
	}})
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Call
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != original.Type {
		t.Errorf("type drift: %s vs %s", decoded.Type, original.Type)
	}
	if decoded.Accept == nil || len(decoded.Accept.Operations) != 1 {
		t.Fatalf("decoded accept: %+v", decoded.Accept)
	}
	op := decoded.Accept.Operations[0]
	if op.Type != OperationLaunch || op.Launch == nil || len(op.Launch.TaskInfos) != 1 {
		t.Fatalf("decoded operation: %+v", op)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("missing %q in:\n  %s", needle, haystack)
	}
}

func TestDecodeOffer_PortsRanges(t *testing.T) {
	const raw = `{
		"type": "OFFERS",
		"offers": {"offers": [{
			"id":          {"value": "O-1"},
			"framework_id":{"value": "abc-0001"},
			"node_id":     {"value": "abc-S0"},
			"hostname":    "node1.local",
			"resources": [
				{"name": "cpus",  "type": "SCALAR", "scalar": {"value": 2.0}},
				{"name": "ports", "type": "RANGES", "ranges": {"range": [
					{"begin": 31000, "end": 31002},
					{"begin": 31010, "end": 31010}
				]}}
			]
		}]}
	}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	o := ev.Offers.Offers[0]

	ports := PortRangesFromOffer(o)
	if ports == nil {
		t.Fatalf("PortRangesFromOffer returned nil; resources=%+v", o.Resources)
	}
	got := ports.Ports()
	want := []uint64{31000, 31001, 31002, 31010}
	if len(got) != len(want) {
		t.Fatalf("Ports(): want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Ports()[%d]: want %d, got %d", i, want[i], got[i])
		}
	}
}

func TestEncodePortsResource_RoundTrip(t *testing.T) {
	r := NewPortsResource(31000, 31001, 31004)
	if r.Name != "ports" || r.Type != "RANGES" || r.Ranges == nil {
		t.Fatalf("NewPortsResource shape: %+v", r)
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mustContain(t, string(b), `"type":"RANGES"`)
	mustContain(t, string(b), `"begin":31004`)

	var back Resource
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := back.Ranges.Ports(); len(got) != 3 || got[0] != 31000 || got[2] != 31004 {
		t.Errorf("round-trip ports: %v", got)
	}
}

func TestNilRangesPorts(t *testing.T) {
	var r *Ranges
	if r.Ports() != nil {
		t.Errorf("nil Ranges.Ports() should be nil")
	}
}

// Declared capabilities reach the SUBSCRIBE call; without them the
// controller's allocator excludes every resource on a GPU-bearing node.
func TestEncodeCall_SubscribeCarriesCapabilities(t *testing.T) {
	c := NewSubscribeCall(FrameworkInfo{
		User: "u", Name: "fw",
		Capabilities: []Capability{{Type: CapabilityGPUResources}},
	})
	got := string(mustMarshal(t, c))
	mustContain(t, got, `"capabilities":[{"type":"GPU_RESOURCES"}]`)
}

// A framework declaring none omits the field, so existing consumers encode
// byte-identically.
func TestEncodeCall_SubscribeOmitsEmptyCapabilities(t *testing.T) {
	c := NewSubscribeCall(FrameworkInfo{User: "u", Name: "fw"})
	if got := string(mustMarshal(t, c)); strings.Contains(got, "capabilities") {
		t.Errorf("no capabilities declared must omit the field, got %s", got)
	}
}

func TestURISha256Wire(t *testing.T) {
	const digest = "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
	b, err := json.Marshal(URI{Value: "http://x/y", Sha256: digest})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"value":"http://x/y","sha256":"`+digest+`"}` {
		t.Fatalf("wire form: %s", got)
	}
	b, err = json.Marshal(URI{Value: "http://x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sha256") {
		t.Fatalf("an empty Sha256 must be omitted: %s", b)
	}
}

// A Ray head on docker_shell: host network, a private IPC namespace with an
// 8 GB /dev/shm, and a 30 s grace period, in the shapes the controller
// accepts (medios#659, medios-sdk-python#8).
func TestEncodeTaskInfo_NetworkLinuxInfoKillPolicy(t *testing.T) {
	shm := uint32(8192)
	ti := TaskInfo{
		Name: "ray-head", TaskID: ID{Value: "ray-head"}, NodeID: ID{Value: "S1"},
		Command: CommandInfo{Shell: true, Value: "ray start --head --block"},
		Container: &ContainerInfo{
			Type:      ContainerDocker,
			Docker:    &DockerInfo{Image: "rayproject/ray:2.40.0", Network: NetworkHost},
			LinuxInfo: &LinuxInfo{IpcMode: IpcPrivate, ShmSize: &shm},
		},
		KillPolicy: &KillPolicy{GracePeriod: NewDurationInfo(30 * time.Second)},
	}
	if err := ValidateContainerInfo(ti.Container); err != nil {
		t.Fatalf("ValidateContainerInfo: %v", err)
	}
	got := string(mustMarshal(t, ti))
	for _, want := range []string{
		`"docker":{"image":"rayproject/ray:2.40.0","network":"HOST"}`,
		`"linux_info":{"ipc_mode":"PRIVATE","shm_size":8192}`,
		`"kill_policy":{"grace_period":{"nanoseconds":30000000000}}`,
	} {
		mustContain(t, got, want)
	}
}

// Unset network, linux_info and kill_policy stay off the wire. An absent
// network matters most: the proto default is HOST, and docker_shell reads
// an absent field as the bridge.
func TestEncodeTaskInfo_UnsetNetworkLinuxInfoKillPolicyOmitted(t *testing.T) {
	ti := TaskInfo{
		Name: "t", TaskID: ID{Value: "t-1"}, NodeID: ID{Value: "S0"},
		Command:   CommandInfo{Shell: true, Value: "echo hi"},
		Container: &ContainerInfo{Type: ContainerDocker, Docker: &DockerInfo{Image: "alpine:latest"}},
	}
	got := string(mustMarshal(t, ti))
	for _, absent := range []string{"network", "linux_info", "kill_policy"} {
		if strings.Contains(got, absent) {
			t.Errorf("unset %q must be omitted, got %s", absent, got)
		}
	}
}

func TestValidateContainerInfo_Refuses(t *testing.T) {
	shm := uint32(64)
	for name, c := range map[string]*ContainerInfo{
		"lowercase network": {Type: ContainerDocker, Docker: &DockerInfo{Image: "a", Network: "host"}},
		"unknown network":   {Type: ContainerDocker, Docker: &DockerInfo{Image: "a", Network: "CNI"}},
		"unknown ipc mode":  {Type: ContainerDocker, LinuxInfo: &LinuxInfo{IpcMode: "PRIVAT"}},
		"shm without mode":  {Type: ContainerDocker, LinuxInfo: &LinuxInfo{ShmSize: &shm}},
		"shm with share":    {Type: ContainerDocker, LinuxInfo: &LinuxInfo{IpcMode: IpcShareParent, ShmSize: &shm}},
	} {
		if err := ValidateContainerInfo(c); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
	for name, c := range map[string]*ContainerInfo{
		"nil":          nil,
		"none network": {Type: ContainerDocker, Docker: &DockerInfo{Image: "a", Network: NetworkNone}},
		"share parent": {Type: ContainerDocker, LinuxInfo: &LinuxInfo{IpcMode: IpcShareParent}},
	} {
		if err := ValidateContainerInfo(c); err != nil {
			t.Errorf("%s: want nil, got %v", name, err)
		}
	}
}

func TestEncodeCall_Request(t *testing.T) {
	nodeID := ID{Value: "S0"}
	c := NewRequestCall("fw-001", []Request{
		{
			NodeID: &nodeID,
			Resources: []Resource{
				{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 4}},
				{Name: "gpus", Type: "SCALAR", Scalar: &Scalar{Value: 1}},
			},
		},
		{
			Resources: []Resource{
				{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 4}},
			},
		},
	})
	got := string(mustMarshal(t, c))
	mustContain(t, got, `"type":"REQUEST"`)
	mustContain(t, got, `"framework_id":{"value":"fw-001"}`)
	mustContain(t, got, `"request":{"requests":[{"node_id":{"value":"S0"},"resources":[{"name":"cpus","type":"SCALAR","scalar":{"value":4}},{"name":"gpus","type":"SCALAR","scalar":{"value":1}}]},{"resources":[{"name":"cpus","type":"SCALAR","scalar":{"value":4}}]}]}`)

	// nil slice encodes as empty array, not null.
	empty := string(mustMarshal(t, NewRequestCall("fw-001", nil)))
	mustContain(t, empty, `"request":{"requests":[]}`)
}

func TestDecodeOffer_WithURLAndDockerParameters(t *testing.T) {
	const raw = `{
		"id": {"value": "O-1"},
		"framework_id": {"value": "fw-1"},
		"node_id": {"value": "S0"},
		"hostname": "node0.internal",
		"url": {
			"scheme": "http",
			"address": {"hostname": "node0.internal", "ip": "10.0.0.5", "port": 5051},
			"path": "/node(1)",
			"query": [{"key": "region", "value": "us-east"}]
		},
		"resources": [{"name": "cpus", "type": "SCALAR", "scalar": {"value": 2}}]
	}`
	var o Offer
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("unmarshal offer: %v", err)
	}
	if o.URL == nil || o.URL.Scheme != "http" || o.URL.Address.IP != "10.0.0.5" ||
		o.URL.Address.Port != 5051 || o.URL.Path != "/node(1)" ||
		len(o.URL.Query) != 1 || o.URL.Query[0].Key != "region" || o.URL.Query[0].Value != "us-east" {
		t.Fatalf("decoded Offer.URL = %+v", o.URL)
	}

	di := DockerInfo{
		Image: "ubuntu:24.04",
		Parameters: []Parameter{
			{Key: "entrypoint", Value: ""},
			{Key: "workdir", Value: "/workspace"},
		},
	}
	encoded := string(mustMarshal(t, di))
	mustContain(t, encoded, `"parameters":[{"key":"entrypoint","value":""},{"key":"workdir","value":"/workspace"}]`)
}
