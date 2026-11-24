package medios

import (
	"errors"
	"fmt"
	"time"
)

// A finish hook on a task: a command the executor runs after the work
// command exits, with a cap on how long it may run, and the hook's outcome
// on the terminal status apart from the task's own state.
//
// Wire shape, from DR-042 (medios#580) as medios#581 lands it:
// TaskFinish {command: CommandInfo = 1, cap: DurationInfo = 2};
// TaskInfo.finish = 19; FinishStatus {state = 1, exit_code = 2,
// signal = 3, reason = 4, limit = 5, duration = 6}; TaskStatus.finish =
// 21. State is encoded by name, RAN, TIMED_OUT, KILLED, DID_NOT_START or
// LOST;
// this package never carries the enum's numbers. command.value is
// required: the launcher runs the value on both the shell and the argv
// path, so arguments alone are refused.
// What the executor does with the hook, its environment, its streams and
// its timers, is DR-042's to state and the medios executor's to prove;
// nothing here claims it. The rules this package enforces are DR-042's
// validation: a command, a cap, a positive cap, a cap within the task's
// max_completion_time. Until the medios proto carries the fields, nothing
// below has been exercised against a node; the wire-shape tests pin the
// names so a divergence fails here first.

// TaskFinish is the finish hook on a task.
type TaskFinish struct {
	// Command is what the executor runs after the work command exits.
	Command *CommandInfo `json:"command"`
	// Cap bounds the hook's run, counted inside the task's own ceiling. It
	// has no default (DR-042: an upload and a checkpoint differ by orders
	// of magnitude, and a wrong cap cuts a hook off or holds a slot), and
	// it may not exceed MaxCompletionTime when the task has one.
	Cap *DurationInfo `json:"cap"`
}

// WithFinish sets the task's finish command and its cap, refusing what the
// controller would refuse: an empty command, a cap that is not positive, a
// cap larger than the task's MaxCompletionTime when one is set, and a
// second finish on a task that has one. Set MaxCompletionTime before
// WithFinish so the cap is checked against it.
//
// The command is copied in full, its environment and URIs included, so the
// task holds a snapshot: a caller that edits its CommandInfo afterwards
// does not change the finish the builder accepted. A refused call leaves
// the task unchanged.
func (t *TaskInfo) WithFinish(cmd CommandInfo, cap time.Duration) error {
	if t.Finish != nil {
		return fmt.Errorf("Invalid finish: the task already has a finish command; a task has one")
	}
	f := &TaskFinish{Command: cloneCommand(cmd), Cap: NewDurationInfo(cap)}
	if err := validateFinish(f, t.MaxCompletionTime); err != nil {
		return err
	}
	t.Finish = f
	return nil
}

// cloneCommand copies a CommandInfo and everything it points at.
func cloneCommand(cmd CommandInfo) *CommandInfo {
	c := cmd
	if cmd.Environment != nil {
		env := &Environment{Variables: append([]EnvVar(nil), cmd.Environment.Variables...)}
		c.Environment = env
	}
	if cmd.URIs != nil {
		c.URIs = append([]URI(nil), cmd.URIs...)
		for i := range c.URIs {
			if c.URIs[i].Extract != nil {
				v := *c.URIs[i].Extract
				c.URIs[i].Extract = &v
			}
		}
	}
	return &c
}

// ValidateTaskFinish reports whether the task's finish is one medios will
// accept, against the task's own MaxCompletionTime. A task without a finish
// is valid, because a task need not have one. This is for a framework that
// assembled the task another way, a decoded TaskInfo among them; WithFinish
// applies the same rules as it builds.
func ValidateTaskFinish(t *TaskInfo) error {
	if t == nil || t.Finish == nil {
		return nil
	}
	return validateFinish(t.Finish, t.MaxCompletionTime)
}

// validateFinish is the controller's check, in the controller's order:
// command, cap present, cap positive, cap within the ceiling.
func validateFinish(f *TaskFinish, ceiling *DurationInfo) error {
	if f.Command == nil || f.Command.Value == "" {
		return fmt.Errorf("Invalid finish: 'command.value' is required; the launcher runs the value on both the shell and the argv path")
	}
	if f.Cap == nil {
		return fmt.Errorf("Invalid finish: 'cap' is required; it has no default")
	}
	if f.Cap.Duration() <= 0 {
		return fmt.Errorf("Invalid finish: 'cap' must be positive; it has no default (got %s)", f.Cap.Duration())
	}
	if ceiling != nil && f.Cap.Duration() > ceiling.Duration() {
		return fmt.Errorf("Invalid finish: 'cap' %s is larger than the task's max_completion_time %s; "+
			"the hook runs inside the task's ceiling", f.Cap.Duration(), ceiling.Duration())
	}
	return nil
}

