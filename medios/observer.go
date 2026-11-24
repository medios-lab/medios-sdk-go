package medios

import (
	"fmt"
	"regexp"
)

// A second command the executor runs in the task's sandbox beside the task,
// whose output becomes what the task says about itself.
//
// A task reports its own progress only if it was written to (DR-038). An
// observer is how a job that says nothing still gets a status message: the
// executor runs the observer alongside the task, reads its stdout, and the
// newest line, prefixed with the observer's name, becomes the task's progress
// message on the next TASK_RUNNING.
//
// medios does not know what an observer watches, how often it looks, what it
// costs, or what credential it uses. The observer's working directory is the
// sandbox, so one that wants the task's output reads `./stdout`; everything
// else is the observer program's own flags. A new kind of observer is a new
// program, not a new field.
//
// Wire field `observers`, field 18 on TaskInfo (DR-041, medios#562). An
// executor that predates the field ignores it and the task runs unobserved,
// so a framework cannot read the absence of observer lines as a verdict.

// Observer rules this library enforces, mirroring the controller's check.
//
// Invariants, all three stated by DR-041 and refused by the controller in
// validateObservers (src/controller/validation.cpp). They are checked here so
// a framework fails before submit, where its own caller can read the reason,
// rather than seeing a TASK_ERROR later. The refusals are the controller's
// own wording, in the controller's own order, so a framework that forwards
// one reads the same sentence whichever side refused.
const (
	// observerNameCharset is the characters TaskObserver.Name may use. The
	// name prefixes the observer's lines in the task record, which is what
	// tells several observers on one task apart.
	observerNameCharset = `^[a-z0-9-]+$`

	// observerNameMaxBytes bounds TaskObserver.Name. The controller counts
	// bytes, not runes, and nothing outside the charset reaches the length
	// check in practice; counting runes here would accept a name the
	// controller refuses.
	observerNameMaxBytes = 32
)

// maxObserversPerTask is how many observers one task may carry.
//
// Parameter, provisional, owned by the controller, where it is
// MAX_OBSERVERS_PER_TASK; this value restates the controller's limit so a
// framework sees the same refusal. 8 is DR-041's choice and is not derived
// from a measurement: nothing has run a task with more than one observer, so
// neither the token cost nor the executor's pipe handling at the limit has
// been observed. Raising it is a research question on DR-041, not a knob a
// framework turns.
const maxObserversPerTask = 8

var observerName = regexp.MustCompile(observerNameCharset)

// TaskObserver is one observer on a task.
type TaskObserver struct {
	// Name prefixes each of this observer's lines in the task record. It
	// must match [a-z0-9-]{1,32} and must be unique among the task's
	// observers.
	Name string `json:"name"`
	// Command is what the executor runs. It runs in the task's sandbox as a
	// child of the executor, after the task's process exists, with the
	// task's uid and the executor's environment. The task's own process
	// does not get that environment, so a credential the node gave its
	// observers is not readable by the job.
	//
	// The command's cost counts against the task's own allocation, because
	// it runs inside the task's container or cgroup: a profile that adds an
	// observer has to add its CPU and memory to the request.
	//
	// An observer has no kill authority. Its lines reach the task record
	// only through the progress message, never the work value, so nothing it
	// says can create a stall or mask one. It is not restarted, and it does
	// not survive an executor or node restart.
	Command *CommandInfo `json:"command"`
}

// WithObserver appends an observer to the task, refusing a name or a count
// the controller would refuse.
//
// The refusal is the point: a framework that builds an observer list by hand
// learns here that two of them are called `haiku`, rather than at a node
// after the offer is already accepted.
func (t *TaskInfo) WithObserver(name string, command CommandInfo) error {
	// The controller's order: the count first, then the name, then the
	// command.
	if len(t.Observers)+1 > maxObserversPerTask {
		return fmt.Errorf("Invalid observers: the task carries %d observers; the limit is %d",
			len(t.Observers)+1, maxObserversPerTask)
	}
	if err := validateObserverName(name); err != nil {
		return err
	}
	for _, existing := range t.Observers {
		if existing.Name == name {
			return fmt.Errorf("Invalid observers: 'name' '%s' is used by more than one "+
				"observer on this task, so their lines could not be told apart", name)
		}
	}
	if command.Value == "" {
		return fmt.Errorf("Invalid observers: 'command' is required on observer '%s'", name)
	}

	t.Observers = append(t.Observers, TaskObserver{Name: name, Command: &command})
	return nil
}

// ValidateTaskObservers reports whether observers is a list medios will
// accept: every observer named and commanded, every name in bounds, no name
// twice, and no more than maxObserversPerTask of them. An empty list is
// valid, because a task need not be observed.
//
// WithObserver applies the same rules one observer at a time. This is for a
// framework that assembled the list another way, a decoded TaskInfo among
// them.
func ValidateTaskObservers(observers []TaskObserver) error {
	if len(observers) == 0 {
		return nil
	}

	if len(observers) > maxObserversPerTask {
		return fmt.Errorf("Invalid observers: the task carries %d observers; the limit is %d",
			len(observers), maxObserversPerTask)
	}

	seen := make(map[string]bool, len(observers))
	for _, o := range observers {
		if err := validateObserverName(o.Name); err != nil {
			return err
		}
		if seen[o.Name] {
			return fmt.Errorf("Invalid observers: 'name' '%s' is used by more than one "+
				"observer on this task, so their lines could not be told apart", o.Name)
		}
		seen[o.Name] = true

		if o.Command == nil || o.Command.Value == "" {
			return fmt.Errorf("Invalid observers: 'command' is required on observer '%s'",
				o.Name)
		}
	}
	return nil
}

func validateObserverName(name string) error {
	if name == "" {
		return fmt.Errorf("Invalid observers: 'name' is required on each observer")
	}
	if len(name) > observerNameMaxBytes {
		return fmt.Errorf("Invalid observers: 'name' '%s' is %d bytes; the limit is %d",
			name, len(name), observerNameMaxBytes)
	}
	if !observerName.MatchString(name) {
		return fmt.Errorf("Invalid observers: 'name' '%s' is outside [a-z0-9-]", name)
	}
	return nil
}
