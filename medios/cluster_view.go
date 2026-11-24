package medios

import (
	"encoding/json"
	"sync"
)

// StoreRequest is one store volume a task asks to mount, paired with the
// access mode it needs (StoreRO, StoreRW or StoreRWExclusive).
type StoreRequest struct {
	Store string
	Mode  StoreMode
}

// StoreWaitReason explains why a pending task's store requests cannot be
// satisfied by the cluster supply observed in a role. It is empty when at
// least one observed node in the role can satisfy every store request and
// pass the caller's placement predicate, or when no offers have been
// observed for the role yet.
type StoreWaitReason string

// Store wait reason codes.
const (
	StoreWaitNone       StoreWaitReason = ""
	StoreWaitUndeclared StoreWaitReason = "waiting_store"
	StoreWaitMode       StoreWaitReason = "waiting_store_mode"
	StoreWaitPlacement  StoreWaitReason = "waiting_store_placement"
)

type nodeRoleKey struct {
	nodeID string
	role   string
}

// ClusterView remembers the latest offer observed per (node, role) so a
// scheduler can distinguish a store that no node in the role declares
// (StoreWaitUndeclared), a store declared without enough reader or writer
// units for the requested mode (StoreWaitMode), and a set of stores or
// placement constraints that no single node in the role satisfies together
// (StoreWaitPlacement).
//
// It is safe for concurrent use by multiple goroutines.
type ClusterView struct {
	mu     sync.RWMutex
	offers map[nodeRoleKey]Offer
}

// NewClusterView returns an empty ClusterView.
func NewClusterView() *ClusterView {
	return &ClusterView{
		offers: make(map[nodeRoleKey]Offer),
	}
}

func offerRole(o Offer) string {
	if o.AllocationInfo != nil && o.AllocationInfo.Role != "" {
		return o.AllocationInfo.Role
	}
	return "*"
}

func normalizeRole(role string) string {
	if role == "" {
		return "*"
	}
	return role
}

func cloneResourceForView(r Resource) Resource {
	cp := r
	if r.Scalar != nil {
		sc := *r.Scalar
		cp.Scalar = &sc
	}
	if r.Ranges != nil {
		rg := *r.Ranges
		if len(r.Ranges.Range) > 0 {
			rg.Range = append([]Range(nil), r.Ranges.Range...)
		}
		cp.Ranges = &rg
	}
	if r.Metadata != nil {
		md := *r.Metadata
		if len(r.Metadata.Labels) > 0 {
			md.Labels = append([]Label(nil), r.Metadata.Labels...)
		}
		cp.Metadata = &md
	}
	if r.AllocationInfo != nil {
		ai := *r.AllocationInfo
		cp.AllocationInfo = &ai
	}
	if len(r.extra) > 0 {
		ex := make(map[string]json.RawMessage, len(r.extra))
		for k, v := range r.extra {
			ex[k] = append(json.RawMessage(nil), v...)
		}
		cp.extra = ex
	}
	return cp
}

func cloneAttributeForView(a Attribute) Attribute {
	cp := a
	if a.Text != nil {
		tx := *a.Text
		cp.Text = &tx
	}
	return cp
}

func cloneOfferForView(o Offer) Offer {
	cp := o
	if o.AllocationInfo != nil {
		ai := *o.AllocationInfo
		cp.AllocationInfo = &ai
	}
	if o.URL != nil {
		u := *o.URL
		if len(o.URL.Query) > 0 {
			u.Query = append([]Parameter(nil), o.URL.Query...)
		}
		cp.URL = &u
	}
	if len(o.Resources) > 0 {
		cp.Resources = make([]Resource, len(o.Resources))
		for i, r := range o.Resources {
			cp.Resources[i] = cloneResourceForView(r)
		}
	}
	if len(o.Attributes) > 0 {
		cp.Attributes = make([]Attribute, len(o.Attributes))
		for i, a := range o.Attributes {
			cp.Attributes[i] = cloneAttributeForView(a)
		}
	}
	return cp
}

// ObserveOffer records the latest offer seen for (o.NodeID.Value, role),
// where role is o.AllocationInfo.Role or "*" when unset. An offer with an
// empty NodeID is ignored.
func (v *ClusterView) ObserveOffer(o Offer) {
	if v == nil || o.NodeID.Value == "" {
		return
	}
	key := nodeRoleKey{
		nodeID: o.NodeID.Value,
		role:   offerRole(o),
	}
	cp := cloneOfferForView(o)
	v.mu.Lock()
	if v.offers == nil {
		v.offers = make(map[nodeRoleKey]Offer)
	}
	v.offers[key] = cp
	v.mu.Unlock()
}

