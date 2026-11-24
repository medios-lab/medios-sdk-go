package medios

import (
	"strings"
	"testing"
)

var fleetOffer = Offer{ID: ID{Value: "O-1"}, NodeID: ID{Value: "S0"}, Hostname: "host-0", Resources: []Resource{
	storeResource("tabicl-tables", "4", "0"), storeResource("results", "4", "1"),
}}

func imageTask(t *testing.T) TaskInfo {
	t.Helper()
	ti, err := NewTaskInfo("team:research", "train", ID{Value: "train-1"}, ID{Value: "S0"}, nil,
		CommandInfo{Shell: true, Value: "python train.py"})
	if err != nil {
		t.Fatal(err)
	}
	ti.Container = &ContainerInfo{Type: ContainerMedios, Medios: &MediosInfo{Image: &Image{
		Type: ImageDocker, Docker: ImageDockerInfo{Name: "nvcr.io/nvidia/pytorch:24.09-py3"}}}}
	return ti
}

// The three modes map to the wire as RO, RW, and RW with exclusive set.
func TestStoreVolume_Modes(t *testing.T) {
	cases := []struct {
		mode StoreMode
		wire string
	}{
		{StoreRO, `{"container_path":"/data","mode":"RO","source":{"type":"STORE","store":{"name":"tabicl-tables"}}}`},
		{StoreRW, `{"container_path":"/data","mode":"RW","source":{"type":"STORE","store":{"name":"tabicl-tables"}}}`},
		{StoreRWExclusive, `{"container_path":"/data","mode":"RW","source":{"type":"STORE","store":{"name":"tabicl-tables","exclusive":true}}}`},
	}
	for _, c := range cases {
		v, err := StoreVolume("tabicl-tables", "/data", c.mode)
		if err != nil {
			t.Fatalf("%s: %v", c.mode, err)
		}
		if got := string(mustMarshal(t, v)); got != c.wire {
			t.Errorf("%s:\n got %s\nwant %s", c.mode, got, c.wire)
		}
	}
}

func TestStoreVolume_Refusals(t *testing.T) {
	cases := []struct {
		name, path string
		mode       StoreMode
		want       string
	}{
		{"", "/data", StoreRO, "Store.Name is required"},
		{"tabicl tables", "/data", StoreRO, "malformed"},
		{"tabicl/tables", "/data", StoreRO, "malformed"},
		{"tabicl-tables", "", StoreRO, "is empty"},
		{"tabicl-tables", ".", StoreRO, "names the sandbox or its parent"},
		{"tabicl-tables", "..", StoreRO, "names the sandbox or its parent"},
		{"tabicl-tables", "data/tables", StoreRO, "more than one component"},
		{"tabicl-tables", "./data", StoreRO, "more than one component"},
		{"tabicl-tables", "/data", StoreMode("RWX"), "unknown"},
	}
	for _, c := range cases {
		_, err := StoreVolume(c.name, c.path, c.mode)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("(%q, %q, %s): err = %v, want it to say %q", c.name, c.path, c.mode, err, c.want)
		}
	}
}

// A relative container path is one name inside the sandbox, the form every
// node delivers; it reaches the wire as given, like an absolute one.
func TestStoreVolume_RelativePathIsOneNameInTheSandbox(t *testing.T) {
	for _, path := range []string{"data", "tabicl.tables", "/data", "/mnt/data/tables"} {
		v, err := StoreVolume("tabicl-tables", path, StoreRW)
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if v.ContainerPath != path {
			t.Fatalf("%q reached the volume as %q", path, v.ContainerPath)
		}
	}
}

