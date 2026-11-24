package medios

// Wire-format Go types for Medios's HTTP v1 scheduler API.
//
// Shapes mirror the JSON the controller emits / accepts. Field names match
// the wire (snake_case via json tags). We keep envelope-shaped structs with
// optional variant pointers, that's the actual shape of the call / event
// JSON, with `type` as the discriminator.
//
// v0.1 surface only: enough for the minimum viable scheduler. Pods,
// containerizers, health checks, volumes, constraints, executor-info, etc.
// are intentionally omitted; they land in later milestones with the
// features that need them.
//
// Field naming: the wire format uses `node_id`.

import (
	"encoding/json"
	"fmt"
)

// ID is the universal {"value": "..."} wrapper that the wire format uses
// for FrameworkID, OfferID, NodeID, TaskID, ContainerID, ExecutorID, etc.
type ID struct {
	Value string `json:"value"`
}

// Resource is a typed quantity (cpus, mem, disk, ports...). Type is "SCALAR"
// (cpus/mem/disk/gpus) or "RANGES" (ports); exactly one of Scalar/Ranges is set.
//
// A resource carries identity beyond its quantity: Metadata distinguishes an
// h100 gpu from an l4 one, and reservation / allocation / provider fields say
// whose it is. The controller compares that identity by equality, so a
// framework that accepts a quantity must return the resource it was offered
// rather than build a fresh one of the same name (medios#432). Every field
// this struct does not model is preserved verbatim through decode and encode
// (see extra), so a resource round-trips unchanged whether or not this library
// has been taught about it.
//
// Carve a task's share off an offered resource with Carve; do not construct a
// bare Resource for anything that came from an offer.
type Resource struct {
	Name   string  `json:"name"`
	Type   string  `json:"type"`             // "SCALAR" or "RANGES"
	Scalar *Scalar `json:"scalar,omitempty"` // populated when Type=="SCALAR"
	Ranges *Ranges `json:"ranges,omitempty"` // populated when Type=="RANGES" (ports)
	// Metadata is the resource's generic key/value identity, e.g. a gpu's
	// class. Present in the medios proto and compared by equality when the
	// controller matches a request to an offered resource.
	Metadata *Metadata `json:"metadata,omitempty"`
	// AllocationInfo is the role this resource is allocated to. Set only
	// for a framework that declares CapabilityMultiRole.
	AllocationInfo *AllocationInfo `json:"allocation_info,omitempty"`

	// extra holds every other field the offer carried (reservation,
	// provider_id, role, and anything added later). It is
	// unexported: callers never populate it, and it exists so identity
	// survives a decode/encode round trip without this library having to
	// track the controller's full resource schema.
	extra map[string]json.RawMessage
}

// resourceKnownFields are the keys Resource models directly; everything else
// in the wire object goes to extra.
var resourceKnownFields = map[string]bool{
	"name": true, "type": true, "scalar": true, "ranges": true, "metadata": true,
	"allocation_info": true,
}

// UnmarshalJSON decodes the modeled fields and keeps the rest verbatim.
func (r *Resource) UnmarshalJSON(b []byte) error {
	type plain Resource // no methods, so no recursion
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for k := range all {
		if resourceKnownFields[k] {
			delete(all, k)
		}
	}
	if len(all) > 0 {
		p.extra = all
	}
	*r = Resource(p)
	return nil
}

