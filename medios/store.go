package medios

import (
	"fmt"
	"regexp"
	"strings"
)

// StoreMode is the access a task asks for on a store. Each mode takes a
// unit the node declares: RO one reader, RW one writer, RW-exclusive every
// writer so no other writer is placed beside the task.
type StoreMode string

// Store modes.
const (
	StoreRO          StoreMode = "RO"
	StoreRW          StoreMode = "RW"
	StoreRWExclusive StoreMode = "RW-exclusive"
)

// A store name is the operator's declaration on the node, one word of
// letters, digits, dot, underscore and hyphen. The SDK checks the shape so
// a framework learns about a bad name at build time rather than at launch.
var storeNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// checkContainerPath refuses the container paths the controller refuses
// (medios validateStorePaths, DR-036 revision on medios#602). A relative
// path is one name inside the task's sandbox, such as "data", and is the
// form every node delivers: filesystem/linux and the docker executor bind
// the store at <sandbox>/<name>, and filesystem/posix on a Mac links it
// there. An absolute path is delivered on Linux only; a Mac node refuses it
// at launch.
func checkContainerPath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("Volume.ContainerPath is empty; name a directory inside the sandbox, such as \"data\", or an absolute path")
	case strings.HasPrefix(path, "/"):
		return nil
	case path == "." || path == "..":
		return fmt.Errorf("Volume.ContainerPath %q names the sandbox or its parent; a relative container path is one name inside the sandbox, such as \"data\"", path)
	case strings.Contains(path, "/"):
		return fmt.Errorf("Volume.ContainerPath %q is a relative path with more than one component; a relative container path is one name inside the sandbox, such as \"data\"", path)
	}
	return nil
}

// StoreVolume builds the volume for one store request: the store by name,
// at containerPath with the given mode. containerPath is one name inside
// the sandbox ("data"), which every node delivers, or an absolute path,
// which only Linux nodes deliver (see checkContainerPath). The mode maps to
// the wire as RO, RW with exclusive unset, or RW with exclusive set. An
// empty or malformed name, a container path the controller would refuse, or
// an unknown mode is refused with an error naming the field. There is no
// default store.
func StoreVolume(name, containerPath string, mode StoreMode) (Volume, error) {
	if name == "" {
		return Volume{}, fmt.Errorf("Volume.Source.Store.Name is required: name the store the task mounts")
	}
	if !storeNamePattern.MatchString(name) {
		return Volume{}, fmt.Errorf("Volume.Source.Store.Name %q is malformed: want one word of [A-Za-z0-9._-]", name)
	}
	if err := checkContainerPath(containerPath); err != nil {
		return Volume{}, err
	}
	v := Volume{
		ContainerPath: containerPath,
		Source:        &VolumeSource{Type: VolumeSourceStore, Store: &StoreSource{Name: name}},
	}
	switch mode {
	case StoreRO:
		v.Mode = VolumeRO
	case StoreRW:
		v.Mode = VolumeRW
	case StoreRWExclusive:
		v.Mode = VolumeRW
		v.Source.Store.Exclusive = true
	default:
		return Volume{}, fmt.Errorf("store mode %q is unknown: want RO, RW or RW-exclusive", string(mode))
	}
	return v, nil
}

// WithStore returns a copy of the task with one more store volume, built by
// StoreVolume, and the store's resource taken from the offer as the node
// advertised it (DR-036: a task that mounts a store lists one copy of
// store/<name>, which is what puts the holder on the controller's ledger).
// The offer must carry the store. Every volume already on the task is
// checked too, so a host path placed by hand is refused here rather than
// at launch.
//
// A volume rides on ContainerInfo, so a task with no Container gets one of
// type MEDIOS with no image: a bare command on the node's own filesystem
// with the store mounted in. A Container already set is kept as it is.
// Whether the node delivers the mount is the node's answer at launch: a
// node without a filesystem isolator refuses the launch with a sentence
// (medios#575), never silently runs without the store.
func (t TaskInfo) WithStore(o Offer, name, containerPath string, mode StoreMode) (TaskInfo, error) {
	if t.Container == nil {
		t.Container = &ContainerInfo{Type: ContainerMedios, Medios: &MediosInfo{}}
	}
	v, err := StoreVolume(name, containerPath, mode)
	if err != nil {
		return TaskInfo{}, err
	}
	r, ok := FindResource(o, StoreResourcePrefix+name)
	if !ok {
		return TaskInfo{}, fmt.Errorf("store %s is not in the offer from node %s", name, o.Hostname)
	}
	c := *t.Container
	c.Volumes = append(append([]Volume(nil), t.Container.Volumes...), v)
	t.Container = &c
	if !hasResource(t.Resources, r.Name) {
		t.Resources = append(append([]Resource(nil), t.Resources...), r)
	}
	if err := ValidateVolumes(t); err != nil {
		return TaskInfo{}, err
	}
	return t, nil
}

func hasResource(rs []Resource, name string) bool {
	for _, r := range rs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// ValidateVolumes refuses any volume on the task whose source is not a
// store. Frameworks name stores; a host path is the node's to know. It also
// refuses a store name the controller would, a container path the
// controller would, two volumes at one container path, exclusive on a
// read-only volume, which has no writer unit to take, and a store volume
// whose store/<name> resource is missing from the task, which the
// controller would refuse as a resource not in the offer.
func ValidateVolumes(t TaskInfo) error {
	if t.Container == nil {
		return nil
	}
	paths := map[string]bool{}
	for _, v := range t.Container.Volumes {
		if err := checkContainerPath(v.ContainerPath); err != nil {
			return err
		}
		if paths[v.ContainerPath] {
			return fmt.Errorf("two volumes on the task have container path %q; each volume needs its own", v.ContainerPath)
		}
		paths[v.ContainerPath] = true
		switch {
		case v.Source == nil:
			return fmt.Errorf("volume %s has no source; frameworks name stores, not host paths", v.ContainerPath)
		case v.Source.Type != VolumeSourceStore:
			return fmt.Errorf("volume %s has a %s source; frameworks name stores, not host paths",
				v.ContainerPath, v.Source.Type)
		case v.Source.Store == nil || v.Source.Store.Name == "":
			return fmt.Errorf("volume %s has a STORE source with no store name", v.ContainerPath)
		case !storeNamePattern.MatchString(v.Source.Store.Name):
			return fmt.Errorf("volume %s store name %q is malformed: want one word of [A-Za-z0-9._-]",
				v.ContainerPath, v.Source.Store.Name)
		case v.Source.Store.Exclusive && v.Mode != VolumeRW:
			return fmt.Errorf("volume %s is exclusive but not RW; exclusive takes writer units", v.ContainerPath)
		case !hasResource(t.Resources, StoreResourcePrefix+v.Source.Store.Name):
			return fmt.Errorf("volume %s names store %s but the task does not list resource %s%s; take it from the offer",
				v.ContainerPath, v.Source.Store.Name, StoreResourcePrefix, v.Source.Store.Name)
		}
	}
	return nil
}
