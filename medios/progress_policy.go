package medios

import (
	"fmt"
	"strings"
	"time"
)

// What a task asks the executor to do about its progress: where to read it
// from, how often, and what to do when the work value stops advancing.
//
// The settings ride on the task rather than on the node or the framework
// because the window that is right for a shard and the window that is right
// for a half-hour research job differ by orders of magnitude, and a window
// shorter than a working job's quietest stretch kills work. medios carries
// them to the executor, the executor reads the job's output, and nothing in
// this library reads a progress source or arms a clock.
//
// Wire field `progress_policy`, field 17 on TaskInfo (DR-040). The controller
// copies an accepted policy onto its own task record, where /state renders it
// beside the task, so the settings a stall kill is explained against are the
// ones the task was launched with.

// DurationInfo is a duration on the wire, as whole nanoseconds. It is the
// encoding medios uses for every duration in a Call, so a framework that
// builds one from a time.Duration gets the controller's own units without
// converting by hand.
type DurationInfo struct {
	Nanoseconds int64 `json:"nanoseconds"`
}

// NewDurationInfo encodes d for the wire.
func NewDurationInfo(d time.Duration) *DurationInfo {
	return &DurationInfo{Nanoseconds: d.Nanoseconds()}
}

// Duration decodes the wire value. A nil receiver reads as zero, which the
// validator refuses as a duration, so a caller cannot mistake an absent
// setting for an instantaneous one by reading through it.
func (d *DurationInfo) Duration() time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(d.Nanoseconds)
}

// ProgressSource names where the executor reads the task's progress from.
type ProgressSource string

const (
	// ProgressSourceStdout reads the task's own stdout file in the sandbox.
	ProgressSourceStdout ProgressSource = "STDOUT"
	// ProgressSourceFile reads the sandbox-relative file named by
	// ProgressPolicy.Path.
	ProgressSourceFile ProgressSource = "FILE"
)

// StallAction names what the executor does when the work value has not
// advanced for a whole stall window.
type StallAction string

const (
	// StallActionReport sends the stall on a TASK_RUNNING update and leaves
	// the task running.
	StallActionReport StallAction = "REPORT"
	// StallActionKill kills the task through its kill-policy escalation and
	// reports TASK_FAILED with ReasonProgressStalled.
	StallActionKill StallAction = "KILL"
)

// ProgressPolicy is one task's progress settings.
//
// A task with no policy reports no progress: there is no implied source and
// no implied window, because reading the wrong file reports another job's
// numbers as this task's progress, and an invented window kills working
// tasks.
type ProgressPolicy struct {
	// Source is required. Both values are legal and neither is a default.
	//
	// Omitted when empty, so a policy built without one reaches the
	// controller as an absent source and is refused by name. Encoding an
	// empty string instead would be worse than useless: medios discards an
	// enum value it cannot resolve on an optional field and the getter then
	// returns the first value in the enum, which is STDOUT, so the task
	// would run with a source nobody chose.
	Source ProgressSource `json:"source,omitempty"`
	// Path is the progress file relative to the task's sandbox, required
	// when Source is ProgressSourceFile and refused otherwise. It may not
	// be absolute and may not contain a `..` component: a progress path
	// that leaves the sandbox reads a file the task does not own.
	Path string `json:"path,omitempty"`
	// Pattern is applied to each line of the source. Capture group one is
	// the work value as a percent; group two, when the pattern has one, is
	// the message.
	//
	// Empty means the executor's own default, which is Cook's convention,
	// `progress: ([0-9]*\.?[0-9]+), (.*)`. The executor owns that value, so
	// a framework that wants it leaves this empty rather than restating it
	// and drifting from it.
	//
	// This library does not compile the pattern. The executor's regular
	// expressions are ECMAScript and Go's are RE2, so a check here would
	// reject patterns the executor accepts and accept patterns it does not.
	// The controller compiles it and refuses a pattern that does not
	// compile, and one with no capture group, with the pattern in the
	// error.
	Pattern string `json:"pattern,omitempty"`
	// SampleInterval is how often the executor reads new bytes from the
	// source. Empty means the executor's default.
	SampleInterval *DurationInfo `json:"sample_interval,omitempty"`
	// StallWindow is how long the work value may stand still before the
	// executor acts. Empty means the stall clock is not armed: no kill and
	// no stall report, whatever else the policy says. There is no default
	// window.
	StallWindow *DurationInfo `json:"stall_window,omitempty"`
	// Action is what happens when a stall window elapses. It is required
	// when StallWindow is set and refused when it is not: killing a stalled
	// task and reporting it are different outcomes for the framework, so
	// the task says which rather than the window implying one.
	Action StallAction `json:"action,omitempty"`
}

