package medios

import (
	"fmt"
	"strconv"
)

// How a node advertises a store in an offer, per DR-036 (medios#529): one
// shared SCALAR resource named store/<name> with value 1, and metadata
// labels class, locality, readers and writers. The counts are the node's
// declaration about the disk, not free units: the controller keeps the
// per-mode holder tally and refuses a launch beyond either count. A task
// that mounts a store lists one copy of the resource, which is what puts
// the holder on the controller's ledger.
const (
	StoreResourcePrefix = "store/"
	storeClassLabel     = "class"
	storeLocalityLabel  = "locality"
	storeReadersLabel   = "readers"
	storeWritersLabel   = "writers"
)

// StoreInfo is a store as a node declares it in an offer.
type StoreInfo struct {
	Class    string // local | shared | object
	Locality string // near | same-site | remote
	Readers  int    // concurrent RO holders the node declares
	Writers  int    // concurrent RW holders the node declares; 0 = reads only
}

// FindStore returns the named store's declaration from an offer, or false
// when the offer does not carry the store.
func FindStore(o Offer, name string) (StoreInfo, bool) {
	r, ok := FindResource(o, StoreResourcePrefix+name)
	if !ok {
		return StoreInfo{}, false
	}
	return storeInfo(r), true
}

func storeInfo(r Resource) StoreInfo {
	var s StoreInfo
	if r.Metadata == nil {
		return s
	}
	s.Class, _ = r.Metadata.Get(storeClassLabel)
	s.Locality, _ = r.Metadata.Get(storeLocalityLabel)
	if v, ok := r.Metadata.Get(storeReadersLabel); ok {
		s.Readers, _ = strconv.Atoi(v)
	}
	if v, ok := r.Metadata.Get(storeWritersLabel); ok {
		s.Writers, _ = strconv.Atoi(v)
	}
	return s
}

// FitsStores reports whether the offer carries every store the task's
// volumes name, with a declared unit for each mode: readers above zero for
// RO, writers above zero for RW, and two volumes on one store counted
// together against the declaration. Whether a unit is free on the node is
// the controller's tally and is decided at launch, not here. The reason
// names the first volume that does not fit, in words a framework can show
// its caller; it is empty when the task fits.
func FitsStores(o Offer, t TaskInfo) (bool, string) {
	if t.Container == nil {
		return true, ""
	}
	asked := map[string]*StoreInfo{}
	for _, v := range t.Container.Volumes {
		if v.Source == nil || v.Source.Type != VolumeSourceStore || v.Source.Store == nil {
			return false, fmt.Sprintf("volume %s has no store source", v.ContainerPath)
		}
		name := v.Source.Store.Name
		declared, ok := FindStore(o, name)
		if !ok {
			return false, fmt.Sprintf("store %s is not on node %s", name, o.Hostname)
		}
		n := asked[name]
		if n == nil {
			n = &StoreInfo{}
			asked[name] = n
		}
		switch v.Mode {
		case VolumeRO:
			n.Readers++
			if n.Readers > declared.Readers {
				return false, fmt.Sprintf("store %s declares %d reader units on node %s, task asks %d",
					name, declared.Readers, o.Hostname, n.Readers)
			}
		default:
			n.Writers++
			if n.Writers > declared.Writers {
				return false, fmt.Sprintf("store %s declares %d writer units on node %s, task asks %d",
					name, declared.Writers, o.Hostname, n.Writers)
			}
		}
	}
	return true, ""
}
