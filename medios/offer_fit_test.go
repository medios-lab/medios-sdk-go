package medios

import (
	"encoding/json"
	"math"
	"testing"
)

func mustDecodeOffer(t *testing.T, raw string) Offer {
	t.Helper()
	var o Offer
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("unmarshal offer: %v", err)
	}
	return o
}

func TestOfferScalarTotalAndMultiCopyCarveScalar(t *testing.T) {
	// Offer with cpus split across a reserved slice (1.5) and unreserved slice (2.5),
	// each carrying distinct metadata / unmodeled reservation fields.
	const raw = `{
		"id": {"value": "offer-1"},
		"framework_id": {"value": "fw-1"},
		"node_id": {"value": "node-1"},
		"hostname": "gpu-node-1",
		"allocation_info": {"role": "team/gpu"},
		"resources": [
			{
				"name": "cpus",
				"type": "SCALAR",
				"scalar": {"value": 1.5},
				"allocation_info": {"role": "team/gpu"},
				"role": "team/gpu",
				"reservations": [{"type": "STATIC", "role": "team/gpu"}],
				"metadata": {"labels": [{"key": "tier", "value": "reserved"}]}
			},
			{
				"name": "mem",
				"type": "SCALAR",
				"scalar": {"value": 4096.0},
				"allocation_info": {"role": "team/gpu"}
			},
			{
				"name": "cpus",
				"type": "SCALAR",
				"scalar": {"value": 2.5},
				"allocation_info": {"role": "*"},
				"role": "*",
				"metadata": {"labels": [{"key": "tier", "value": "unreserved"}]}
			}
		]
	}`
	offer := mustDecodeOffer(t, raw)

	if got := offer.ScalarTotal("cpus"); math.Abs(got-4.0) > scalarEpsilon {
		t.Fatalf("ScalarTotal(cpus) = %v, want 4.0", got)
	}
	if got := offer.ScalarTotal("gpus"); got != 0 {
		t.Fatalf("ScalarTotal(gpus) = %v, want 0", got)
	}

	// Carve 3.0 cpus across the two slices (1.5 from first, 1.5 from second, leaving 1.0 in second).
	carved, rem, ok := offer.CarveScalar("cpus", 3.0)
	if !ok {
		t.Fatal("CarveScalar(cpus, 3.0) failed, want ok=true")
	}
	if len(carved) != 2 {
		t.Fatalf("len(carved) = %d, want 2 slices", len(carved))
	}
	if math.Abs(carved[0].Scalar.Value-1.5) > scalarEpsilon || math.Abs(carved[1].Scalar.Value-1.5) > scalarEpsilon {
		t.Fatalf("carved values = [%v, %v], want [1.5, 1.5]", carved[0].Scalar.Value, carved[1].Scalar.Value)
	}

	// Verify metadata and unmodeled extra fields ("reservations", "role") survived on both carved chunks.
	if v, _ := carved[0].Metadata.Get("tier"); v != "reserved" {
		t.Errorf("carved[0] metadata tier = %q, want reserved", v)
	}
	if v, _ := carved[1].Metadata.Get("tier"); v != "unreserved" {
		t.Errorf("carved[1] metadata tier = %q, want unreserved", v)
	}
	b0, err := json.Marshal(carved[0])
	if err != nil {
		t.Fatalf("marshal carved[0]: %v", err)
	}
	var m0 map[string]any
	if err := json.Unmarshal(b0, &m0); err != nil {
		t.Fatalf("unmarshal carved[0]: %v", err)
	}
	if m0["role"] != "team/gpu" || m0["reservations"] == nil {
		t.Errorf("carved[0] lost unmodeled identity fields: %s", string(b0))
	}

	// Verify remaining offer has 1.0 cpus (with unreserved metadata) and 4096 mem.
	if got := rem.ScalarTotal("cpus"); math.Abs(got-1.0) > scalarEpsilon {
		t.Fatalf("remaining ScalarTotal(cpus) = %v, want 1.0", got)
	}
	if got := rem.ScalarTotal("mem"); math.Abs(got-4096.0) > scalarEpsilon {
		t.Fatalf("remaining ScalarTotal(mem) = %v, want 4096.0", got)
	}
	if len(rem.Resources) != 2 {
		t.Fatalf("len(rem.Resources) = %d, want 2 (exhausted first cpus slice dropped)", len(rem.Resources))
	}
}