// Two volumes at one container path are refused, as the controller does;
// one store at two paths is fine.
func TestValidateVolumes_RefusesASharedContainerPath(t *testing.T) {
	ti, err := imageTask(t).WithStore(fleetOffer, "tabicl-tables", "data", StoreRO)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ti.WithStore(fleetOffer, "results", "data", StoreRW); err == nil || !strings.Contains(err.Error(), `container path "data"`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ti.WithStore(fleetOffer, "tabicl-tables", "tables", StoreRO); err != nil {
		t.Fatal(err)
	}
}

// A volume built by hand is held to the same path rule as StoreVolume.
func TestValidateVolumes_ChecksTheContainerPath(t *testing.T) {
	ti := imageTask(t)
	ti.Resources = []Resource{storeResource("results", "4", "1")}
	ti.Container.Volumes = []Volume{{ContainerPath: "out/run1", Mode: VolumeRW,
		Source: &VolumeSource{Type: VolumeSourceStore, Store: &StoreSource{Name: "results"}}}}
	if err := ValidateVolumes(ti); err == nil || !strings.Contains(err.Error(), "more than one component") {
		t.Fatalf("err = %v", err)
	}
}

func TestWithStore_AppendsAndKeepsTheTask(t *testing.T) {
	ti := imageTask(t)
	ti, err := ti.WithStore(fleetOffer, "tabicl-tables", "/data", StoreRO)
	if err != nil {
		t.Fatal(err)
	}
	ti, err = ti.WithStore(fleetOffer, "results", "/out", StoreRWExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if len(ti.Container.Volumes) != 2 || ti.Principal != "team:research" {
		t.Fatalf("task = %+v", ti)
	}
	if len(ti.Resources) != 2 || ti.Resources[0].Name != "store/tabicl-tables" || ti.Resources[1].Name != "store/results" {
		t.Fatalf("store resources not taken from the offer: %+v", ti.Resources)
	}
	got := string(mustMarshal(t, ti))
	mustContain(t, got, `"volumes":[{"container_path":"/data","mode":"RO","source":{"type":"STORE","store":{"name":"tabicl-tables"}}},`+
		`{"container_path":"/out","mode":"RW","source":{"type":"STORE","store":{"name":"results","exclusive":true}}}]`)
	if strings.Contains(got, "host_path") {
		t.Fatalf("a host path reached the wire: %s", got)
	}
	if ti, err = ti.WithStore(fleetOffer, "results", "/scratch", StoreRW); err != nil || len(ti.Resources) != 2 {
		t.Fatalf("a second volume on one store must not list the resource twice: %v %+v", err, ti.Resources)
	}
}

func TestWithStore_NeedsTheStoreInTheOffer(t *testing.T) {
	ti := imageTask(t)
	if _, err := ti.WithStore(fleetOffer, "ollama-models", "/models", StoreRO); err == nil || !strings.Contains(err.Error(), "not in the offer from node host-0") {
		t.Fatalf("err = %v", err)
	}
}

// A bare command may mount a store: WithStore builds the MEDIOS container
// with no image that the volume rides on, lists the store resource, and
// leaves delivery to the node.
func TestWithStore_BareCommandGetsAnImagelessMediosContainer(t *testing.T) {
	ti := imageTask(t)
	ti.Container = nil
	ti, err := ti.WithStore(fleetOffer, "tabicl-tables", "/data", StoreRO)
	if err != nil {
		t.Fatal(err)
	}
	c := ti.Container
	if c == nil || c.Type != ContainerMedios || c.Medios == nil || c.Medios.Image != nil || c.Docker != nil {
		t.Fatalf("container = %+v", c)
	}
	if len(c.Volumes) != 1 || c.Volumes[0].Source.Store.Name != "tabicl-tables" {
		t.Fatalf("volumes = %+v", c.Volumes)
	}
	if len(ti.Resources) != 1 || ti.Resources[0].Name != "store/tabicl-tables" {
		t.Fatalf("resources = %+v", ti.Resources)
	}
	got := string(mustMarshal(t, ti))
	mustContain(t, got, `"container":{"type":"MEDIOS","medios":{},"volumes":[{"container_path":"/data","mode":"RO","source":{"type":"STORE","store":{"name":"tabicl-tables"}}}]}`)
	if err := ValidateVolumes(ti); err != nil {
		t.Fatal(err)
	}
}

// A Container the framework set is kept as it is; only the volume and the
// resource are added.
func TestWithStore_KeepsAnExistingContainer(t *testing.T) {
	ti := imageTask(t)
	ti, err := ti.WithStore(fleetOffer, "tabicl-tables", "/data", StoreRO)
	if err != nil {
		t.Fatal(err)
	}
	if ti.Container.Medios.Image == nil || ti.Container.Medios.Image.Docker.Name != "nvcr.io/nvidia/pytorch:24.09-py3" {
		t.Fatalf("image lost: %+v", ti.Container)
	}
}

// A HOST_PATH source, or a volume with no source, anywhere on the task is
// refused with a sentence that says what frameworks send instead.
func TestValidateVolumes_RefusesHostPath(t *testing.T) {
	ti := imageTask(t)
	ti.Container.Volumes = []Volume{{ContainerPath: "/data", Mode: VolumeRW,
		Source: &VolumeSource{Type: VolumeSourceHostPath}}}
	err := ValidateVolumes(ti)
	if err == nil || !strings.Contains(err.Error(), "HOST_PATH") || !strings.Contains(err.Error(), "not host paths") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ti.WithStore(fleetOffer, "results", "/out", StoreRW); err == nil || !strings.Contains(err.Error(), "HOST_PATH") {
		t.Fatalf("WithStore let a host path through: %v", err)
	}

	ti.Container.Volumes = []Volume{{ContainerPath: "/data", Mode: VolumeRW}}
	if err := ValidateVolumes(ti); err == nil || !strings.Contains(err.Error(), "no source") {
		t.Fatalf("err = %v", err)
	}

	ti.Container.Volumes = []Volume{{ContainerPath: "/data", Mode: VolumeRO,
		Source: &VolumeSource{Type: VolumeSourceStore, Store: &StoreSource{Name: "tabicl-tables", Exclusive: true}}}}
	if err := ValidateVolumes(ti); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("err = %v", err)
	}

	ti.Container.Volumes = []Volume{{ContainerPath: "/data", Mode: VolumeRO,
		Source: &VolumeSource{Type: VolumeSourceStore, Store: &StoreSource{Name: "tabicl-tables"}}}}
	if err := ValidateVolumes(ti); err == nil || !strings.Contains(err.Error(), "does not list resource store/tabicl-tables") {
		t.Fatalf("a store volume without its resource must be refused: %v", err)
	}

	ti.Container.Volumes = nil
	if err := ValidateVolumes(ti); err != nil {
		t.Fatalf("no volumes must pass: %v", err)
	}
}
