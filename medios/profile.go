package medios

import (
	"fmt"
	"time"
)

// A job names a profile and the profile expands into the settings a job of
// that kind needs (decision E of the task-and-executor packet, 2026-10-03).
// The point is that a job author says what kind of work this is, which they
// know, instead of a sample interval and a stall window, which they do not.
//
// Only progress settings expand from a profile today. A profile is not a
// resource class, a queue or a priority.

// Profile is the kind of work a job does.
type Profile string

const (
	// ProfileTraining is a batch job that runs to completion and reports how
	// far through it is.
	ProfileTraining Profile = "training"
	// ProfileService is a long-running process with no end and so no notion
	// of how far through it is.
	ProfileService Profile = "service"
)

// Sample interval for a training job: how often the executor reads new
// progress bytes.
//
// Parameter, provisional. 1 second is Cook's environment default, the only
// sampled-progress interval with a deployment behind it. Nothing here has
// measured the cost of reading more or less often.
const trainingSampleInterval = 1 * time.Second

// Stall window for a training job: how long the work value may stand still
// before the executor kills the task.
//
// Parameter, provisional. 120 seconds sits below the 300-second runtime cap
// the batch jobs run under, so a stall can actually fire inside a capped run;
// a window at or above the cap would never be reached and the setting would
// look armed while doing nothing. It is not derived from any observed stall,
// and the first run of real jobs should correct it: the stretches a working
// job goes quiet for are the measurement this needs.
const trainingStallWindow = 120 * time.Second

// ProgressPolicyForProfile returns the progress settings a profile expands
// into, or an error naming a profile it does not know. A nil policy with a
// nil error is a profile that expects no progress, which is not a failure.
//
// A framework may take the result and change a field; the defaults are
// provisional and the task's own settings win.
//
// No profile expands into an observer (DR-041). A ProfileTraining task gets
// progress settings and nothing else: an observer spends tokens on every
// look, and a cost a job author did not ask for should not arrive with a
// profile name. A framework that wants one appends it itself, with
// TaskInfo.WithObserver, and adds its CPU and memory to the task's request.
func ProgressPolicyForProfile(p Profile) (*ProgressPolicy, error) {
	switch p {
	case ProfileTraining:
		// Pattern is left empty so the executor's default applies; the
		// executor owns that convention.
		return &ProgressPolicy{
			Source:         ProgressSourceStdout,
			SampleInterval: NewDurationInfo(trainingSampleInterval),
			StallWindow:    NewDurationInfo(trainingStallWindow),
			Action:         StallActionKill,
		}, nil
	case ProfileService:
		// A service has no completion to be a fraction of, so there is
		// nothing to stall against and no source worth reading. Reporting a
		// service as stalled because it printed nothing for two minutes
		// would kill a healthy process.
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown medios.Profile %q; it must be %q or %q",
			p, ProfileTraining, ProfileService)
	}
}
