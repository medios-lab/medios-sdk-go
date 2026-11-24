package medios

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A task principal names the submitter a task runs on behalf of. It is a
// declared value: the framework forwards what its own caller said, medios
// records it beside the framework principal and the OS user, and nothing in
// this library verifies it against an identity provider.
//
// The shape is kind:name with three kinds. operator is a human, team is a
// Claude Code session holding a team identity, run is an agent run that
// submits work of its own, e.g. operator:alice, team:research,
// run:research-164-s5-r3. The kind prefix is the only structure; the name
// is opaque and at most PrincipalNameMaxLength characters. The controller
// checks the same shape and bound where the principal enters and treats it
// as a string elsewhere.
var principalPattern = regexp.MustCompile(`^(operator|team|run):[A-Za-z0-9._-]+$`)

// PrincipalNameMaxLength is the longest name accepted after the kind, in
// characters. Parameter: a principal is a label for a human or an agent run
// id, and 64 fits run:research-164-s5-r3-shaped ids with room while
// bounding the string that lands in every /state render, log line and
// status update. The controller enforces the same value.
const PrincipalNameMaxLength = 64

// ErrPrincipalRequired is returned by NewTaskInfo when the principal is
// empty. There is no fallback to the framework's principal or its user:
// those name the framework, not the submitter.
var ErrPrincipalRequired = errors.New(
	"TaskInfo.Principal is required: pass the submitter's principal as kind:name")

// ValidatePrincipal reports whether p is a well-formed task principal. The
// error names the field and the value so a framework can return it to its
// caller unchanged.
func ValidatePrincipal(p string) error {
	if p == "" {
		return ErrPrincipalRequired
	}
	if !principalPattern.MatchString(p) {
		return fmt.Errorf("TaskInfo.Principal %q is malformed: want kind:name with kind "+
			"operator, team or run and a name of [A-Za-z0-9._-]", p)
	}
	if n := len(p) - (strings.IndexByte(p, ':') + 1); n > PrincipalNameMaxLength {
		return fmt.Errorf("TaskInfo.Principal has a name of %d characters after the kind; "+
			"the limit is %d", n, PrincipalNameMaxLength)
	}
	return nil
}

// NewTaskInfo builds the TaskInfo for one launch. It is the only path that
// checks the principal, so a framework that launches through it cannot send
// a task without one. Container and Labels are set on the result when the
// task needs them.
func NewTaskInfo(
	principal, name string,
	taskID, nodeID ID,
	resources []Resource,
	command CommandInfo,
) (TaskInfo, error) {
	if err := ValidatePrincipal(principal); err != nil {
		return TaskInfo{}, err
	}
	return TaskInfo{
		Name:      name,
		TaskID:    taskID,
		NodeID:    nodeID,
		Principal: principal,
		Resources: resources,
		Command:   command,
	}, nil
}
