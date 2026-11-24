package medios

import (
	"encoding/json"
	"strings"
	"testing"
)

// A resource's identity is more than its quantity. The controller compares it
// by equality, so anything the offer carried has to survive a decode/encode
// round trip even when this library does not model it (medios#432).

const offeredGPU = `{"name":"gpus","type":"SCALAR","scalar":{"value":4},` +
	`"metadata":{"labels":[{"key":"class","value":"gb10"}]},` +
	`"reservations":[{"type":"DYNAMIC","role":"fennel"}],` +
	`"allocation_info":{"role":"fennel"},"provider_id":{"value":"rp-1"}}`

func TestResource_RoundTripPreservesUnmodeledFields(t *testing.T) {
	var r Resource
	if err := json.Unmarshal([]byte(offeredGPU), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Name != "gpus" || r.Scalar == nil || r.Scalar.Value != 4 {
		t.Fatalf("modeled fields wrong: %+v", r)
	}
	if got, ok := r.Metadata.Get("class"); !ok || got != "gb10" {
		t.Fatalf("metadata class = %q (present=%v), want gb10", got, ok)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"reservations":[{"type":"DYNAMIC","role":"fennel"}]`,
		`"allocation_info":{"role":"fennel"}`,
		`"provider_id":{"value":"rp-1"}`,
		`"metadata":{"labels":[{"key":"class","value":"gb10"}]}`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("round trip dropped %s\ngot %s", want, out)
		}
	}
}

// A plain resource encodes exactly as before, so existing consumers are
// unaffected.
func TestResource_PlainEncodingUnchanged(t *testing.T) {
	r := Resource{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 2}}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(out), `{"name":"cpus","type":"SCALAR","scalar":{"value":2}}`; got != want {
		t.Fatalf("encoding = %s, want %s", got, want)
	}
}

// Carve takes a share of the quantity and keeps every bit of identity: this
// is what a framework puts in TaskInfo.Resources.
func TestResource_CarveKeepsIdentity(t *testing.T) {
	var offered Resource
	if err := json.Unmarshal([]byte(offeredGPU), &offered); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := offered.Carve(1)
	if !ok {
		t.Fatal("Carve(1) of a 4-gpu resource should succeed")
	}
	if got.Scalar.Value != 1 {
		t.Fatalf("carved quantity = %v, want 1", got.Scalar.Value)
	}
	if cls, _ := got.Metadata.Get("class"); cls != "gb10" {
		t.Fatalf("carved resource lost its class: %+v", got.Metadata)
	}
	out, _ := json.Marshal(got)
	if !strings.Contains(string(out), `"provider_id":{"value":"rp-1"}`) {
		t.Errorf("carved resource lost unmodeled identity: %s", out)
	}
	// The offered resource is untouched.
	if offered.Scalar.Value != 4 {
		t.Errorf("Carve mutated the offer: %v", offered.Scalar.Value)
	}
}

func TestResource_CarveRefusesMoreThanOffered(t *testing.T) {
	r := Resource{Name: "gpus", Type: "SCALAR", Scalar: &Scalar{Value: 1}}
	if _, ok := r.Carve(2); ok {
		t.Fatal("carving more than the resource holds must fail")
	}
	if _, ok := r.Carve(-1); ok {
		t.Fatal("carving a negative quantity must fail")
	}
}

// RANGES are not divisible by a number; a share is chosen by picking ranges.
func TestResource_CarveNonScalarUnchanged(t *testing.T) {
	r := NewPortsResource(31000, 31001)
	got, ok := r.Carve(1)
	if ok {
		t.Fatal("a RANGES resource is not carvable by quantity")
	}
	if got.Ranges == nil || len(got.Ranges.Range) != 2 {
		t.Fatalf("non-scalar Carve should return the resource unchanged, got %+v", got)
	}
}

func TestFindResource(t *testing.T) {
	var gpu Resource
	_ = json.Unmarshal([]byte(offeredGPU), &gpu)
	o := Offer{Resources: []Resource{
		{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 8}},
		gpu,
	}}
	got, ok := FindResource(o, "gpus")
	if !ok {
		t.Fatal("gpus not found in offer")
	}
	if cls, _ := got.Metadata.Get("class"); cls != "gb10" {
		t.Fatalf("FindResource lost metadata: %+v", got)
	}
	if _, ok := FindResource(o, "disk"); ok {
		t.Fatal("absent resource reported present")
	}
}

// An offer decoded and re-encoded whole keeps every resource's identity.
func TestOffer_RoundTripPreservesResourceIdentity(t *testing.T) {
	in := `{"id":{"value":"O-1"},"node_id":{"value":"S1"},"hostname":"host-0",` +
		`"resources":[` + offeredGPU + `]}`
	var o Offer
	if err := json.Unmarshal([]byte(in), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"allocation_info":{"role":"fennel"}`) {
		t.Errorf("offer round trip dropped resource identity: %s", out)
	}
}

// The controller sets allocation_info on the offer and on each of its
// resources for a MULTI_ROLE framework; both decode, and a carved share keeps
// the resource's.
func TestOffer_AllocationInfoDecodes(t *testing.T) {
	in := `{"id":{"value":"O-1"},"node_id":{"value":"S1"},"hostname":"n1",` +
		`"allocation_info":{"role":"pool/batch"},"resources":[` +
		`{"name":"exclusive_slot","type":"SCALAR","scalar":{"value":1},` +
		`"role":"pool","reservations":[{"type":"STATIC","role":"pool"}],` +
		`"allocation_info":{"role":"pool/batch"}},` +
		`{"name":"cpus","type":"SCALAR","scalar":{"value":2}}]}`
	var o Offer
	if err := json.Unmarshal([]byte(in), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if o.AllocationInfo == nil || o.AllocationInfo.Role != "pool/batch" {
		t.Fatalf("offer allocation_info = %+v", o.AllocationInfo)
	}
	r := o.Resources[0]
	if r.AllocationInfo == nil || r.AllocationInfo.Role != "pool/batch" {
		t.Fatalf("resource allocation_info = %+v", r.AllocationInfo)
	}
	if o.Resources[1].AllocationInfo != nil {
		t.Errorf("resource without allocation_info decoded one: %+v", o.Resources[1].AllocationInfo)
	}
	share, ok := r.Carve(1)
	if !ok {
		t.Fatal("carve refused")
	}
	out, err := json.Marshal(share)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"allocation_info":{"role":"pool/batch"}`,
		`"reservations":[{"type":"STATIC","role":"pool"}]`,
		`"role":"pool"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("carved share dropped %s: %s", want, out)
		}
	}
	if n := strings.Count(string(out), `"allocation_info"`); n != 1 {
		t.Errorf("allocation_info encoded %d times: %s", n, out)
	}
}
