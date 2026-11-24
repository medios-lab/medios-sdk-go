// Package outputs is the attempt namespace inside a declared RW store: where
// a task's finish command publishes its files, the manifest that binds them
// to the exact attempt, and the verifier a later reader uses.
//
// Layout, under the store's mount: <namespace>/<job>/<attempt>/. The finish
// command writes its artifacts into the attempt directory first, then
// commits manifest.json last, so a reader that finds a manifest finds every
// artifact it names, including after a host crash. The fsync set, in
// order: each artifact's bytes through the descriptor that hashed them,
// then every ancestor directory of every declared artifact below the
// attempt directory, deepest first, then the attempt directory, then the
// link of manifest.json, then the attempt directory again. A second
// writer for the same attempt fails; nothing here overwrites a committed
// manifest.
//
// The commit is a hard link, so a store must be on a filesystem that has
// them: a local directory or an NFS export do, which is what a declared
// store is today; FAT and FUSE object-store mounts do not, and Publish
// refuses every commit there with a sentence naming the requirement. A
// publish interrupted by a kill or a crash can leave manifest.json.*.tmp
// in the attempt directory; nothing removes it, and Verify reports it as
// an undeclared file so a reconciler sees it.
//
// The package has no medios dependency so that the publish binary a finish
// script calls stays small and a reader outside a framework can verify a
// store with the same code.
package outputs

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion is the manifest format this package writes and reads. A
// manifest with another version is refused rather than guessed at; the
// version changes only when a field's meaning changes.
const SchemaVersion = 1

// ManifestName is the committed manifest's file name inside the attempt
// directory. It is also the one path an artifact may not use.
const ManifestName = "manifest.json"

// A namespace, job or attempt is one path component of letters, digits,
// dot, underscore and hyphen, so the layout is the same on every filesystem
// a store can be and no component can climb out of the store.
var componentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// FinishState is what became of the finish command, as medios reports it
// on the terminal status (DR-042, medios#580): the enum's name. The
// manifest carries the writer's view at publish time, which is RAN while
// the finish command is the one writing; the controller's status is the
// authority for the other three. The names, fields and rules here are the
// same as the medios package's FinishStatus, pinned by a test that feeds
// one payload to both validators, so one record has one rule.
type FinishState string

const (
	FinishRan         FinishState = "RAN"
	FinishTimedOut    FinishState = "TIMED_OUT"
	FinishKilled      FinishState = "KILLED"
	FinishDidNotStart FinishState = "DID_NOT_START"
	// FinishLost: the hook ran and its outcome could not be read; the
	// reason carries the read error, and neither exit_code nor signal is
	// set.
	FinishLost FinishState = "LOST"
)

// Manifest is the receipt for one attempt's outputs.
type Manifest struct {
	SchemaVersion int `json:"schema_version"`

	// Namespace, Job and Attempt are the attempt's identity and its path
	// under the store: a new framework generation that reuses a job name
	// publishes under its own namespace and never collides.
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Attempt   string `json:"attempt"`

	// The medios identities of the task that produced the outputs, as the
	// finish command recorded them.
	FrameworkID string `json:"framework_id"`
	TaskID      string `json:"task_id"`
	NodeID      string `json:"node_id"`
	// Submitter is the task's principal, kind:name.
	Submitter string `json:"submitter"`

	// Work is the work command's outcome: its exit status, as the finish
	// command received it (see WorkOutcome).
	Work WorkOutcome `json:"work"`
	// Finish is the finish command's outcome as known at publish time.
	Finish FinishOutcome `json:"finish"`

	// StartedAt and FinishedAt bound the work command, RFC 3339 in UTC,
	// when the writer knows them. PublishedAt is set by Publish.
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	PublishedAt string `json:"published_at"`

	Artifacts []Artifact `json:"artifacts"`
}

// WorkOutcome is the work command's exit status as the finish command
// recorded it. How the finish command learns it is between it and the
// executor; this package asserts nothing about that. ExitStatus is a
// pointer so that a manifest that never said how the work ended is refused
// rather than read as 0; int32 is the width medios uses.
type WorkOutcome struct {
	ExitStatus *int32 `json:"exit_status"`
}

// FinishOutcome is the finish command's outcome, in DR-042's shape.
type FinishOutcome struct {
	State FinishState `json:"state"`
	// ExitCode is set when the hook ran and exited; Signal when it ran and
	// was signalled. Exactly one of them is set for RAN.
	ExitCode *int32 `json:"exit_code,omitempty"`
	Signal   *int32 `json:"signal,omitempty"`
	// Reason says why for DID_NOT_START.
	Reason string `json:"reason,omitempty"`
}

