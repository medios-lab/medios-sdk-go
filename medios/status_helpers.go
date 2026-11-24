package medios

import (
	"regexp"
	"strconv"
)

var (
	// workExitCodePatterns match the Medios command executor's terminal status
	// messages carrying a work process exit status, ordered from most specific
	// ("Command exited with status N") to general ("exited with status/code N",
	// "exit status/code N").
	workExitCodePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bCommand exited with status (-?\d+)\b`),
		regexp.MustCompile(`(?i)\bexited with (?:status|code) (-?\d+)\b`),
		regexp.MustCompile(`(?i)\bexit (?:status|code) (-?\d+)\b`),
	}

	// storeUnitRefusedPatterns match the Medios controller's DR-036 store unit
	// contention messages on TASK_ERROR / REASON_TASK_INVALID when all declared
	// reader/writer units are currently held by running tasks. Static declaration
	// errors ("declares 0 writer units", "is not on node") do not match.
	storeUnitRefusedPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?:Task uses store|Store) '([^']+)' (?:which )?has no free (?:readers|writers) unit`),
		regexp.MustCompile(`(?:Task uses store|Store) '([^']+)' (?:which )?is requested exclusive and has`),
	}
)

// WorkExitCode returns the work command's process exit code from a terminal
// TaskStatus and true when the exit code is known.
//
// For TaskFinished, the work process exited 0, so WorkExitCode returns (0, true).
// For TaskFailed, WorkExitCode extracts the integer exit status from the
// command executor's message (e.g. "Command exited with status 1"); if the
// process died from a signal ("Command terminated with signal Aborted") or
// failed before exit without a status code, it returns (0, false).
// For every other state (TaskKilled, TaskLost, TaskError, TaskRunning, ...),
// it returns (0, false).
func (s TaskStatus) WorkExitCode() (int, bool) {
	switch s.State {
	case TaskFinished:
		return 0, true
	case TaskFailed:
		for _, re := range workExitCodePatterns {
			m := re.FindStringSubmatch(s.Message)
			if len(m) == 2 {
				code, err := strconv.Atoi(m[1])
				if err == nil {
					return code, true
				}
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

// StoreUnitRefused reports whether the status is a controller launch refusal
// caused by transient store unit contention (DR-036: no free reader/writer unit
// or exclusive request while units are held) and returns the store name from
// the controller's message.
//
// It matches only when s.State == TaskError and s.Reason is empty or
// "REASON_TASK_INVALID". Static validation refusals (such as a read-only store
// declaring 0 writer units or a store absent from the node) return ("", false).
func (s TaskStatus) StoreUnitRefused() (store string, ok bool) {
	if s.State != TaskError {
		return "", false
	}
	if s.Reason != "" && s.Reason != "REASON_TASK_INVALID" {
		return "", false
	}
	for _, re := range storeUnitRefusedPatterns {
		m := re.FindStringSubmatch(s.Message)
		if len(m) == 2 && m[1] != "" {
			return m[1], true
		}
	}
	return "", false
}
