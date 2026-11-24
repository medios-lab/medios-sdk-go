package medios

import (
	"sort"
	"strings"
)

// scalarEpsilon is the floating-point tolerance used when comparing or carving
// SCALAR resource quantities across multiple role/reservation copies in an offer.
const scalarEpsilon = 1e-9

// ScalarTotal returns the sum of all SCALAR resources named name in the offer.
// A single resource name may appear multiple times in an offer when a node's
// capacity is split across unreserved and role-reserved slices (medios#432).
func (o Offer) ScalarTotal(name string) float64 {
	var total float64
	for _, r := range o.Resources {
		if r.Name == name && r.Type == "SCALAR" && r.Scalar != nil && r.Scalar.Value > 0 {
			total += r.Scalar.Value
		}
	}
	return total
}

// CarveScalar carves want units of the named SCALAR resource across one or more
// same-named entries in o.Resources in the order the controller sent them.
//
// Each contributing entry is carved via Resource.Carve so its Metadata,
// AllocationInfo, and unmodeled extra identity fields (role, reservations,
// provider_id) survive verbatim on both the carved task resources and the
// remaining offer resources (medios#432).
//
// If want < 0 or the offer holds less than want - 1e-9 in total, CarveScalar
// returns (nil, o, false). If want == 0, it returns (nil, o, true). Scalar
// entries whose remaining quantity drops to <= 1e-9 are omitted from the
// returned remaining offer.
func (o Offer) CarveScalar(name string, want float64) (carved []Resource, remaining Offer, ok bool) {
	if want < 0 {
		return nil, o, false
	}
	if want == 0 {
		return nil, o, true
	}
	if o.ScalarTotal(name) < want-scalarEpsilon {
		return nil, o, false
	}

	need := want
	remResources := make([]Resource, 0, len(o.Resources))

	for _, r := range o.Resources {
		if need <= scalarEpsilon || r.Name != name || r.Type != "SCALAR" || r.Scalar == nil || r.Scalar.Value <= scalarEpsilon {
			remResources = append(remResources, r)
			continue
		}

		avail := r.Scalar.Value
		if avail <= need+scalarEpsilon {
			// Consume this entire resource slice.
			chunk, carvedOK := r.Carve(avail)
			if !carvedOK {
				return nil, o, false
			}
			carved = append(carved, chunk)
			need -= avail
			if need < 0 {
				need = 0
			}
			continue
		}

		// Split this resource slice into the needed chunk and the remainder.
		chunk, carvedOK := r.Carve(need)
		if !carvedOK {
			return nil, o, false
		}
		carved = append(carved, chunk)

		left := avail - need
		if left > scalarEpsilon {
			remChunk, remOK := r.Carve(left)
			if !remOK {
				return nil, o, false
			}
			remResources = append(remResources, remChunk)
		}
		need = 0
	}

	if need > scalarEpsilon {
		return nil, o, false
	}

	remOffer := o
	remOffer.Resources = remResources
	return carved, remOffer, true
}

// CarveScalars carves multiple named SCALAR quantities from the offer in
// deterministic sorted key order. If any requested resource does not fit, it
// returns (nil, o, false) with the original offer unchanged.
func (o Offer) CarveScalars(wants map[string]float64) (carved []Resource, remaining Offer, ok bool) {
	if len(wants) == 0 {
		return nil, o, true
	}
	names := make([]string, 0, len(wants))
	for name := range wants {
		names = append(names, name)
	}
	sort.Strings(names)

	cur := o
	var allCarved []Resource
	for _, name := range names {
		want := wants[name]
		part, next, fit := cur.CarveScalar(name, want)
		if !fit {
			return nil, o, false
		}
		allCarved = append(allCarved, part...)
		cur = next
	}
	return allCarved, cur, true
}

// AttributeText returns the string value of the first TEXT attribute named name
// on the offer, or ("", false) when absent or not TEXT-typed.
func (o Offer) AttributeText(name string) (string, bool) {
	for _, a := range o.Attributes {
		if a.Name == name && a.Type == "TEXT" && a.Text != nil {
			return a.TextValue(), true
		}
	}
	return "", false
}

// MatchesAttributes reports whether every key/value pair in want matches a TEXT
// attribute on the offer. An empty or nil want map matches any offer.
func (o Offer) MatchesAttributes(want map[string]string) bool {
	for k, wantVal := range want {
		gotVal, ok := o.AttributeText(k)
		if !ok || gotVal != wantVal {
			return false
		}
	}
	return true
}

// SupportsContainer reports whether the offering node can run a task with the
// given image and finish-hook requirements, returning the ContainerInfo.Type to
// build (ContainerMedios, ContainerDocker, or "" for a bare command).
//
// Rules:
//   - When !hasImage (bare command): matches only when the node's
//     "containerizer" attribute is "medios", returning ("", true); a
//     "docker_shell" node refuses bare commands with NOT_SUPPORTED.
//   - When hasImage && hasFinish: requires containerizer == "medios" and
//     "docker" in the comma-separated "images" attribute (DR-042: the docker
//     executor does not run finish hooks), returning (ContainerMedios, true).
//   - When hasImage && !hasFinish: on containerizer == "docker_shell" returns
//     (ContainerDocker, true); on containerizer == "medios" with "docker" in
//     "images" returns (ContainerMedios, true); otherwise ("", false).
func (o Offer) SupportsContainer(hasImage, hasFinish bool) (containerType string, ok bool) {
	cz, _ := o.AttributeText("containerizer")
	if !hasImage {
		if cz == "medios" {
			return "", true
		}
		return "", false
	}

	if hasFinish {
		if cz == "medios" && o.hasDockerImageCapability() {
			return ContainerMedios, true
		}
		return "", false
	}

	switch cz {
	case "docker_shell":
		return ContainerDocker, true
	case "medios":
		if o.hasDockerImageCapability() {
			return ContainerMedios, true
		}
		return "", false
	default:
		return "", false
	}
}

func (o Offer) hasDockerImageCapability() bool {
	raw, ok := o.AttributeText("images")
	if !ok || raw == "" {
		return false
	}
	for _, tok := range strings.Split(raw, ",") {
		if strings.TrimSpace(tok) == "docker" {
			return true
		}
	}
	return false
}
