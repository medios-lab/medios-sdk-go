package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var errWriteFailed = errors.New("write failed")

// syncBuffer collects observations from the observer's goroutine while a
// test reads them.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testObserver builds an observer over a sandbox holding two lines of task
// output, with its observations captured in out.
func testObserver(t *testing.T, endpoint string, out, stderr *syncBuffer) *observer {
	t.Helper()
	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, defaultStdoutPath)
	writeFile(t, stdoutPath, "step 1 loss 2.0\nstep 2 loss 1.8\n")
	return &observer{
		judge:        testJudge(t, endpoint),
		stdoutPath:   stdoutPath,
		lines:        defaultLines,
		budgetTokens: defaultBudgetTokens,
		maxErrors:    defaultMaxConsecutiveErrors,
		every:        time.Second,
		once:         true,
		out:          out,
		stderr:       stderr,
	}
}

func observations(out *syncBuffer) []string {
	s := strings.TrimSuffix(out.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// immediateTicks returns a closed channel so loopOn advances to the next
// iteration without sleeping while keeping o.every at 1s for the per-request
// HTTP timeout (#54).
func immediateTicks() <-chan time.Time {
	ch := make(chan time.Time)
	close(ch)
	return ch
}

func TestObserverOncePrintsOneObservationAndLogsUsage(t *testing.T) {
	srv, _, _ := replyServer(t, "OK loss falling", 1900, 20)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)

	if code := o.loop(context.Background()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := observations(&out); len(got) != 1 || got[0] != "OK loss falling" {
		t.Fatalf("observations %q", got)
	}
	if want := "usage input=1900 output=20 total_so_far=1920\n"; stderr.String() != want {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
}

func TestObserverPrintsUnparsedForProse(t *testing.T) {
	srv, _, _ := replyServer(t, "The job looks fine to me.", 10, 10)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)

	o.loop(context.Background())
	if got := observations(&out); len(got) != 1 || got[0] != "UNPARSED The job looks fine to me." {
		t.Fatalf("observations %q", got)
	}
}

// A request failure is an observation and the timer is kept, until the run
// of failures reaches the bound and the observer stops itself.
func TestObserverRecordsRequestErrorsThenStops(t *testing.T) {
	var out, stderr syncBuffer
	o := testObserver(t, "http://127.0.0.1:1/v1/messages", &out, &stderr)
	o.once = false
	o.maxErrors = 3

	if code := o.loopOn(context.Background(), immediateTicks()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := observations(&out)
	if len(got) != 4 {
		t.Fatalf("want three error lines and one stopped line, got %q", got)
	}
	for _, l := range got[:3] {
		if !strings.HasPrefix(l, "error: ") {
			t.Fatalf("line %q", l)
		}
	}
	if got[3] != "stopped: 3 consecutive errors" {
		t.Fatalf("last line %q", got[3])
	}
}

// One failure between two good calls does not accumulate: the run counts
// consecutive failures, not failures.
func TestObserverResetsTheErrorRunOnASuccess(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limit exceeded"}}`)
			return
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"OK fine"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)
	o.once = false
	o.maxErrors = 2
	o.budgetTokens = 6 // three answered calls of two tokens; the 429 bills none

	if code := o.loopOn(context.Background(), immediateTicks()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := observations(&out)
	if len(got) != 5 {
		t.Fatalf("want three verdicts, one error and one budget line, got %q", got)
	}
	if !strings.Contains(got[1], "error: messages api returned 429") {
		t.Fatalf("second line %q", got[1])
	}
	if !strings.Contains(got[4], "stopped: token budget") {
		t.Fatalf("last line %q, want the budget to be what stopped it", got[4])
	}
}

func TestObserverRecordsAnUnreadableTaskOutput(t *testing.T) {
	srv, _, _ := replyServer(t, "OK fine", 10, 10)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)
	if err := os.Remove(o.stdoutPath); err != nil {
		t.Fatal(err)
	}

	o.loop(context.Background())
	got := observations(&out)
	if len(got) != 1 || !strings.Contains(got[0], "error: could not read the task output") {
		t.Fatalf("observations %q", got)
	}
}

// The budget binds: the last observation says it is spent and the observer
// exits 0 without a further judgment.
func TestObserverStopsWhenTheBudgetIsSpent(t *testing.T) {
	srv, _, _ := replyServer(t, "OK loss falling", 400, 20)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)
	o.once = false
	o.budgetTokens = 1000 // spent after three calls of 420 tokens

	if code := o.loopOn(context.Background(), immediateTicks()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := observations(&out)
	if len(got) != 4 {
		t.Fatalf("want three verdicts and one budget line, got %q", got)
	}
	if !strings.Contains(got[3], "stopped: token budget of 1000 tokens is spent (1260 used)") {
		t.Fatalf("last line %q", got[3])
	}
}

// The observer must not outlive the sandbox: when the directory holding the
// task's stdout is gone it exits 0 without judging.
func TestObserverExitsWhenTheSandboxIsGone(t *testing.T) {
	srv, _, _ := replyServer(t, "OK fine", 10, 10)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)
	o.once = false
	if err := os.RemoveAll(filepath.Dir(o.stdoutPath)); err != nil {
		t.Fatal(err)
	}

	if code := o.loop(context.Background()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := observations(&out); got != nil {
		t.Fatalf("judged into a gone sandbox: %q", got)
	}
}

// SIGTERM, which the executor sends when it kills the task, exits 0.
func TestObserverExitsZeroWhenCancelled(t *testing.T) {
	srv, _, _ := replyServer(t, "OK fine", 1, 1)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)
	o.once = false
	o.every = time.Hour // the cancel, not the timer, ends the loop

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- o.loopOn(ctx, make(chan time.Time)) }()
	for observations(&out) == nil {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the observer did not exit on cancellation")
	}
}

// A judgment that outlasts the interval skips the tick it overran rather
// than making a second call at once: two pending intervals yield two
// judgments, not three.
func TestObserverSkipsATickItOverran(t *testing.T) {
	srv, _, _ := replyServer(t, "OK fine", 1, 1)
	var out, stderr syncBuffer
	o := testObserver(t, srv.URL, &out, &stderr)
	o.once = false

	ticks := make(chan time.Time, 2)
	ticks <- time.Now()
	ticks <- time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- o.loopOn(ctx, ticks) }()

	deadline := time.After(2 * time.Second)
	for len(observations(&out)) < 2 {
		select {
		case <-deadline:
			t.Fatalf("observations %q", observations(&out))
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := observations(&out); len(got) != 2 {
		t.Fatalf("got %d judgments, want 2: %q", len(got), got)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}