func TestOfferCarveScalarFloatToleranceAndEdgeCases(t *testing.T) {
	const raw = `{
		"id": {"value": "offer-tol"},
		"node_id": {"value": "node-1"},
		"resources": [
			{"name": "cpus", "type": "SCALAR", "scalar": {"value": 0.1}},
			{"name": "cpus", "type": "SCALAR", "scalar": {"value": 0.2}}
		]
	}`
	offer := mustDecodeOffer(t, raw)

	// 0.1 + 0.2 in IEEE-754 float64; carving 0.3 must succeed and leave 0 cpus slices.
	carved, rem, ok := offer.CarveScalar("cpus", 0.3)
	if !ok {
		t.Fatal("CarveScalar(cpus, 0.3) across 0.1+0.2 failed")
	}
	if len(carved) != 2 {
		t.Fatalf("len(carved) = %d, want 2", len(carved))
	}
	if len(rem.Resources) != 0 {
		t.Fatalf("expected exhausted slices to be dropped, got %d resources", len(rem.Resources))
	}

	// want == 0 succeeds as a no-op.
	cZero, remZero, okZero := offer.CarveScalar("cpus", 0)
	if !okZero || cZero != nil || len(remZero.Resources) != 2 {
		t.Fatalf("CarveScalar(0) = (%v, %d, %v), want (nil, 2, true)", cZero, len(remZero.Resources), okZero)
	}

	// want < 0 or want > total fails and leaves original offer untouched.
	if _, _, okNeg := offer.CarveScalar("cpus", -1.0); okNeg {
		t.Fatal("CarveScalar(-1.0) succeeded, want false")
	}
	if _, _, okOver := offer.CarveScalar("cpus", 0.31); okOver {
		t.Fatal("CarveScalar(0.31) succeeded on 0.3 total, want false")
	}
}

func TestOfferCarveScalarsDeterministicAndAtomic(t *testing.T) {
	const raw = `{
		"id": {"value": "offer-multi"},
		"node_id": {"value": "node-1"},
		"resources": [
			{"name": "cpus", "type": "SCALAR", "scalar": {"value": 4.0}},
			{"name": "mem",  "type": "SCALAR", "scalar": {"value": 2048.0}},
			{"name": "gpus", "type": "SCALAR", "scalar": {"value": 2.0}}
		]
	}`
	offer := mustDecodeOffer(t, raw)

	carved, rem, ok := offer.CarveScalars(map[string]float64{
		"mem":  512,
		"cpus": 1.5,
		"gpus": 1,
	})
	if !ok {
		t.Fatal("CarveScalars failed, want true")
	}
	// Sorted key order: cpus, gpus, mem.
	if len(carved) != 3 || carved[0].Name != "cpus" || carved[1].Name != "gpus" || carved[2].Name != "mem" {
		t.Fatalf("CarveScalars order = %+v, want [cpus, gpus, mem]", carved)
	}
	if rem.ScalarTotal("cpus") != 2.5 || rem.ScalarTotal("gpus") != 1.0 || rem.ScalarTotal("mem") != 1536.0 {
		t.Fatalf("unexpected remaining totals: cpus=%v gpus=%v mem=%v",
			rem.ScalarTotal("cpus"), rem.ScalarTotal("gpus"), rem.ScalarTotal("mem"))
	}

	// If one resource in the map fails to fit, the original offer is returned unmodified.
	_, remFail, okFail := offer.CarveScalars(map[string]float64{
		"cpus": 1.0,
		"gpus": 3.0, // exceeds 2.0
	})
	if okFail {
		t.Fatal("CarveScalars with insufficient gpus succeeded, want false")
	}
	if remFail.ScalarTotal("cpus") != 4.0 {
		t.Fatalf("failed CarveScalars mutated remaining offer cpus: got %v, want 4.0", remFail.ScalarTotal("cpus"))
	}
}

