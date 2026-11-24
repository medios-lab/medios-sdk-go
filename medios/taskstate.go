package medios

// TaskState is a task's lifecycle state exactly as the controller puts it on the
// wire (TaskStatus.state): the values are the literal TASK_* strings, 1:1 with
// the medios TaskState enum. There is no relabeling, so nothing needs to be
// stripped or translated, and the value is unambiguous (TASK_RUNNING, not a
// bare "Running" that could mean anything).
//
// Frameworks layer their own richer lifecycle (pre-launch states, etc.) in their
// own models and map a TaskState in; that lifecycle is the framework's domain,
// these states are the wire's.
type TaskState string

const (
	TaskStaging        TaskState = "TASK_STAGING"
	TaskStarting       TaskState = "TASK_STARTING"
	TaskRunning        TaskState = "TASK_RUNNING"
	TaskKilling        TaskState = "TASK_KILLING"
	TaskFinished       TaskState = "TASK_FINISHED"
	TaskFailed         TaskState = "TASK_FAILED"
	TaskKilled         TaskState = "TASK_KILLED"
	TaskError          TaskState = "TASK_ERROR"
	TaskLost           TaskState = "TASK_LOST"
	TaskDropped        TaskState = "TASK_DROPPED"
	TaskUnreachable    TaskState = "TASK_UNREACHABLE"
	TaskGone           TaskState = "TASK_GONE"
	TaskGoneByOperator TaskState = "TASK_GONE_BY_OPERATOR"
	TaskUnknown        TaskState = "TASK_UNKNOWN"
)

// activeStates: the task is in flight.
var activeStates = map[TaskState]bool{
	TaskStaging: true, TaskStarting: true, TaskRunning: true,
	TaskKilling: true, TaskUnreachable: true,
}

// terminalStates: the run has ended; no further transitions.
var terminalStates = map[TaskState]bool{
	TaskFinished: true, TaskFailed: true, TaskKilled: true, TaskError: true,
	TaskLost: true, TaskDropped: true, TaskGone: true, TaskGoneByOperator: true,
	TaskUnknown: true,
}

// failureStates: the run ended badly (not Finished, not an intentional Kill).
// TASK_LOST has a real home here, instead of the old relabel that fell through
// to Unknown.
var failureStates = map[TaskState]bool{
	TaskFailed: true, TaskError: true, TaskLost: true, TaskDropped: true,
	TaskGone: true, TaskGoneByOperator: true, TaskUnknown: true,
}

// IsActive reports whether the task is in flight.
func (s TaskState) IsActive() bool { return activeStates[s] }

// IsTerminal reports whether the run has reached a final state.
func (s TaskState) IsTerminal() bool { return terminalStates[s] }

// IsFailure reports whether the run ended in a failure state.
func (s TaskState) IsFailure() bool { return failureStates[s] }
