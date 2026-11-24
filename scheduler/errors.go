package scheduler

import (
	"errors"
	"fmt"
)

// CallError is a call the controller answered with a non-success status.
//
// It carries the status because the status is the only thing that says whether
// the call took effect, and callers need that to decide whether rolling back
// is safe. Use Rejected rather than reading StatusCode directly.
type CallError struct {
	// Call is the call type, e.g. "ACCEPT".
	Call string
	// StatusCode is what came back.
	StatusCode int
	// Body is the response body, trimmed.
	Body string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("scheduler: %s got HTTP %d: %s", e.Call, e.StatusCode, e.Body)
}

// Rejected reports whether the controller answered with a client error, which
// is the strongest evidence this library can offer that a call did not take
// effect. It is evidence, not proof, and the difference matters.
//
// True only for a 4xx. False for everything else, which means "do not conclude
// anything", covering success, ambiguity, transport failure and every 5xx.
//
// The 5xx case is the one worth stating because it looks like a failure and is
// not proof of one. A 500 can come from a controller that crashed after
// acting. A 502 or 504 can come from a gateway that forwarded the call, saw
// the controller dispatch it, and then failed to obtain or deliver the answer.
//
// What a true result assumes, and does not verify:
//
//   - The answer came from the controller rather than an intermediary. A proxy
//     can produce a 404 or a 409 of its own after the controller acted.
//   - The call was submitted once. If a layer above retried, a 409 on the
//     second attempt may be the first attempt having succeeded.
//
// Neither is checkable from here, so a caller that cannot vouch for both
// should treat true the same as false. Use this to decide whether to release
// local state promptly, never to decide that remote work does not exist: keep
// a record of anything submitted regardless, so a wrong answer here costs a
// reconciliation round rather than a forgotten task.
func Rejected(err error) bool {
	var ce *CallError
	if !errors.As(err, &ce) {
		return false
	}
	return ce.StatusCode >= 400 && ce.StatusCode < 500
}
