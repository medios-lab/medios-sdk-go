package medios

import (
	"sync"
	"testing"
)

func offerForNodeRole(nodeID, host, role string, attrs []Attribute, rs ...Resource) Offer {
	o := Offer{
		ID:         ID{Value: "O-" + nodeID + "-" + role},
		NodeID:     ID{Value: nodeID},
		Hostname:   host,
		Resources:  rs,
		Attributes: attrs,
	}
	if role != "" {
		o.AllocationInfo = &AllocationInfo{Role: role}
	}
	return o
}

func textAttr(name, val string) Attribute {
	return Attribute{Name: name, Type: "TEXT", Text: &AttributeText{Value: val}}
}

func TestClusterView_EmptyAndColdStart(t *testing.T) {
	v := NewClusterView()
	reqs := []StoreRequest{{Store: "tabicl-tables", Mode: StoreRO}}

	// Before any offer is observed for the role, ClassifyStoreWait returns StoreWaitNone.
	if got := v.ClassifyStoreWait("", reqs, nil); got != StoreWaitNone {
		t.Fatalf("cold cluster view: got %q, want %q", got, StoreWaitNone)
	}
	if got := v.ClassifyStoreWait("*", reqs, nil); got != StoreWaitNone {
		t.Fatalf("cold cluster view (*): got %q, want %q", got, StoreWaitNone)
	}

	v.ObserveOffer(offerForNodeRole("S0", "host-0", "", nil, storeResource("tabicl-tables", "4", "0")))

	// Empty requests always return StoreWaitNone.
	if got := v.ClassifyStoreWait("*", nil, nil); got != StoreWaitNone {
		t.Fatalf("empty reqs: got %q, want %q", got, StoreWaitNone)
	}

	// Unobserved role ("gpu") still returns StoreWaitNone even when "*" has offers.
	if got := v.ClassifyStoreWait("gpu", reqs, nil); got != StoreWaitNone {
		t.Fatalf("unobserved role gpu: got %q, want %q", got, StoreWaitNone)
	}
}