// FinishState is FinishStatus.State, encoded as the enum's name.
type FinishState string

const (
	// FinishRan: the hook exited on its own; ExitCode or Signal says how.
	FinishRan FinishState = "RAN"
	// FinishTimedOut: the hook was killed at Limit.
	FinishTimedOut FinishState = "TIMED_OUT"
	// FinishKilled: a kill or shutdown arrived while the hook ran.
	FinishKilled FinishState = "KILLED"
	// FinishDidNotStart: the hook never started; Reason says why.
	FinishDidNotStart FinishState = "DID_NOT_START"
	// FinishLost: the hook ran and its outcome could not be read; Reason
	// carries the read error, Limit and Duration are set, and neither
	// ExitCode nor Signal is. A valid record: Ran and Signalled answer
	// false with no error.
	FinishLost FinishState = "LOST"
)

// FinishStatus is the hook's outcome on the terminal TaskStatus, wire field
// `finish` (DR-042). It is typed so that a framework reads it as a value
// and never parses the status message for it.
//
// It decodes as sent, whatever it says: a status must always decode, and
// one unreadable field must not cost a framework the whole terminal
// status. Ran and Signalled are the gate: they refuse, with
// ErrFinishMalformed, every shape ValidateFinishOutcome refuses, a state
// this library does not know among them, so a record is never read as a
// confident answer it does not support. A framework that wants to check
// up front calls ValidateFinishOutcome itself.
type FinishStatus struct {
	State FinishState `json:"state"`
	// ExitCode is set when the hook ran and exited; Signal when it ran and
	// was signalled. Exactly one of them is set for RAN.
	ExitCode *int32 `json:"exit_code,omitempty"`
	Signal   *int32 `json:"signal,omitempty"`
	// Reason says why for DID_NOT_START and LOST.
	Reason string `json:"reason,omitempty"`
	// Limit is the bound that applied to the hook; Duration is how long it
	// ran, when it ran.
	Limit    *DurationInfo `json:"limit,omitempty"`
	Duration *DurationInfo `json:"duration,omitempty"`
}

// ErrFinishMalformed wraps the reason when Ran or Signalled are asked about
// a record that is not a sound finish outcome.
var ErrFinishMalformed = errors.New("finish outcome is malformed")

// Ran reports the hook's exit code when it ran and exited on its own. The
// second value is false for a signalled hook and for every other state, so
// a caller cannot read a timeout, a kill or a signal as exit 0. A record
// ValidateFinishOutcome refuses, RAN with neither or both of exit code and
// signal or an unknown state among them, returns false and an error
// wrapping ErrFinishMalformed, never a confident answer.
func (f *FinishStatus) Ran() (int32, bool, error) {
	if f == nil {
		return 0, false, nil
	}
	if err := ValidateFinishOutcome(f); err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrFinishMalformed, err)
	}
	if f.State != FinishRan || f.ExitCode == nil {
		return 0, false, nil
	}
	return *f.ExitCode, true, nil
}

// Signalled reports the signal that ended a hook that ran, when one did,
// with the same refusal as Ran on a record that fails validation.
func (f *FinishStatus) Signalled() (int32, bool, error) {
	if f == nil {
		return 0, false, nil
	}
	if err := ValidateFinishOutcome(f); err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrFinishMalformed, err)
	}
	if f.State != FinishRan || f.Signal == nil {
		return 0, false, nil
	}
	return *f.Signal, true, nil
}

// ValidateFinishOutcome refuses a status this library does not understand:
// an unknown state, RAN with neither or both of exit_code and signal,
// DID_NOT_START or LOST without a reason or with an exit_code or signal. Ran and Signalled apply it; a framework
// that wants to check a decoded or hand-built record up front applies it
// itself. Nil is valid.
func ValidateFinishOutcome(f *FinishStatus) error {
	if f == nil {
		return nil
	}
	switch f.State {
	case FinishRan:
		if (f.ExitCode == nil) == (f.Signal == nil) {
			return fmt.Errorf("finish outcome RAN carries exactly one of exit_code and signal")
		}
	case FinishTimedOut, FinishKilled:
	case FinishDidNotStart, FinishLost:
		if f.Reason == "" {
			return fmt.Errorf("finish outcome %s carries no reason", f.State)
		}
		if f.ExitCode != nil || f.Signal != nil {
			return fmt.Errorf("finish outcome %s carries an exit_code or signal; only RAN does", f.State)
		}
	case "":
		return fmt.Errorf("finish outcome has no state")
	default:
		return fmt.Errorf("finish outcome state %q is unknown; this library knows RAN, TIMED_OUT, KILLED, DID_NOT_START and LOST", f.State)
	}
	return nil
}