// MarshalJSON writes the modeled fields plus every preserved one.
func (r Resource) MarshalJSON() ([]byte, error) {
	type plain Resource
	b, err := json.Marshal(plain(r))
	if err != nil {
		return nil, err
	}
	if len(r.extra) == 0 {
		return b, nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	for k, v := range r.extra {
		if _, modeled := out[k]; !modeled {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// Carve returns a copy of an offered resource holding want of its quantity and
// all of its identity: metadata, and every field this library does not model.
// This is how a framework builds TaskInfo.Resources from the offer it matched.
//
// Only SCALAR resources are divisible; for any other type Carve returns the
// resource unchanged, since a RANGES share is chosen by picking ranges rather
// than by splitting a number. Asking for more than the resource holds is a
// caller bug and yields false.
func (r Resource) Carve(want float64) (Resource, bool) {
	if r.Type != "SCALAR" || r.Scalar == nil {
		return r, false
	}
	if want < 0 || want > r.Scalar.Value {
		return Resource{}, false
	}
	out := r
	out.Scalar = &Scalar{Value: want}
	return out, true
}

// Metadata is a resource's generic key/value identity (medios Resource.metadata).
type Metadata struct {
	Labels []Label `json:"labels"`
}

// Get returns the value of the first label with the given key.
func (m *Metadata) Get(key string) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, l := range m.Labels {
		if l.Key == key {
			return l.Value, true
		}
	}
	return "", false
}

// FindResource returns the first resource in the offer with the given name,
// with its identity intact. Prefer it over ranging and rebuilding.
func FindResource(o Offer, name string) (Resource, bool) {
	for _, r := range o.Resources {
		if r.Name == name {
			return r, true
		}
	}
	return Resource{}, false
}

type Scalar struct {
	Value float64 `json:"value"`
}

// Ranges is the value of a RANGES resource (e.g. ports): a set of inclusive
// integer ranges. Mirrors the medios Value.Ranges message.
type Ranges struct {
	Range []Range `json:"range"`
}

// Range is one inclusive [Begin, End] interval.
type Range struct {
	Begin uint64 `json:"begin"`
	End   uint64 `json:"end"`
}

// Ports flattens the ranges into the individual port numbers they cover, in
// order. A single offered range [31000,32000] yields 1001 ports.
func (r *Ranges) Ports() []uint64 {
	if r == nil {
		return nil
	}
	var ports []uint64
	for _, rng := range r.Range {
		for p := rng.Begin; p <= rng.End; p++ {
			ports = append(ports, p)
		}
	}
	return ports
}

// PortRangesFromOffer returns the "ports" RANGES resource from an offer, or nil
// if the offer carries no ports. Use (*Ranges).Ports to list individual ports.
func PortRangesFromOffer(o Offer) *Ranges {
	for _, r := range o.Resources {
		if r.Name == "ports" && r.Type == "RANGES" && r.Ranges != nil {
			return r.Ranges
		}
	}
	return nil
}

// NewPortsResource builds a "ports" RANGES resource for a task from individual
// port numbers, emitting one [p,p] range per port. Put the result in
// TaskInfo.Resources alongside cpus/mem to reserve those ports for the task.
func NewPortsResource(ports ...uint64) Resource {
	ranges := make([]Range, 0, len(ports))
	for _, p := range ports {
		ranges = append(ranges, Range{Begin: p, End: p})
	}
	return Resource{Name: "ports", Type: "RANGES", Ranges: &Ranges{Range: ranges}}
}

// CommandInfo describes how a task is launched. v0.2 adds Environment for
// per-task env vars and URIs for the fetcher (files/archives fetched into the
// sandbox before the command runs).
type CommandInfo struct {
	Shell       bool         `json:"shell"`
	Value       string       `json:"value"`
	Environment *Environment `json:"environment,omitempty"`
	URIs        []URI        `json:"uris,omitempty"`
}

// URI is a fetcher entry: the node fetches Value into the task's sandbox before
// the command runs. Mirrors the Medios CommandInfo.URI message.
type URI struct {
	Value      string `json:"value"`                // what to fetch: file://, http://, https://, …
	Executable bool   `json:"executable,omitempty"` // chmod +x the fetched file
	// Extract: if the fetched file is an archive, unpack it into the sandbox.
	// The controller's default is TRUE — so nil means "extract if it's an
	// archive"; set an explicit false for a plain file or binary you don't want
	// unpacked. (Use BoolPtr to set it.)
	Extract    *bool  `json:"extract,omitempty"`
	Cache      bool   `json:"cache,omitempty"`       // fetch through the fetcher cache
	OutputFile string `json:"output_file,omitempty"` // local name; defaults to the URI basename
	// Sha256: the fetched file's SHA-256 as 64 lowercase hex characters. The
	// node fails the fetch on a mismatch, before chmod or extract.
	Sha256 string `json:"sha256,omitempty"`
}

// BoolPtr returns a pointer to b — for setting URI.Extract explicitly
// (e.g. medios.BoolPtr(false) for a binary you don't want unpacked).
func BoolPtr(b bool) *bool { return &b }

// Environment is a set of env vars merged into the task's process.
type Environment struct {
	Variables []EnvVar `json:"variables"`
}

// EnvVar is one name/value environment entry.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TaskInfo is the per-task spec sent in ACCEPT/LAUNCH. Container (for
// image-based stages) and Labels (task attribution / reconciliation) are
// optional, so a bare-command task omits them.
//
// Principal is required: the controller refuses a launch without one. Build
// a TaskInfo with NewTaskInfo, which checks it, rather than filling the
// struct directly.
type TaskInfo struct {
	Name   string `json:"name"`
	TaskID ID     `json:"task_id"`
	NodeID ID     `json:"node_id"`
	// Principal is the submitter the task runs on behalf of, as kind:name
	// (see ValidatePrincipal). It is distinct from the framework's own
	// principal, which the controller binds at SUBSCRIBE, and from the OS
	// user on Command.User. Always encoded, never omitted: a captured
	// ACCEPT then shows an empty principal as empty rather than absent.
	Principal string         `json:"principal"`
	Resources []Resource     `json:"resources"`
	Command   CommandInfo    `json:"command"`
	Container *ContainerInfo `json:"container,omitempty"`
	Labels    *Labels        `json:"labels,omitempty"`
	// MaxCompletionTime is the task's own ceiling: the executor kills the
	// task when it has run this long, field 14 on TaskInfo. Nil means no
	// ceiling. A finish hook's cap is counted inside it (DR-042).
	MaxCompletionTime *DurationInfo `json:"max_completion_time,omitempty"`
	// KillPolicy is how long the node waits between the stop signal and the
	// forced kill. Nil means the node's default; under docker_shell that is
	// `docker stop --time` 10 s, and a grace period is rounded up to whole
	// seconds (medios docs/containerizer-fields.md).
	KillPolicy *KillPolicy `json:"kill_policy,omitempty"`
	// ProgressPolicy is what the executor should do about this task's
	// progress. Nil means the task reports none, which is the shape of
	// every task launched before the field existed. Set it from a job's
	// profile with ProgressPolicyForProfile, and check a hand-built one
	// with ValidateProgressPolicy: NewTaskInfo does not take it.
	ProgressPolicy *ProgressPolicy `json:"progress_policy,omitempty"`
	// Observers are commands the executor runs in the task's sandbox beside
	// the task, whose newest output line becomes the task's progress
	// message. Empty means the task is unobserved, which is the shape of
	// every task launched before the field existed. Append with
	// WithObserver, which refuses a name or a count the controller would
	// refuse, and check a hand-built list with ValidateTaskObservers:
	// NewTaskInfo does not take them.
	Observers []TaskObserver `json:"observers,omitempty"`
	// Finish is a command the executor runs after the work command exits,
	// whatever the exit status, in the same sandbox, with its own cap
	// counted inside the task's ceiling (DR-042, field 19). Nil means the
	// task has no finish hook, which is the shape of every task launched
	// before the field existed. Set it with WithFinish; check a hand-built
	// one with ValidateTaskFinish.
	Finish *TaskFinish `json:"finish,omitempty"`
}

// ContainerInfo describes how a task's process is containerized. Two types
// are supported, and which one a node accepts depends on the containerizer
// it runs:
//
//   - DOCKER: an OCI image run by the docker_shell containerizer; set Docker.
//   - MEDIOS: an OCI image provisioned and run by the medios containerizer
//     (--containerizers=medios); set Medios. A DOCKER-typed task sent to
//     such a node is refused with TASK_ERROR and REASON_TASK_INVALID.
//
// Exactly one of Docker and Medios is set, matching Type. Volumes are store
// mounts (see Volume) and apply to both. On an image node a TaskInfo for an image
// stage must carry a ContainerInfo or it has no execution path (medios#200).
type ContainerInfo struct {
	Type    string      `json:"type"` // ContainerDocker | ContainerMedios
	Docker  *DockerInfo `json:"docker,omitempty"`
	Medios  *MediosInfo `json:"medios,omitempty"`
	Volumes []Volume    `json:"volumes,omitempty"`
	// LinuxInfo sets the container's IPC namespace and the size of its
	// /dev/shm. Nil gives docker_shell's defaults, including Docker's
	// 64 MB /dev/shm. Check it with ValidateContainerInfo.
	LinuxInfo *LinuxInfo `json:"linux_info,omitempty"`
}

// ContainerInfo types.
const (
	// ContainerDocker is the ContainerInfo.Type for an OCI image run via the
	// docker_shell containerizer.
	ContainerDocker = "DOCKER"
	// ContainerMedios is the ContainerInfo.Type for an OCI image run via the
	// medios containerizer.
	ContainerMedios = "MEDIOS"
)

// DockerInfo names the image for a DOCKER container.
//
// Network is one of the Network* names, or empty for Docker's default
// bridge. Leave it empty rather than sending NetworkBridge by habit: the
// proto's default is HOST, and docker_shell reads an absent field as the
// bridge. docker_shell gives `--network=host` for NetworkHost and refuses
// NetworkNone and NetworkUser (medios#661). Check it with
// ValidateContainerInfo.
type DockerInfo struct {
	Image          string      `json:"image"`
	ForcePullImage bool        `json:"force_pull_image,omitempty"`
	Network        string      `json:"network,omitempty"`
	Parameters     []Parameter `json:"parameters,omitempty"`
}

// Parameter is a key/value pair used by DockerInfo.Parameters (for example
// Key: "entrypoint" under docker_shell, medios#574) and URL.Query.
type Parameter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// DockerInfo.Network values, the proto's DockerInfo.Network names.
const (
	NetworkHost   = "HOST"
	NetworkBridge = "BRIDGE"
	NetworkNone   = "NONE"
	NetworkUser   = "USER"
)

// LinuxInfo is the container's IPC namespace and the size of its /dev/shm
// in MB. ShmSize needs IpcMode IpcPrivate, the controller's own rule.
type LinuxInfo struct {
	IpcMode string  `json:"ipc_mode,omitempty"`
	ShmSize *uint32 `json:"shm_size,omitempty"`
}

// LinuxInfo.IpcMode values.
const (
	IpcPrivate     = "PRIVATE"
	IpcShareParent = "SHARE_PARENT"
)

// KillPolicy is the task's grace period between the stop signal and the
// forced kill.
type KillPolicy struct {
	GracePeriod *DurationInfo `json:"grace_period,omitempty"`
}

// ValidateContainerInfo refuses the enum values the controller would drop
// without an error, and a /dev/shm size without a private IPC namespace.
// A nil ContainerInfo is valid.
func ValidateContainerInfo(c *ContainerInfo) error {
	if c == nil {
		return nil
	}
	if c.Docker != nil {
		switch c.Docker.Network {
		case "", NetworkHost, NetworkBridge, NetworkNone, NetworkUser:
		default:
			return fmt.Errorf("Invalid container: 'docker.network' %q is not one of %s, %s, %s, %s",
				c.Docker.Network, NetworkHost, NetworkBridge, NetworkNone, NetworkUser)
		}
	}
	if l := c.LinuxInfo; l != nil {
		switch l.IpcMode {
		case "", IpcPrivate, IpcShareParent:
		default:
			return fmt.Errorf("Invalid container: 'linux_info.ipc_mode' %q is not one of %s, %s",
				l.IpcMode, IpcPrivate, IpcShareParent)
		}
		if l.ShmSize != nil && l.IpcMode != IpcPrivate {
			return fmt.Errorf("Invalid container: 'linux_info.shm_size' needs 'ipc_mode' %s", IpcPrivate)
		}
	}
	return nil
}

// MediosInfo configures a MEDIOS container. Image is the OCI image the
// medios containerizer provisions; absent, the task runs on the node's own
// filesystem.
type MediosInfo struct {
	Image *Image `json:"image,omitempty"`
}

// Image names an OCI image for a MEDIOS container. Type is ImageDocker and
// Docker names the image. Cached defaults to true on the node; set it to
// false to make the node pull the image again even when its store already
// has it, the MEDIOS counterpart of DockerInfo.ForcePullImage.
type Image struct {
	Type   string          `json:"type"` // ImageDocker
	Docker ImageDockerInfo `json:"docker"`
	Cached *bool           `json:"cached,omitempty"`
}

// ImageDocker is the Image.Type for an image fetched from a docker registry.
const ImageDocker = "DOCKER"

// ImageDockerInfo names a registry image:
// [REGISTRY_HOST[:REGISTRY_PORT]/]REPOSITORY[:TAG|@TYPE:DIGEST].
type ImageDockerInfo struct {
	Name string `json:"name"`
}

// Volume mounts a store the node declares at ContainerPath. A framework
// names the store and the access it wants; the node resolves the name to a
// path and the controller records what the task was given. There is no host
// path: the controller refuses a volume whose source is anything but a
// STORE, so build one with StoreVolume or TaskInfo.WithStore.
//
// Mode RO takes one of the store's reader units, Mode RW one of its writer
// units; Source.Store.Exclusive on an RW volume takes every writer unit the
// store declares so no other writer is placed beside it.
type Volume struct {
	ContainerPath string        `json:"container_path"`
	Mode          string        `json:"mode"` // VolumeRW | VolumeRO
	Source        *VolumeSource `json:"source,omitempty"`
}

// Volume modes.
const (
	VolumeRW = "RW"
	VolumeRO = "RO"
)

// VolumeSource says where a volume comes from. STORE is the only type a
// framework may send; HOST_PATH is named so the refusal can say what it saw.
type VolumeSource struct {
	Type  string       `json:"type"` // VolumeSourceStore
	Store *StoreSource `json:"store,omitempty"`
}

// Volume source types.
const (
	VolumeSourceStore    = "STORE"
	VolumeSourceHostPath = "HOST_PATH"
)

// StoreSource names a store declared on a node. Exclusive applies to an RW
// volume only.
type StoreSource struct {
	Name      string `json:"name"`
	Exclusive bool   `json:"exclusive,omitempty"`
}

// Labels is a set of key/value labels attached to a TaskInfo, used for
// attribution and reconciliation (recovering which task belongs to what after
// a restart).
type Labels struct {
	Labels []Label `json:"labels"`
}

// Label is one key/value task label.
type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// AllocationInfo names the role a resource, or a whole offer, is allocated
// to. The controller sets it on every offer and every offered resource for a
// framework that declares CapabilityMultiRole, and a task's resources must
// carry it back (medios src/controller/controller.cpp, offer construction).
type AllocationInfo struct {
	Role string `json:"role"`
}

// Offer is the resource grant the controller sends in OFFERS events. The
// controller builds one offer per node and role, so all of an offer's
// resources share its AllocationInfo.Role.
type Offer struct {
	AllocationInfo *AllocationInfo `json:"allocation_info,omitempty"`
	ID             ID              `json:"id"`
	FrameworkID    ID              `json:"framework_id"`
	NodeID         ID              `json:"node_id"`
	Hostname       string          `json:"hostname"`
	URL            *URL            `json:"url,omitempty"`
	Resources      []Resource      `json:"resources"`
	Attributes     []Attribute     `json:"attributes,omitempty"` // v0.2.1: for constraint matching
}

// URL is a node's HTTP endpoint as carried on an Offer (medios URL message).
type URL struct {
	Scheme   string      `json:"scheme"`
	Address  Address     `json:"address"`
	Path     string      `json:"path,omitempty"`
	Query    []Parameter `json:"query,omitempty"`
	Fragment string      `json:"fragment,omitempty"`
}

// Attribute is a typed key/value on an Offer. v0.2.1 supports TEXT only;
// SCALAR / RANGES / SET land if constraint use-cases require them.
//
// Wire format:
//
//	{"name": "rack", "type": "TEXT", "text": {"value": "rack-1"}}
type Attribute struct {
	Name string         `json:"name"`
	Type string         `json:"type"` // "TEXT" for v0.2.1
	Text *AttributeText `json:"text,omitempty"`
}

type AttributeText struct {
	Value string `json:"value"`
}

// TextValue returns the text value if the attribute is TEXT-typed, "" otherwise.
func (a Attribute) TextValue() string {
	if a.Type == "TEXT" && a.Text != nil {
		return a.Text.Value
	}
	return ""
}

// TaskStatus is the per-task lifecycle update in UPDATE events.
type TaskStatus struct {
	TaskID  ID        `json:"task_id"`
	State   TaskState `json:"state"`
	NodeID  ID        `json:"node_id"`
	UUID    string    `json:"uuid,omitempty"`
	Source  string    `json:"source,omitempty"`
	Message string    `json:"message,omitempty"`
	// Reason is why the controller sent this status, e.g.
	// REASON_RECONCILIATION or REASON_CONTAINER_LAUNCH_FAILED. It is what
	// makes TASK_LOST interpretable: the same state means "gone" or "may
	// still be running" depending on the reason, so a framework that ignores
	// it cannot tell a dead task from an unreachable one. Passed through as
	// the wire string rather than enumerated, because which reasons a
	// controller sends is its contract to state.
	Reason          string           `json:"reason,omitempty"`
	Timestamp       float64          `json:"timestamp,omitempty"`
	ContainerStatus *ContainerStatus `json:"container_status,omitempty"`
	// Principal and FrameworkPrincipal are the controller's record of the
	// pair a task ran as: the submitter from TaskInfo.Principal and the
	// framework principal bound at SUBSCRIBE. Read-only; a framework's
	// store mirrors them and holds no copy of its own. Empty from a
	// controller that predates the record.
	Principal          string `json:"principal,omitempty"`
	FrameworkPrincipal string `json:"framework_principal,omitempty"`
	// Stores is the controller's record of the stores the task was given,
	// one entry per store volume (DR-036, medios#529; wire field `stores`,
	// since `data` is the executor payload). Read-only; a framework's
	// store mirrors it. Empty from a controller that predates the record.
	Stores []TaskData `json:"stores,omitempty"`
	// Progress is the last progress the task reported about itself, as the
	// executor watching it saw it (medios#544 decision A; wire field
	// `progress`). Read-only: the executor running the task sets it and the
	// controller records it. Nil means nothing was reported, which is not
	// zero and not complete. See Progress.
	Progress *Progress `json:"progress,omitempty"`
	// Finish is what became of the task's finish command, reported on the
	// terminal status apart from the task's own state, which follows the
	// work command (DR-042, field 21). Nil means the task had no finish
	// hook or the controller predates the field; a framework never reads
	// nil as ran.
	// It decodes as sent; Ran and Signalled refuse a shape
	// ValidateFinishOutcome refuses, so an unreadable finish never costs
	// the status and is never read as a confident answer.
	Finish *FinishStatus `json:"finish,omitempty"`
}

// TaskData is one store a task was given, as the controller records it:
// the store, where it is mounted, with what access, and on which node.
type TaskData struct {
	Store         string `json:"store"`
	ContainerPath string `json:"container_path"`
	Mode          string `json:"mode"` // VolumeRW | VolumeRO
	Exclusive     bool   `json:"exclusive,omitempty"`
	NodeID        ID     `json:"node_id"`
}

// ContainerStatus surfaces container_id (captured from the first TASK_RUNNING
// update; needed to construct the sandbox stdout path, see medios
// docs/scheduler-http-api.md).
type ContainerStatus struct {
	ContainerID ID `json:"container_id"`
}

// FrameworkInfo is the framework identity sent at SUBSCRIBE time.
// User MUST be a real OS user that exists on every node; an empty or
// nonexistent user causes silent TASK_LOST.
type FrameworkInfo struct {
	User string `json:"user"`
	Name string `json:"name"`
	// ID is set only on re-subscribe, to the framework_id captured from a
	// previous run, so the controller treats this as the *same* framework
	// (failover) rather than a new one. nil on first subscribe.
	ID *ID `json:"id,omitempty"`
	// FailoverTimeout is how long (seconds) the controller keeps this
	// framework's tasks alive after it disconnects, waiting for it to
	// re-subscribe. 0 (the default) means tasks are killed (TASK_LOST) the
	// moment the framework disconnects — set non-zero for restart survival.
	FailoverTimeout float64 `json:"failover_timeout,omitempty"`
	// Checkpoint asks each node to record this framework's executors and
	// tasks on disk, so that a restarted node recovers them instead of
	// destroying them as orphans. false (the default) means a node restart
	// kills every task this framework has on that node. It covers a node
	// restart only; FailoverTimeout covers the framework's own restart.
	Checkpoint bool `json:"checkpoint,omitempty"`
	// Principal is the framework's own identity (required at SUBSCRIBE by
	// medios#537 when unauthenticated, or bound from the authenticated
	// principal when HTTP authentication is enabled). Set through
	// scheduler.WithPrincipal.
	Principal string `json:"principal,omitempty"`
	// Hostname is the hostname reported by the framework at SUBSCRIBE
	// (medios#409). Defaults to os.Hostname() in scheduler.NewClient and
	// can be overridden with scheduler.WithHostname.
	Hostname string `json:"hostname,omitempty"`
	// Capabilities the framework declares. The controller gates parts of
	// the offer stream on these: without CapabilityGPUResources its
	// allocator excludes every resource on a GPU-bearing node, not just
	// the gpus (medios src/controller/allocator/medios/hierarchical.cpp,
	// filter_gpu_resources, default true). Empty is the single-tenant
	// default and omits the field.
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Role is the allocation role this framework subscribes under. Empty
	// means the default role, which is what a single-tenant cluster wants.
	//
	// It decides which reserved resources reach this framework at all: a
	// resource reserved for role "x" is offered only to a framework
	// subscribed as "x", so a framework that leaves this unset never sees
	// reserved supply and cannot exercise the paths that handle it. That is
	// also why a name appears more than once in an offer — reservations split
	// a quantity across role-bearing copies — which is the case both fennel
	// and cue had to fix in their matchers.
	//
	// Role and Roles are exclusive: the controller refuses both together.
	Role string `json:"role,omitempty"`
	// Roles are the allocation roles of a framework that declares
	// CapabilityMultiRole. Set through scheduler.WithRoles.
	Roles []string `json:"roles,omitempty"`
	// WebUIURL is the base URL clients use to reach this framework's own
	// API, for example "http://fennel.example:8080". The controller stores
	// it from SUBSCRIBE and lists it per framework in /state, which is how a
	// client that knows only the controller address finds the framework.
	// It is a client address, not a bind address: a wildcard host such as
	// 0.0.0.0 is not reachable by any client. Empty is omitted, and the
	// controller then reports an empty webui_url. Set through
	// scheduler.WithWebUIURL.
	WebUIURL string `json:"webui_url,omitempty"`
}

// Capability is one entry in FrameworkInfo.capabilities.
type Capability struct {
	Type string `json:"type"`
}

// CapabilityGPUResources lets the framework receive offers from nodes that
// advertise GPUs. Confirmed required by default against the medios
// controller (2026-09-10). Other capability names are passed through as
// strings rather than enumerated here, because which ones this controller
// honors is its contract to state, not this library's to assume.
const CapabilityGPUResources = "GPU_RESOURCES"

// CapabilityPartitionAware makes the controller report a partitioned task as
// TASK_UNREACHABLE instead of collapsing it into TASK_LOST.
//
// Without it every loss looks the same, and a framework has to infer from
// TaskStatus.Reason whether a task is gone or merely out of contact. With it
// the substrate says which, so an unreachable task can be waited out rather
// than replaced on a guess.
const CapabilityPartitionAware = "PARTITION_AWARE"

// CapabilityMultiRole lets a framework subscribe under several roles at once
// through FrameworkInfo.Roles. The controller then puts AllocationInfo on
// every offer and offered resource, and expects it back on launched tasks.
const CapabilityMultiRole = "MULTI_ROLE"

// =========================================================================
// Events (controller → scheduler over the SUBSCRIBE response stream)
// =========================================================================

// Event is the envelope-shaped event the controller emits. The Type field
// is the discriminator; the variant pointer for that Type is populated and
// the rest are nil.
type Event struct {
	Type       string          `json:"type"`
	Subscribed *SubscribedBody `json:"subscribed,omitempty"`
	Offers     *OffersBody     `json:"offers,omitempty"`
	Update     *UpdateBody     `json:"update,omitempty"`
	Rescind    *RescindBody    `json:"rescind,omitempty"`
	// HEARTBEAT carries no body.
	Error *ErrorBody `json:"error,omitempty"`
}

const (
	EventSubscribed = "SUBSCRIBED"
	EventOffers     = "OFFERS"
	EventUpdate     = "UPDATE"
	EventHeartbeat  = "HEARTBEAT"
	EventRescind    = "RESCIND"
	EventError      = "ERROR"
)

type SubscribedBody struct {
	FrameworkID              ID      `json:"framework_id"`
	HeartbeatIntervalSeconds float64 `json:"heartbeat_interval_seconds,omitempty"`
	// ControllerInfo describes the controller that accepted this
	// subscription. Nil if the event carried none.
	ControllerInfo *ControllerInfo `json:"controller_info,omitempty"`
}

// ControllerInfo identifies the controller a framework is subscribed to and
// states what it supports. It arrives on SUBSCRIBED, including on
// re-subscribe, which makes it the only place a framework learns that the
// controller it just reconnected to is not the one it was talking to before.
//
// Only the fields with a settled meaning are modelled. The deprecated packed
// ip/port pair is deliberately absent: address supersedes it, and reviving a
// 4-byte packed integer in a JSON library would be preserving a mistake.
type ControllerInfo struct {
	// ID is the controller's own identifier, and it changes on failover.
	// A framework that records it can tell a reconnect to the same
	// controller from a reconnect to a different one, which the framework
	// ID alone does not distinguish.
	ID string `json:"id,omitempty"`
	// Version is the running controller version.
	Version string `json:"version,omitempty"`
	// Hostname as the controller reports it. Unreliable where internal
	// DNS does not resolve; prefer Address.
	Hostname string `json:"hostname,omitempty"`
	// Address is the controller's IPv4 or IPv6 address and port.
	Address *Address `json:"address,omitempty"`
	// Capabilities the controller declares. These are the controller's,
	// not the framework's: a framework reads them to find out what this
	// controller can do before asking it to do it. Unknown names pass
	// through as strings rather than being enumerated here.
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// Address is a host and port. IPv4 and IPv6 both arrive in the ip field as
// text; the field is named for what it carries rather than parsed here,
// because a framework that needs a net.IP can parse it and one that only
// logs the endpoint should not pay for the failure case.
type Address struct {
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip,omitempty"`
	Port     int    `json:"port,omitempty"`
}

type OffersBody struct {
	Offers []Offer `json:"offers"`
}

type UpdateBody struct {
	Status TaskStatus `json:"status"`
}

type RescindBody struct {
	OfferID ID `json:"offer_id"`
}

type ErrorBody struct {
	Message string `json:"message"`
}

// =========================================================================
// Calls (scheduler → controller, POST /api/v1/scheduler)
// =========================================================================

// Call is the envelope-shaped request body. Type is the discriminator;
// FrameworkID is required on every call except SUBSCRIBE; the variant
// pointer for the chosen Type is populated.
type Call struct {
	Type        string           `json:"type"`
	FrameworkID *ID              `json:"framework_id,omitempty"`
	Subscribe   *SubscribeBody   `json:"subscribe,omitempty"`
	Accept      *AcceptBody      `json:"accept,omitempty"`
	Decline     *DeclineBody     `json:"decline,omitempty"`
	Kill        *KillBody        `json:"kill,omitempty"`
	Acknowledge *AcknowledgeBody `json:"acknowledge,omitempty"`
	Suppress    *SuppressBody    `json:"suppress,omitempty"`
	Revive      *ReviveBody      `json:"revive,omitempty"`
	Reconcile   *ReconcileBody   `json:"reconcile,omitempty"`
	Request     *RequestBody     `json:"request,omitempty"`
	// TEARDOWN carries no body beyond Type + FrameworkID.
}

const (
	CallSubscribe   = "SUBSCRIBE"
	CallAccept      = "ACCEPT"
	CallDecline     = "DECLINE"
	CallKill        = "KILL"
	CallAcknowledge = "ACKNOWLEDGE"
	CallSuppress    = "SUPPRESS"
	CallRevive      = "REVIVE"
	CallReconcile   = "RECONCILE"
	CallRequest     = "REQUEST"
	CallTeardown    = "TEARDOWN"
)

// RequestBody is the REQUEST call body: a gang or proactive resource request
// to the allocator (medios scheduler.Call.Request).
type RequestBody struct {
	Requests []Request `json:"requests"`
}

// Request is one member's per-node resource demand in a REQUEST call
// (medios.v1.Request). When NodeID is set, the demand is pinned to that node.
type Request struct {
	NodeID    *ID        `json:"node_id,omitempty"`
	Resources []Resource `json:"resources"`
}

// ReconcileBody is the RECONCILE call body. RECONCILE asks the controller to
// resend the current status of the given tasks — the basis for restart
// survival: after a restart a framework reconciles the tasks it believes are
// running and re-learns their state. An empty Tasks list reconciles ALL of the
// framework's known tasks (implicit reconciliation).
type ReconcileBody struct {
	Tasks []ReconcileTask `json:"tasks"`
}

// ReconcileTask names a task to reconcile. NodeID is optional but including it
// (when known) lets the controller answer faster.
type ReconcileTask struct {
	TaskID ID  `json:"task_id"`
	NodeID *ID `json:"node_id,omitempty"`
}

type SubscribeBody struct {
	FrameworkInfo FrameworkInfo `json:"framework_info"`
}

type AcceptBody struct {
	OfferIDs   []ID        `json:"offer_ids"`
	Operations []Operation `json:"operations"`
}

// Operation is currently always LAUNCH.
type Operation struct {
	Type   string    `json:"type"` // "LAUNCH"
	Launch *LaunchOp `json:"launch,omitempty"`
}

const OperationLaunch = "LAUNCH"

type LaunchOp struct {
	TaskInfos []TaskInfo `json:"task_infos"`
}

type DeclineBody struct {
	OfferIDs []ID     `json:"offer_ids"`
	Filters  *Filters `json:"filters,omitempty"`
}

// Filters tunes how the controller treats the resources a framework declines
// (or doesn't use after ACCEPT). refuse_seconds is the time the controller
// considers those resources refused before offering them to this framework
// again. The controller's own default is 5s when no Filters is sent; pass an
// explicit value (Marathon uses 120s) to reduce decline/offer churn.
type Filters struct {
	RefuseSeconds float64 `json:"refuse_seconds,omitempty"`
}

// SuppressBody is the SUPPRESS call body. Empty Roles suppresses offers for
// all of the framework's roles — the single-role case.
type SuppressBody struct {
	Roles []string `json:"roles,omitempty"`
}

// ReviveBody is the REVIVE call body. REVIVE removes any filters previously
// set via ACCEPT or DECLINE and resumes offers. Empty Roles revives all of
// the framework's roles.
type ReviveBody struct {
	Roles []string `json:"roles,omitempty"`
}

type KillBody struct {
	TaskID ID `json:"task_id"`
	NodeID ID `json:"node_id"`
}

type AcknowledgeBody struct {
	NodeID ID     `json:"node_id"`
	TaskID ID     `json:"task_id"`
	UUID   string `json:"uuid"`
}

// =========================================================================
// Constructors (less wire-shape footgun, more readable consumer code)
// =========================================================================

// NewSubscribeCall builds a SUBSCRIBE call body.
//
// On re-subscribe (info.ID set), the controller requires the top-level
// call.framework_id to be present AND equal to subscribe.framework_info.id —
// otherwise it rejects with HTTP 400 "'framework_id' differs from
// 'subscribe.framework_info.id'" and the framework can never fail over to its
// previous registration.
func NewSubscribeCall(info FrameworkInfo) Call {
	return Call{
		Type:        CallSubscribe,
		FrameworkID: info.ID,
		Subscribe:   &SubscribeBody{FrameworkInfo: info},
	}
}

// NewAcceptCall builds an ACCEPT with a single LAUNCH operation over the
// given offers and task list.
func NewAcceptCall(frameworkID string, offerIDs []ID, tasks []TaskInfo) Call {
	fid := ID{Value: frameworkID}
	return Call{
		Type:        CallAccept,
		FrameworkID: &fid,
		Accept: &AcceptBody{
			OfferIDs: offerIDs,
			Operations: []Operation{{
				Type:   OperationLaunch,
				Launch: &LaunchOp{TaskInfos: tasks},
			}},
		},
	}
}

// NewDeclineCall builds a DECLINE for the given offers. When refuseSeconds is
// > 0 it is attached as a Filters.refuse_seconds, so the controller waits that
// long before re-offering these resources (reduces churn vs the 5s default).
// A value <= 0 omits Filters and the controller uses its default.
func NewDeclineCall(frameworkID string, offerIDs []ID, refuseSeconds float64) Call {
	fid := ID{Value: frameworkID}
	body := &DeclineBody{OfferIDs: offerIDs}
	if refuseSeconds > 0 {
		body.Filters = &Filters{RefuseSeconds: refuseSeconds}
	}
	return Call{
		Type:        CallDecline,
		FrameworkID: &fid,
		Decline:     body,
	}
}

// NewSuppressCall builds a SUPPRESS — tell the controller to stop sending
// offers to this framework (single-role: all roles).
func NewSuppressCall(frameworkID string) Call {
	fid := ID{Value: frameworkID}
	return Call{
		Type:        CallSuppress,
		FrameworkID: &fid,
		Suppress:    &SuppressBody{},
	}
}

// NewReviveCall builds a REVIVE — clear any decline/accept filters and resume
// offers (single-role: all roles).
func NewReviveCall(frameworkID string) Call {
	fid := ID{Value: frameworkID}
	return Call{
		Type:        CallRevive,
		FrameworkID: &fid,
		Revive:      &ReviveBody{},
	}
}

// NewReconcileCall builds a RECONCILE for the given tasks. Pass nil/empty to
// reconcile all of the framework's tasks (implicit reconciliation).
func NewReconcileCall(frameworkID string, tasks []ReconcileTask) Call {
	fid := ID{Value: frameworkID}
	if tasks == nil {
		tasks = []ReconcileTask{} // emit "tasks":[] (reconcile-all), not null
	}
	return Call{
		Type:        CallReconcile,
		FrameworkID: &fid,
		Reconcile:   &ReconcileBody{Tasks: tasks},
	}
}

// NewRequestCall builds a REQUEST call for the given per-node resource demands.
func NewRequestCall(frameworkID string, requests []Request) Call {
	fid := ID{Value: frameworkID}
	if requests == nil {
		requests = []Request{}
	}
	return Call{
		Type:        CallRequest,
		FrameworkID: &fid,
		Request:     &RequestBody{Requests: requests},
	}
}

// NewKillCall builds a KILL for the given task on the given node.
func NewKillCall(frameworkID, taskID, nodeID string) Call {
	fid := ID{Value: frameworkID}
	return Call{
		Type:        CallKill,
		FrameworkID: &fid,
		Kill: &KillBody{
			TaskID: ID{Value: taskID},
			NodeID: ID{Value: nodeID},
		},
	}
}

// NewAcknowledgeCall builds an ACKNOWLEDGE for a status update with a uuid.
// Updates without a uuid don't require acknowledgment.
func NewAcknowledgeCall(frameworkID, taskID, nodeID, uuid string) Call {
	fid := ID{Value: frameworkID}
	return Call{
		Type:        CallAcknowledge,
		FrameworkID: &fid,
		Acknowledge: &AcknowledgeBody{
			TaskID: ID{Value: taskID},
			NodeID: ID{Value: nodeID},
			UUID:   uuid,
		},
	}
}

// NewTeardownCall builds a TEARDOWN.
func NewTeardownCall(frameworkID string) Call {
	fid := ID{Value: frameworkID}
	return Call{
		Type:        CallTeardown,
		FrameworkID: &fid,
	}
}