// Artifact is one published file.
type Artifact struct {
	// Name is how a reader asks for the artifact; unique within the attempt.
	Name string `json:"name"`
	// Path is the file's path relative to the attempt directory, forward
	// slashes, never absolute and never climbing out.
	Path string `json:"path"`
	// SHA256 is the hex digest of the file's bytes and Bytes its length.
	// Publish computes both from the file; a value the writer supplies
	// must agree or the publish is refused. Bytes is a pointer because an
	// empty artifact is a real thing to publish, so zero is a value and
	// nil is the absence.
	SHA256 string `json:"sha256"`
	Bytes  *int64 `json:"bytes"`
	// MediaType is the artifact's media type as the writer declares it.
	// Required: nothing here guesses one from a file name.
	MediaType string `json:"media_type"`
}

// ValidateComponent reports whether s can be a namespace, job or attempt.
func ValidateComponent(field, s string) error {
	if s == "" {
		return fmt.Errorf("%s is required", field)
	}
	if s == "." || s == ".." || !componentPattern.MatchString(s) {
		return fmt.Errorf("%s %q is malformed: want one path component of [A-Za-z0-9._-], not . or ..", field, s)
	}
	return nil
}

// validate checks every field a reader relies on, before the hashes.
func (m *Manifest) validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version %d is not supported; this package writes and reads version %d",
			m.SchemaVersion, SchemaVersion)
	}
	for _, c := range []struct{ field, value string }{
		{"namespace", m.Namespace}, {"job", m.Job}, {"attempt", m.Attempt},
	} {
		if err := ValidateComponent(c.field, c.value); err != nil {
			return err
		}
	}
	for _, c := range []struct{ field, value string }{
		{"framework_id", m.FrameworkID}, {"task_id", m.TaskID},
		{"node_id", m.NodeID}, {"submitter", m.Submitter},
	} {
		if c.value == "" {
			return fmt.Errorf("%s is required", c.field)
		}
	}
	if m.Work.ExitStatus == nil {
		return fmt.Errorf("work.exit_status is required: the work command's exit as the finish command received it")
	}
	// The same three rules medios.ValidateFinishOutcome applies.
	switch m.Finish.State {
	case FinishRan:
		if (m.Finish.ExitCode == nil) == (m.Finish.Signal == nil) {
			return fmt.Errorf("finish.state RAN carries exactly one of exit_code and signal")
		}
	case FinishTimedOut, FinishKilled:
	case FinishDidNotStart, FinishLost:
		if m.Finish.Reason == "" {
			return fmt.Errorf("finish.state %s carries no reason", m.Finish.State)
		}
		if m.Finish.ExitCode != nil || m.Finish.Signal != nil {
			return fmt.Errorf("finish.state %s carries an exit_code or signal; only RAN does", m.Finish.State)
		}
	case "":
		return fmt.Errorf("finish.state is required: one of RAN, TIMED_OUT, KILLED, DID_NOT_START, LOST")
	default:
		return fmt.Errorf("finish.state %q is unknown: want RAN, TIMED_OUT, KILLED, DID_NOT_START or LOST", m.Finish.State)
	}
	for _, c := range []struct{ field, value string }{
		{"started_at", m.StartedAt}, {"finished_at", m.FinishedAt}, {"published_at", m.PublishedAt},
	} {
		if c.value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, c.value); err != nil {
			return fmt.Errorf("%s %q is not RFC 3339", c.field, c.value)
		}
	}
	if len(m.Artifacts) == 0 {
		return fmt.Errorf("artifacts is empty: a manifest names at least one file")
	}
	names := make(map[string]bool, len(m.Artifacts))
	paths := make(map[string]bool, len(m.Artifacts))
	for i, a := range m.Artifacts {
		if a.Name == "" {
			return fmt.Errorf("artifacts[%d].name is required", i)
		}
		if names[a.Name] {
			return fmt.Errorf("artifact name %q is used twice", a.Name)
		}
		names[a.Name] = true
		if err := validateArtifactPath(a.Path); err != nil {
			return fmt.Errorf("artifact %q: %w", a.Name, err)
		}
		if paths[a.Path] {
			return fmt.Errorf("artifact path %q is used twice", a.Path)
		}
		paths[a.Path] = true
		if a.MediaType == "" {
			return fmt.Errorf("artifact %q: media_type is required", a.Name)
		}
	}
	return nil
}

func validateArtifactPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("path is required")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q is absolute; want a path relative to the attempt directory", p)
	case strings.Contains(p, "\\"):
		return fmt.Errorf("path %q uses a backslash; want forward slashes", p)
	case p == ManifestName:
		return fmt.Errorf("path %q is the manifest's own name", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			return fmt.Errorf("path %q has an empty or dot segment", p)
		case "..":
			return fmt.Errorf("path %q climbs out of the attempt directory", p)
		}
	}
	return nil
}