// ValidateProgressPolicy reports whether p is one medios will accept,
// mirroring the check the node makes when it reads the policy off the task.
// A nil policy is valid: a task need not report progress.
//
// The error names the field and the value so a framework can return it to
// its caller unchanged. Validating here means a framework rejects a bad
// policy where its user can read the reason, rather than seeing the task
// fail to launch later.
func ValidateProgressPolicy(p *ProgressPolicy) error {
	if p == nil {
		return nil
	}

	switch p.Source {
	case ProgressSourceStdout:
		if p.Path != "" {
			return fmt.Errorf("TaskInfo.ProgressPolicy.Path is %q with Source %q; "+
				"a path is read only with Source %q",
				p.Path, ProgressSourceStdout, ProgressSourceFile)
		}
	case ProgressSourceFile:
		if err := validateProgressPath(p.Path); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("TaskInfo.ProgressPolicy.Source is required; it must be %q or %q",
			ProgressSourceStdout, ProgressSourceFile)
	default:
		return fmt.Errorf("TaskInfo.ProgressPolicy.Source is %q; it must be %q or %q",
			p.Source, ProgressSourceStdout, ProgressSourceFile)
	}

	if p.SampleInterval != nil && p.SampleInterval.Nanoseconds <= 0 {
		return fmt.Errorf("TaskInfo.ProgressPolicy.SampleInterval is %d ns; "+
			"it must be a positive duration", p.SampleInterval.Nanoseconds)
	}

	if p.StallWindow != nil && p.StallWindow.Nanoseconds <= 0 {
		return fmt.Errorf("TaskInfo.ProgressPolicy.StallWindow is %d ns; "+
			"it must be a positive duration", p.StallWindow.Nanoseconds)
	}

	if p.StallWindow == nil {
		if p.Action != "" {
			return fmt.Errorf("TaskInfo.ProgressPolicy.Action is %q without a StallWindow, "+
				"so there is no stall to act on", p.Action)
		}
		return nil
	}

	switch p.Action {
	case StallActionReport, StallActionKill:
		return nil
	case "":
		return fmt.Errorf("TaskInfo.ProgressPolicy.StallWindow is set without an Action; "+
			"it must be %q or %q", StallActionReport, StallActionKill)
	default:
		return fmt.Errorf("TaskInfo.ProgressPolicy.Action is %q; it must be %q or %q",
			p.Action, StallActionReport, StallActionKill)
	}
}

func validateProgressPath(path string) error {
	if path == "" {
		return fmt.Errorf("TaskInfo.ProgressPolicy.Path is required with Source %q; "+
			"it is a path relative to the task's sandbox", ProgressSourceFile)
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("TaskInfo.ProgressPolicy.Path is %q; "+
			"a progress path is relative to the task's sandbox", path)
	}
	for _, component := range strings.Split(path, "/") {
		if component == ".." {
			return fmt.Errorf("TaskInfo.ProgressPolicy.Path is %q; "+
				"a progress path may not leave the task's sandbox", path)
		}
	}
	return nil
}