func TestClusterView_ClassifyStoreWait_FourSteps(t *testing.T) {
	v := NewClusterView()
	// Node S0 (rack-1): declares tabicl-tables (RO-only: 4 readers, 0 writers)
	// Node S1 (rack-2): declares results (4 readers, 1 writer) and scratch (2 readers, 2 writers)
	v.ObserveOffers([]Offer{
		offerForNodeRole("S0", "host-0", "*", []Attribute{textAttr("rack", "rack-1")},
			storeResource("tabicl-tables", "4", "0")),
		offerForNodeRole("S1", "host-1", "*", []Attribute{textAttr("rack", "rack-2")},
			storeResource("results", "4", "1"),
			storeResource("scratch", "2", "2")),
	})

	cases := []struct {
		name     string
		role     string
		reqs     []StoreRequest
		nodePred func(Offer) bool
		want     StoreWaitReason
	}{
		{
			name: "Step 1: store undeclared on all nodes in role",
			role: "",
			reqs: []StoreRequest{{Store: "ollama-models", Mode: StoreRO}},
			want: StoreWaitUndeclared,
		},
		{
			name: "Step 1 precedence over Step 2: one store mode-mismatch and one store undeclared",
			role: "*",
			reqs: []StoreRequest{
				{Store: "tabicl-tables", Mode: StoreRW},
				{Store: "ollama-models", Mode: StoreRO},
			},
			want: StoreWaitUndeclared,
		},
		{
			name: "Step 2: RW requested on read-only store (writers=0)",
			role: "*",
			reqs: []StoreRequest{{Store: "tabicl-tables", Mode: StoreRW}},
			want: StoreWaitMode,
		},
		{
			name: "Step 2: RW-exclusive requested on read-only store (writers=0)",
			role: "*",
			reqs: []StoreRequest{{Store: "tabicl-tables", Mode: StoreRWExclusive}},
			want: StoreWaitMode,
		},
		{
			name: "Step 2: RW-exclusive takes all writers so a second RW request on the same store exceeds capacity",
			role: "*",
			reqs: []StoreRequest{
				{Store: "scratch", Mode: StoreRWExclusive},
				{Store: "scratch", Mode: StoreRW},
			},
			want: StoreWaitMode,
		},
		{
			name: "Step 2: aggregated duplicate store requests exceed declared writer units",
			role: "*",
			reqs: []StoreRequest{
				{Store: "results", Mode: StoreRW},
				{Store: "results", Mode: StoreRW},
			},
			want: StoreWaitMode,
		},
		{
			name: "Step 2: aggregated duplicate store requests exceed declared reader units",
			role: "*",
			reqs: []StoreRequest{
				{Store: "scratch", Mode: StoreRO},
				{Store: "scratch", Mode: StoreRO},
				{Store: "scratch", Mode: StoreRO},
			},
			want: StoreWaitMode,
		},
		{
			name: "Step 2 precedence over Step 3: split stores across nodes where one also lacks writer mode",
			role: "*",
			reqs: []StoreRequest{
				{Store: "tabicl-tables", Mode: StoreRW},
				{Store: "results", Mode: StoreRO},
			},
			want: StoreWaitMode,
		},
		{
			name: "Step 3: each store fits on some node, but no single node carries both stores",
			role: "*",
			reqs: []StoreRequest{
				{Store: "tabicl-tables", Mode: StoreRO},
				{Store: "results", Mode: StoreRWExclusive},
			},
			want: StoreWaitPlacement,
		},
		{
			name: "Step 3: store fits on S0, but placement predicate requires rack-2 (S1)",
			role: "*",
			reqs: []StoreRequest{{Store: "tabicl-tables", Mode: StoreRO}},
			nodePred: func(o Offer) bool {
				for _, a := range o.Attributes {
					if a.Name == "rack" && a.TextValue() == "rack-2" {
						return true
					}
				}
				return false
			},
			want: StoreWaitPlacement,
		},
		{
			name: "Step 4: single store RO fits on S0 and passes placement predicate",
			role: "",
			reqs: []StoreRequest{{Store: "tabicl-tables", Mode: StoreRO}},
			nodePred: func(o Offer) bool {
				return o.Hostname == "host-0"
			},
			want: StoreWaitNone,
		},
		{
			name: "Step 4: two stores and aggregated 2 writers on scratch fit together on S1",
			role: "*",
			reqs: []StoreRequest{
				{Store: "results", Mode: StoreRWExclusive},
				{Store: "scratch", Mode: StoreRW},
				{Store: "scratch", Mode: StoreRW},
			},
			want: StoreWaitNone,
		},
	}

	for _, c := range cases {
		got := v.ClassifyStoreWait(c.role, c.reqs, c.nodePred)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClusterView_CrossNodeModeSplitIsPlacementWait(t *testing.T) {
	v := NewClusterView()
	// Node S0 has tabicl-tables (RW: 4 readers, 1 writer) and results (RO-only: 4 readers, 0 writers).
	// Node S1 has tabicl-tables (RO-only: 4 readers, 0 writers) and results (RW: 4 readers, 1 writer).
	// Both stores are declared on both nodes, and each store has >= 1 writer on SOME node in the role,
	// so a task asking for tabicl-tables(RW) + results(RW) passes Step 1 and Step 2, and fails Step 3
	// with StoreWaitPlacement.
	v.ObserveOffers([]Offer{
		offerForNodeRole("S0", "host-0", "*", nil,
			storeResource("tabicl-tables", "4", "1"),
			storeResource("results", "4", "0")),
		offerForNodeRole("S1", "host-1", "*", nil,
			storeResource("tabicl-tables", "4", "0"),
			storeResource("results", "4", "1")),
	})

	reqs := []StoreRequest{
		{Store: "tabicl-tables", Mode: StoreRW},
		{Store: "results", Mode: StoreRW},
	}
	if got := v.ClassifyStoreWait("*", reqs, nil); got != StoreWaitPlacement {
		t.Fatalf("got %q, want %q", got, StoreWaitPlacement)
	}
}

func TestClusterView_RoleIsolationUpdateAndForgetNode(t *testing.T) {
	v := NewClusterView()
	// Observe S0 under both "*" and "gpu" roles with different store declarations.
	v.ObserveOffer(offerForNodeRole("S0", "host-0", "*", nil, storeResource("tabicl-tables", "4", "0")))
	v.ObserveOffer(offerForNodeRole("S0", "host-0", "gpu", nil, storeResource("checkpoints", "4", "1")))

	// In role "*", checkpoints is undeclared; in role "gpu", checkpoints is satisfied.
	if got := v.ClassifyStoreWait("*", []StoreRequest{{Store: "checkpoints", Mode: StoreRW}}, nil); got != StoreWaitUndeclared {
		t.Fatalf("role *: got %q, want %q", got, StoreWaitUndeclared)
	}
	if got := v.ClassifyStoreWait("gpu", []StoreRequest{{Store: "checkpoints", Mode: StoreRW}}, nil); got != StoreWaitNone {
		t.Fatalf("role gpu: got %q, want %q", got, StoreWaitNone)
	}

	// Updating S0's offer in role "*" to add a writer unit resolves StoreWaitMode -> StoreWaitNone.
	if got := v.ClassifyStoreWait("*", []StoreRequest{{Store: "tabicl-tables", Mode: StoreRW}}, nil); got != StoreWaitMode {
		t.Fatalf("before update: got %q, want %q", got, StoreWaitMode)
	}
	v.ObserveOffer(offerForNodeRole("S0", "host-0", "*", nil, storeResource("tabicl-tables", "4", "1")))
	if got := v.ClassifyStoreWait("*", []StoreRequest{{Store: "tabicl-tables", Mode: StoreRW}}, nil); got != StoreWaitNone {
		t.Fatalf("after update: got %q, want %q", got, StoreWaitNone)
	}

	// ForgetNode("S0") clears S0 across both "*" and "gpu" roles.
	v.ForgetNode("S0")
	if got := v.ClassifyStoreWait("*", []StoreRequest{{Store: "tabicl-tables", Mode: StoreRO}}, nil); got != StoreWaitNone {
		t.Fatalf("after ForgetNode (*): got %q, want %q", got, StoreWaitNone)
	}
	if got := v.ClassifyStoreWait("gpu", []StoreRequest{{Store: "checkpoints", Mode: StoreRW}}, nil); got != StoreWaitNone {
		t.Fatalf("after ForgetNode (gpu): got %q, want %q", got, StoreWaitNone)
	}
}

func TestClusterView_IsolatesCallerMutationsAfterObserveOffer(t *testing.T) {
	v := NewClusterView()
	orig := offerForNodeRole("S0", "host-0", "gpu",
		[]Attribute{textAttr("rack", "rack-1")},
		storeResource("results", "4", "1"))
	orig.URL = &URL{
		Scheme:  "http",
		Address: Address{IP: "10.0.0.1", Port: 5051},
		Query:   []Parameter{{Key: "k", Value: "v"}},
	}
	v.ObserveOffer(orig)

	// Mutate every nested pointer/slice on orig after ObserveOffer.
	orig.AllocationInfo.Role = "mutated-role"
	orig.URL.Scheme = "https"
	orig.URL.Query[0].Value = "mutated"
	orig.Attributes[0].Text.Value = "rack-mutated"
	for i := range orig.Resources[0].Metadata.Labels {
		if orig.Resources[0].Metadata.Labels[i].Key == "writers" {
			orig.Resources[0].Metadata.Labels[i].Value = "0"
		}
	}
	orig.Resources[0].Name = "store/corrupted"

	reqs := []StoreRequest{{Store: "results", Mode: StoreRW}}
	got := v.ClassifyStoreWait("gpu", reqs, func(o Offer) bool {
		return len(o.Attributes) == 1 && o.Attributes[0].TextValue() == "rack-1" &&
			o.URL != nil && o.URL.Scheme == "http" &&
			len(o.URL.Query) == 1 && o.URL.Query[0].Value == "v"
	})
	if got != StoreWaitNone {
		t.Fatalf("caller mutation leaked into ClusterView snapshot: got %q, want %q", got, StoreWaitNone)
	}
}

func TestClusterView_IsolatesNodePredCallbackMutations(t *testing.T) {
	v := NewClusterView()
	orig := offerForNodeRole("S0", "host-0", "*",
		[]Attribute{textAttr("rack", "rack-1")},
		storeResource("results", "4", "1"))
	orig.URL = &URL{
		Scheme:  "http",
		Address: Address{IP: "10.0.0.1", Port: 5051},
		Query:   []Parameter{{Key: "k", Value: "v"}},
	}
	v.ObserveOffer(orig)

	reqs := []StoreRequest{{Store: "results", Mode: StoreRW}}

	// First call passes a mutating nodePred callback.
	got1 := v.ClassifyStoreWait("*", reqs, func(o Offer) bool {
		o.Resources[0].Name = "corrupted"
		for i := range o.Resources[0].Metadata.Labels {
			o.Resources[0].Metadata.Labels[i].Value = "0"
		}
		o.Attributes[0].Text.Value = "corrupted"
		o.URL.Scheme = "https"
		o.URL.Query[0].Value = "corrupted"
		return true
	})
	if got1 != StoreWaitNone {
		t.Fatalf("first ClassifyStoreWait: got %q, want %q", got1, StoreWaitNone)
	}

	// Subsequent call must see the uncorrupted store, attributes, and URL.
	got2 := v.ClassifyStoreWait("*", reqs, func(o Offer) bool {
		return len(o.Attributes) == 1 && o.Attributes[0].TextValue() == "rack-1" &&
			o.URL != nil && o.URL.Scheme == "http" &&
			len(o.URL.Query) == 1 && o.URL.Query[0].Value == "v"
	})
	if got2 != StoreWaitNone {
		t.Fatalf("nodePred mutation corrupted stored snapshot: got %q, want %q", got2, StoreWaitNone)
	}
}

func TestClusterView_ConcurrentAccess(t *testing.T) {
	v := NewClusterView()
	o0 := offerForNodeRole("S0", "host-0", "*", nil, storeResource("tabicl-tables", "4", "1"))
	o1 := offerForNodeRole("S1", "host-1", "gpu", nil, storeResource("results", "4", "0"))
	reqs := []StoreRequest{{Store: "tabicl-tables", Mode: StoreRO}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				v.ObserveOffer(o0)
				v.ObserveOffer(o1)
				_ = v.ClassifyStoreWait("*", reqs, func(o Offer) bool { return o.Hostname != "" })
				if j%25 == 0 {
					v.ForgetNode("S1")
				}
			}
		}(i)
	}
	wg.Wait()
}