// ObserveOffers records every offer in the slice.
func (v *ClusterView) ObserveOffers(offers []Offer) {
	for _, o := range offers {
		v.ObserveOffer(o)
	}
}

// ForgetNode removes all recorded offers for nodeID across every role.
func (v *ClusterView) ForgetNode(nodeID string) {
	if v == nil || nodeID == "" {
		return
	}
	v.mu.Lock()
	for k := range v.offers {
		if k.nodeID == nodeID {
			delete(v.offers, k)
		}
	}
	v.mu.Unlock()
}

type storeDemand struct {
	name      string
	readers   int
	writers   int
	exclusive bool
	invalid   bool
}

func (d storeDemand) satisfiedBy(info StoreInfo) bool {
	if d.invalid {
		return false
	}
	if d.readers > info.Readers {
		return false
	}
	if d.writers > info.Writers {
		return false
	}
	if d.exclusive && (info.Writers <= 0 || d.writers > 1) {
		return false
	}
	return true
}

// ClassifyStoreWait classifies why reqs cannot be placed on the observed
// nodes in role (where an empty role is normalized to "*").
//
// If reqs is empty, or no offers have been observed for role yet, it
// returns StoreWaitNone. Otherwise it evaluates:
//  1. StoreWaitUndeclared ("waiting_store"): at least one requested store
//     is not declared on any observed node in role.
//  2. StoreWaitMode ("waiting_store_mode"): every requested store is
//     declared on at least one node in role, but for some requested store
//     no node in role declares enough reader/writer units for the aggregated
//     modes requested on that store.
//  3. StoreWaitPlacement ("waiting_store_placement"): every requested store
//     individually has a node in role with sufficient mode units, but no
//     single node in role satisfies all requested stores simultaneously and
//     passes nodePred (when nodePred is non-nil).
//  4. StoreWaitNone (""): at least one observed node in role satisfies all
//     requested stores and passes nodePred.
func (v *ClusterView) ClassifyStoreWait(role string, reqs []StoreRequest, nodePred func(Offer) bool) StoreWaitReason {
	if v == nil || len(reqs) == 0 {
		return StoreWaitNone
	}
	normRole := normalizeRole(role)

	v.mu.RLock()
	var roleOffers []Offer
	for k, o := range v.offers {
		if k.role == normRole {
			roleOffers = append(roleOffers, o)
		}
	}
	v.mu.RUnlock()

	if len(roleOffers) == 0 {
		return StoreWaitNone
	}

	// Aggregate multiple requests for the same store name in deterministic order.
	var demands []storeDemand
	byName := make(map[string]int, len(reqs))
	for _, req := range reqs {
		idx, exists := byName[req.Store]
		if !exists {
			idx = len(demands)
			byName[req.Store] = idx
			demands = append(demands, storeDemand{name: req.Store})
		}
		d := &demands[idx]
		switch req.Mode {
		case StoreRO:
			d.readers++
		case StoreRW:
			d.writers++
		case StoreRWExclusive:
			d.writers++
			d.exclusive = true
		default:
			d.invalid = true
		}
	}

	// Step 1: Check whether every requested store is declared on >= 1 node in role.
	for _, d := range demands {
		declaredAnywhere := false
		for _, o := range roleOffers {
			if _, ok := FindStore(o, d.name); ok {
				declaredAnywhere = true
				break
			}
		}
		if !declaredAnywhere {
			return StoreWaitUndeclared
		}
	}

	// Step 2: Check whether every requested store has >= 1 node in role with
	// sufficient declared reader/writer capacity for its aggregated mode demand.
	for _, d := range demands {
		modeFitsAnywhere := false
		for _, o := range roleOffers {
			if info, ok := FindStore(o, d.name); ok && d.satisfiedBy(info) {
				modeFitsAnywhere = true
				break
			}
		}
		if !modeFitsAnywhere {
			return StoreWaitMode
		}
	}

	// Step 3: Check whether at least one single node in role satisfies all
	// requested stores simultaneously AND passes nodePred.
	for _, o := range roleOffers {
		allFit := true
		for _, d := range demands {
			info, ok := FindStore(o, d.name)
			if !ok || !d.satisfiedBy(info) {
				allFit = false
				break
			}
		}
		if !allFit {
			continue
		}
		if nodePred != nil && !nodePred(cloneOfferForView(o)) {
			continue
		}
		return StoreWaitNone
	}

	return StoreWaitPlacement
}