func TestOfferAttributesAndSupportsContainer(t *testing.T) {
	mediosImgOffer := Offer{
		Attributes: []Attribute{
			{Name: "containerizer", Type: "TEXT", Text: &AttributeText{Value: "medios"}},
			{Name: "images", Type: "TEXT", Text: &AttributeText{Value: "oci, docker"}},
			{Name: "zone", Type: "TEXT", Text: &AttributeText{Value: "us-west"}},
		},
	}
	mediosBareOnlyOffer := Offer{
		Attributes: []Attribute{
			{Name: "containerizer", Type: "TEXT", Text: &AttributeText{Value: "medios"}},
			{Name: "zone", Type: "TEXT", Text: &AttributeText{Value: "us-east"}},
		},
	}
	dockerShellOffer := Offer{
		Attributes: []Attribute{
			{Name: "containerizer", Type: "TEXT", Text: &AttributeText{Value: "docker_shell"}},
			{Name: "images", Type: "TEXT", Text: &AttributeText{Value: "docker"}},
		},
	}
	noAttrOffer := Offer{}

	// AttributeText & MatchesAttributes
	if v, ok := mediosImgOffer.AttributeText("zone"); !ok || v != "us-west" {
		t.Fatalf("AttributeText(zone) = (%q, %v), want (us-west, true)", v, ok)
	}
	if _, ok := mediosImgOffer.AttributeText("missing"); ok {
		t.Fatal("AttributeText(missing) returned true")
	}
	if !mediosImgOffer.MatchesAttributes(map[string]string{"zone": "us-west", "containerizer": "medios"}) {
		t.Fatal("MatchesAttributes returned false on matching map")
	}
	if mediosImgOffer.MatchesAttributes(map[string]string{"zone": "us-east"}) {
		t.Fatal("MatchesAttributes returned true on mismatched value")
	}
	if !noAttrOffer.MatchesAttributes(nil) {
		t.Fatal("MatchesAttributes(nil) should return true")
	}

	// SupportsContainer: bare command (!hasImage) is only supported on containerizer == "medios".
	for name, off := range map[string]Offer{
		"mediosImg":      mediosImgOffer,
		"mediosBareOnly": mediosBareOnlyOffer,
	} {
		ct, ok := off.SupportsContainer(false, false)
		if !ok || ct != "" {
			t.Errorf("%s SupportsContainer(false, false) = (%q, %v), want (\"\", true)", name, ct, ok)
		}
		ctFin, okFin := off.SupportsContainer(false, true)
		if !okFin || ctFin != "" {
			t.Errorf("%s SupportsContainer(false, true) = (%q, %v), want (\"\", true)", name, ctFin, okFin)
		}
	}
	for name, off := range map[string]Offer{
		"dockerShell": dockerShellOffer,
		"noAttr":      noAttrOffer,
	} {
		if ct, ok := off.SupportsContainer(false, false); ok || ct != "" {
			t.Errorf("%s SupportsContainer(false, false) = (%q, %v), want (\"\", false)", name, ct, ok)
		}
		if ct, ok := off.SupportsContainer(false, true); ok || ct != "" {
			t.Errorf("%s SupportsContainer(false, true) = (%q, %v), want (\"\", false)", name, ct, ok)
		}
	}

	// SupportsContainer: image without finish (hasImage && !hasFinish)
	if ct, ok := mediosImgOffer.SupportsContainer(true, false); !ok || ct != ContainerMedios {
		t.Errorf("mediosImg SupportsContainer(true, false) = (%q, %v), want (%q, true)", ct, ok, ContainerMedios)
	}
	if ct, ok := dockerShellOffer.SupportsContainer(true, false); !ok || ct != ContainerDocker {
		t.Errorf("dockerShell SupportsContainer(true, false) = (%q, %v), want (%q, true)", ct, ok, ContainerDocker)
	}
	if _, ok := mediosBareOnlyOffer.SupportsContainer(true, false); ok {
		t.Error("mediosBareOnly (no images:docker) SupportsContainer(true, false) = true, want false")
	}
	if _, ok := noAttrOffer.SupportsContainer(true, false); ok {
		t.Error("noAttr SupportsContainer(true, false) = true, want false")
	}

	// SupportsContainer: image with finish (hasImage && hasFinish, DR-042)
	if ct, ok := mediosImgOffer.SupportsContainer(true, true); !ok || ct != ContainerMedios {
		t.Errorf("mediosImg SupportsContainer(true, true) = (%q, %v), want (%q, true)", ct, ok, ContainerMedios)
	}
	if _, ok := dockerShellOffer.SupportsContainer(true, true); ok {
		t.Error("dockerShell SupportsContainer(true, true) = true, want false (DR-042)")
	}
	if _, ok := mediosBareOnlyOffer.SupportsContainer(true, true); ok {
		t.Error("mediosBareOnly SupportsContainer(true, true) = true, want false")
	}
}
