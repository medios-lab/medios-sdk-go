package medios

import (
	"encoding/json"
	"strings"
	"testing"
)

// storeResource is a store as DR-036 has a node advertise it; the shared
// marker rides in the resource's unmodeled fields and round-trips.
func storeResource(name string, readers, writers string) Resource {
	var r Resource
	raw := `{"name":"` + StoreResourcePrefix + name + `","type":"SCALAR","scalar":{"value":1},"shared":{},` +
		`"metadata":{"labels":[{"key":"class","value":"local"},{"key":"locality","value":"near"},` +
		`{"key":"readers","value":"` + readers + `"},{"key":"writers","value":"` + writers + `"}]}}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		panic(err)
	}
	return r
}

func storeOffer(rs ...Resource) Offer {
	return Offer{ID: ID{Value: "O-1"}, NodeID: ID{Value: "S0"}, Hostname: "host-0", Resources: rs}
}

type ask struct {
	name, path string
	mode       StoreMode
}

// taskWithStores builds the volumes and resources by hand, so a task can
// ask for a store the offer under test does not carry.
func taskWithStores(t *testing.T, stores ...ask) TaskInfo {
	t.Helper()
	ti := imageTask(t)
	for _, s := range stores {
		v, err := StoreVolume(s.name, s.path, s.mode)
		if err != nil {
			t.Fatal(err)
		}
		ti.Container.Volumes = append(ti.Container.Volumes, v)
		if !hasResource(ti.Resources, StoreResourcePrefix+s.name) {
			ti.Resources = append(ti.Resources, storeResource(s.name, "0", "0"))
		}
	}
	return ti
}

func TestFitsStores(t *testing.T) {
	tables := storeResource("tabicl-tables", "4", "0")
	results := storeResource("results", "4", "1")
	cases := []struct {
		name  string
		offer Offer
		asks  []ask
		fit   bool
		why   string
	}{
		{"no volumes fits anything", storeOffer(), nil, true, ""},
		{"RO with a free reader", storeOffer(tables), []ask{{"tabicl-tables", "/data", StoreRO}}, true, ""},
		{"RW on a read-only store", storeOffer(tables), []ask{{"tabicl-tables", "/data", StoreRW}}, false, "declares 0 writer units"},
		{"store not on the node", storeOffer(results), []ask{{"tabicl-tables", "/data", StoreRO}}, false, "not on node host-0"},
		{"RO and RW-exclusive on two stores", storeOffer(tables, results),
			[]ask{{"tabicl-tables", "/data", StoreRO}, {"results", "/out", StoreRWExclusive}}, true, ""},
		{"two writers on a one-writer store", storeOffer(results),
			[]ask{{"results", "/a", StoreRW}, {"results", "/b", StoreRW}}, false, "declares 1 writer units on node host-0, task asks 2"},
		{"RO on a store declaring no readers", storeOffer(storeResource("tabicl-tables", "0", "1")),
			[]ask{{"tabicl-tables", "/data", StoreRO}}, false, "declares 0 reader units"},
	}
	for _, c := range cases {
		ti := taskWithStores(t, c.asks...)
		fit, why := FitsStores(c.offer, ti)
		if fit != c.fit || !strings.Contains(why, c.why) {
			t.Errorf("%s: fit=%v why=%q, want fit=%v why containing %q", c.name, fit, why, c.fit, c.why)
		}
	}
}

func TestFindStore(t *testing.T) {
	o := storeOffer(storeResource("results", "4", "1"), Resource{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 2}})
	u, ok := FindStore(o, "results")
	if !ok || u.Readers != 4 || u.Writers != 1 || u.Class != "local" || u.Locality != "near" {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	// The resource a task lists is the offered one, shared marker and all.
	out, _ := json.Marshal(o.Resources[0])
	if !strings.Contains(string(out), `"shared":{}`) {
		t.Fatalf("shared marker lost on round trip: %s", out)
	}
	if _, ok := FindStore(o, "cpus"); ok {
		t.Fatal("a plain resource is not a store")
	}
}

// A status update carries the controller's record of what the task was
// given; a status without it decodes to no data.
func TestTaskStatus_DecodesStores(t *testing.T) {
	raw := `{"task_id":{"value":"t-1"},"state":"TASK_RUNNING","node_id":{"value":"S0"},` +
		`"stores":[{"store":"tabicl-tables","container_path":"/data","mode":"RO","node_id":{"value":"S0"}},` +
		`{"store":"results","container_path":"/out","mode":"RW","exclusive":true,"node_id":{"value":"S0"}}]}`
	var st TaskStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Stores) != 2 || st.Stores[0].Store != "tabicl-tables" || st.Stores[0].Mode != VolumeRO ||
		st.Stores[1].Exclusive != true || st.Stores[1].NodeID.Value != "S0" {
		t.Fatalf("decoded %+v", st.Stores)
	}
	var old TaskStatus
	if err := json.Unmarshal([]byte(`{"task_id":{"value":"t-1"},"state":"TASK_RUNNING"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Stores != nil {
		t.Fatalf("a status without data must decode none: %+v", old.Stores)
	}
}
