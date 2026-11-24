package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// observationMaxBytes bounds one observation line, in bytes, excluding the
// terminating newline.
//
// Type: parameter. Owner: this binary. DR-041 has the executor truncate an
// observer's line to DR-038's 4096-byte progress message bound, so nothing
// requires this observer to be shorter. 1000 bytes is the default because
// the message is a headline and not a log, and because the model is capped
// at 120 output tokens: a longer line means the model ignored the
// instruction rather than that the reader needs more room.
const observationMaxBytes = 1000

// observationUnparsedPrefixBytes is how much of an unrecognised model reply
// is kept in the observation, in bytes.
//
// Type: parameter. Owner: this binary. Enough to see what the model said
// instead of a verdict, short enough that a runaway reply does not fill the
// task's progress message.
const observationUnparsedPrefixBytes = 200

// defaultMaxConsecutiveErrors is how many failed calls in a row end the
// observer.
//
// Type: default of the --max-consecutive-errors parameter, in calls. A
// rejected or expired key fails every call, so a bounded run stops a
// task-long stream of identical error lines; 5 is long enough that a rate
// limit or a provider blip is ridden out and short enough that a dead
// credential is one short burst on the task record.
const defaultMaxConsecutiveErrors = 5

// observationLine renders one observation as the executor reads it: a single
// line with embedded newlines flattened and the whole line truncated.
//
// No name prefix. DR-041 has the executor prefix each line with the
// observer's name from the TaskObserver message; an observer that prefixed
// its own lines would be named twice.
func observationLine(body string) string {
	body = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(body)
	return truncateBytes(strings.TrimSpace(body), observationMaxBytes)
}

// truncateBytes cuts s to at most n bytes without leaving a partial rune.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// emit writes one observation with one write, so the executor reading the
// pipe never sees half a line.
func emit(w io.Writer, body string) error {
	_, err := fmt.Fprint(w, observationLine(body)+"\n")
	return err
}
