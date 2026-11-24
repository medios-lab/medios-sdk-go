package medios

// The progress a task reports about itself.
//
// Between launch and exit a task says nothing about how far it has got, so a
// job that has stopped progressing looks at the controller exactly like one
// that is working. Progress rides in the status update because that is the
// acknowledged channel the controller retains the latest value of per task;
// the executor MESSAGE channel guarantees no delivery at all.
//
// medios holds the last reported progress and frameworks forward it. A
// framework must not interpret a progress value, derive completion from it,
// or fill a missing one.
//
// Wire field `progress`, field 20 on TaskStatus, added to medios by
// medios#548 (DR-038, decision A of medios#544). A controller that predates
// the field drops it silently: the node's JSON-to-protobuf parse skips an
// unrecognised key without a diagnostic. So a nil Progress means "this
// controller or this task said nothing", never zero and never complete.

// Progress is one progress report about a task.
//
// Every field is optional except Sequence, and absence means unknown. A
// reader that treats a nil Fraction as 0 reports a stalled job as one that
// has not started, which is a different claim.
type Progress struct {
	// Fraction is how far through the work the task believes it is, in 0 to
	// 1, so a framework rendering a percent multiplies by 100. The
	// controller refuses a report whose fraction is outside that range or is
	// not finite, dropping the field and delivering the rest of the update.
	//
	// Nil means the task reported no fraction.
	Fraction *float64 `json:"fraction,omitempty"`
	// Step and Total are the work counter in the job's own declared units:
	// shards done of shards total, epochs, rows, whatever the job counts.
	// They are not derivable from Fraction and Fraction is not derivable
	// from them; a job may report either, both or neither.
	Step  *uint64 `json:"step,omitempty"`
	Total *uint64 `json:"total,omitempty"`
	// Message is the job's own short description of what it is doing. It
	// carries no structure and no framework parses it. The controller bounds
	// its length and drops a report that exceeds the bound.
	Message string `json:"message,omitempty"`
	// Sequence increases by one per report within a task attempt, so a
	// reader can tell a resent older report from a newer one and can see
	// that reports were lost. It restarts at the first report of a new
	// attempt, so it orders reports within an attempt and nothing across
	// attempts.
	Sequence uint64 `json:"sequence"`
	// ObservedAt is when the executor read this value off the job, in
	// seconds since the epoch on the executor's clock, field 6 on the
	// Progress message. The controller records it and never rewrites it.
	//
	// It is what tells a stale report from a fresh one. A reconciliation
	// reply carries a current TaskStatus.Timestamp beside whatever
	// ObservedAt the last report had, which may be hours old, so a reader
	// comparing the two learns the age of the observation rather than the
	// age of the reply.
	//
	// The clock is the executor's, and nothing reconciles it with the
	// controller's: medios does not refuse a report for being in the future,
	// because that would turn a clock fault into a lost observation. A
	// framework that cares about skew has to notice it.
	//
	// Nil means the reporter set none, which includes every controller that
	// predates the field.
	ObservedAt *float64 `json:"observed_at,omitempty"`
}

// ReasonProgressStalled is the TaskStatus.Reason on a TASK_FAILED sent by an
// executor that killed its own task because the task's work counter stopped
// advancing for longer than the task's configured stall window.
//
// It distinguishes a wedged task from a crashed one without reading a log.
// The accompanying TaskStatus.Message carries the last progress value seen
// and how long the task was stalled. Stall-killing is off unless the task's
// configuration names a stall window; there is no default window, because a
// window shorter than a working job's quietest stretch kills work.
//
// A plain string rather than an enumerated constant, matching
// TaskStatus.Reason: which reasons a controller sends is its contract to
// state. The value is 35 in the medios enum (medios#548); a controller that
// predates it discards the unrecognised value and the status arrives with no
// reason set, so the message is the only place the stall shows.
const ReasonProgressStalled = "REASON_PROGRESS_STALLED"

// Fold returns the progress a framework should hold for a task after applying
// one status update, given the progress it holds now. The receiver may be nil,
// so a framework can write `rec.Progress = rec.Progress.Fold(&st)` for every
// update without a special case for the first one.
//
// Two rules, and both exist because a status update is not a progress report:
//
//   - An update that carries no progress keeps the progress already held. Most
//     status updates say nothing about progress — a reconciliation reply, a
//     state transition, a terminal status — and clearing on those would make a
//     working task's progress blink out and reappear. A terminal status is not
//     special: the last value stays beside the terminal state and reason, which
//     is what tells a stall kill apart from a crash.
//   - An update whose progress has a higher Sequence than the held value
//     replaces it. An equal or lower Sequence is a replay of a report already
//     seen, and is ignored; node retransmission of an unacknowledged update
//     makes that an ordinary event, not a fault.
//
// The controller carries progress forward on its own task record the same way
// (medios#548), so a framework that folds this way shows what `/state` shows.
// A framework that cleared on an absent value would disagree with the
// controller about the same task.
//
// Fold returns one of its two inputs and allocates nothing: either the
// receiver, meaning the held value stands, or update.Progress, meaning the
// report was taken. A caller holding progress in its own type can compare the
// result against update.Progress to learn which side won, and so delegate the
// rule here rather than restating it.
//
// Fold never interprets a progress value: it does not derive completion from a
// fraction, fill a missing field from another report, or compare ObservedAt.
// Sequence orders reports within a task attempt only, so a framework that
// relaunches a task starts a fresh record rather than folding the new attempt's
// reports onto the old attempt's.
func (p *Progress) Fold(update *TaskStatus) *Progress {
	if update == nil || update.Progress == nil {
		return p
	}
	if p != nil && update.Progress.Sequence <= p.Sequence {
		return p
	}
	return update.Progress
}
