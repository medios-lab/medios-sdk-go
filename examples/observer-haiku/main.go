// Command medios-observer-haiku is the first task observer of DR-041
// (medios#562): the medios executor runs it in the task's sandbox beside the
// task, it tails the task's stdout file on its own timer, asks Haiku whether
// the run looks healthy, and prints each judgment as one line to its own
// stdout. The executor prefixes each line with the observer's name and the
// newest line becomes the task's progress message.
//
// It has no authority. It never sets the task's work value, never arms the
// stall clock and never ends a task. Its output is observation lines on
// stdout and token accounting on stderr, which the executor keeps in the
// sandbox as observers/<name>.stderr.
//
// medios passes it no arguments. Everything it needs is its own flags and
// the working directory the executor gives it, which is the task's sandbox.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// defaultStdoutPath is the file the observer tails, relative to the task's
// sandbox, which is the observer's working directory.
//
// Type: default of the --stdout parameter. DR-041: the task's stdout is
// already the sandbox file `stdout`, for the command executor and for the
// docker executor alike, and no path travels on the wire. An observer that
// wants to watch something else is pointed at it with the flag.
const defaultStdoutPath = "stdout"

// defaultEverySeconds is how long the observer waits between judgments, in
// seconds.
//
// Type: default of the --every parameter. Cost binds: at roughly 2000 tokens
// a judgment, 300 seconds is about 24000 tokens an hour per task. It is also
// the granularity of what the observer can see, so a five-minute shard gets
// about one verdict and a day-long run gets a few hundred. A task that needs
// either end of that sets the flag.
const defaultEverySeconds = 300

// defaultModel is the model the observer judges with.
//
// Type: default of the --model parameter. Haiku 4.5 is the cheapest current
// model and the judgment asked of it is coarse: one line, is this output
// working or not. Revisit if UNPARSED lines or verdicts that contradict the
// stall check become common.
const defaultModel = "claude-haiku-4-5-20251001"

// defaultLines is how many lines of the task's stdout are sent per judgment.
//
// Type: default of the --lines parameter, in lines. A job that prints one
// line per step puts its last few minutes in 200 lines, which is the window
// in which a loss curve going flat or a retry loop is visible. Larger
// windows cost input tokens on every call for output the model has already
// judged.
const defaultLines = 200

// defaultBudgetTokens is the token budget for one task's whole run.
//
// Type: default of the --budget-tokens parameter, in tokens, counting input
// and output together. At roughly 2000 tokens a call, 200000 tokens is about
// 100 calls, so about eight hours at the default timer. When it is spent the
// observer prints one last line saying so and exits 0; the task runs on.
const defaultBudgetTokens = 200000

// requestTimeout bounds one Messages API call.
//
// Type: default. Owner: this binary. A judgment that has not returned in 30
// seconds has missed the interval it was for; the next one sends fresher
// output, so waiting longer buys a stale verdict. Capped at the interval
// when the interval is shorter, so a slow call cannot make the loop skip.
const requestTimeout = 30 * time.Second

// exitUsage is returned for a malformed invocation.
const exitUsage = 2

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("medios-observer-haiku", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: medios-observer-haiku --credential-file <path> [flags]\n\n")
		fmt.Fprintf(stderr, "Runs in the task's sandbox, tails ./%s, prints one observation per line.\n\n", defaultStdoutPath)
		fs.PrintDefaults()
	}
	var (
		credFile   = fs.String("credential-file", "", "file holding the API key; required, read on every judgment")
		stdoutFile = fs.String("stdout", defaultStdoutPath, "file to tail, relative to the task's sandbox")
		every      = fs.Float64("every", defaultEverySeconds, "seconds between judgments")
		model      = fs.String("model", defaultModel, "model to judge with")
		lines      = fs.Int("lines", defaultLines, "lines of task output sent per judgment")
		budget     = fs.Int("budget-tokens", defaultBudgetTokens, "input plus output tokens this observer may spend on this task")
		maxErrors  = fs.Int("max-consecutive-errors", defaultMaxConsecutiveErrors, "consecutive failed calls after which the observer stops itself")
		promptFile = fs.String("prompt-file", "", "file holding the system prompt; the embedded prompt is used when empty")
		once       = fs.Bool("once", false, "make one judgment, print it, and exit")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "no positional arguments: medios passes the observer none\n")
		fs.Usage()
		return exitUsage
	}

	// Anything that stops the observer before its first judgment is one
	// line on the task record, as DR-041's lifecycle rules require, then a
	// non-zero exit. The executor does not restart it.
	fail := func(reason string) int {
		if err := emit(stdout, "did not start: "+reason); err != nil {
			fmt.Fprintf(stderr, "could not write the observation: %v\n", err)
		}
		return 1
	}

	if *credFile == "" {
		return fail("--credential-file was not given")
	}
	if *every <= 0 {
		return fail(fmt.Sprintf("--every must be a positive number of seconds, got %v", *every))
	}
	if *lines <= 0 {
		return fail(fmt.Sprintf("--lines must be positive, got %d", *lines))
	}
	if *budget <= 0 {
		return fail(fmt.Sprintf("--budget-tokens must be positive, got %d", *budget))
	}
	if *maxErrors <= 0 {
		return fail(fmt.Sprintf("--max-consecutive-errors must be positive, got %d", *maxErrors))
	}

	prompt := defaultPrompt
	if *promptFile != "" {
		b, err := os.ReadFile(*promptFile)
		if err != nil {
			return fail(fmt.Sprintf("could not read the prompt file: %v", err))
		}
		if len(b) == 0 {
			return fail(fmt.Sprintf("the prompt file %s is empty", *promptFile))
		}
		prompt = string(b)
	}

	cred := credential{path: *credFile}
	// Read once before the loop so a credential the node never placed is
	// one refusal on the task record rather than an error line per
	// interval.
	if _, err := cred.read(); err != nil {
		return fail(err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	o := &observer{
		judge: &judge{
			client:   &http.Client{},
			endpoint: messagesEndpoint,
			cred:     cred,
			model:    *model,
			prompt:   prompt,
		},
		stdoutPath:   *stdoutFile,
		lines:        *lines,
		budgetTokens: *budget,
		maxErrors:    *maxErrors,
		every:        time.Duration(*every * float64(time.Second)),
		once:         *once,
		out:          stdout,
		stderr:       stderr,
	}
	return o.loop(ctx)
}

type observer struct {
	judge        *judge
	stdoutPath   string
	lines        int
	budgetTokens int
	maxErrors    int
	every        time.Duration
	once         bool
	out          io.Writer
	stderr       io.Writer

	spent           int
	consecutiveErrs int
}

// loop judges on the timer until the sandbox goes away, the budget is spent,
// the calls fail in a row or the process is signalled. SIGTERM exits 0: the
// executor kills the observer with the task and that is not a failure.
//
// The first judgment happens at once rather than after one interval, so a
// credential the API rejects shows up on the task record in seconds instead
// of after the first interval.
func (o *observer) loop(ctx context.Context) int {
	ticker := time.NewTicker(o.every)
	defer ticker.Stop()
	return o.loopOn(ctx, ticker.C)
}

// loopOn is loop over a caller's timer channel.
func (o *observer) loopOn(ctx context.Context, ticks <-chan time.Time) int {
	for {
		if !o.sandboxPresent() {
			fmt.Fprintf(o.stderr, "the directory of %s is gone; exiting\n", o.stdoutPath)
			return 0
		}
		if done := o.judgeOnce(ctx); done || o.once {
			return 0
		}

		// A judgment that outlasted the interval leaves a tick waiting.
		// Dropping it skips that interval rather than making a second call
		// at once: the next call would read nearly the same output and bill
		// for it, and two calls in flight is not what --every asked for.
		select {
		case <-ticks:
		default:
		}

		select {
		case <-ctx.Done():
			return 0
		case <-ticks:
		}
	}
}

// sandboxPresent reports whether the directory holding the task's stdout
// still exists. The executor removes the sandbox when it reaps the task, so
// its absence is the observer's cue to stop; it must not outlive the
// sandbox.
func (o *observer) sandboxPresent() bool {
	st, err := os.Stat(filepath.Dir(o.stdoutPath))
	return err == nil && st.IsDir()
}

// judgeOnce makes one judgment and prints one observation. It reports
// whether the observer is finished, which happens when the budget is spent
// or when the calls have failed --max-consecutive-errors times in a row.
func (o *observer) judgeOnce(ctx context.Context) bool {
	tail, err := tailLines(o.stdoutPath, o.lines)
	if err != nil {
		// A read failure is an observation and not a reason to stop: the
		// task may not have written its first bytes yet.
		o.observe(fmt.Sprintf("error: could not read the task output: %v", err))
		return false
	}

	timeout := requestTimeout
	if o.every < timeout {
		timeout = o.every
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	reply, used, err := o.judge.judgeTail(callCtx, tail)
	cancel()

	o.spent += used.Input + used.Output
	fmt.Fprintf(o.stderr, "usage input=%d output=%d total_so_far=%d\n", used.Input, used.Output, o.spent)

	if err != nil {
		if ctx.Err() != nil {
			// The signal, not the API, ended the call.
			return true
		}
		o.observe(fmt.Sprintf("error: %v", err))
		o.consecutiveErrs++
		if o.consecutiveErrs >= o.maxErrors {
			o.observe(fmt.Sprintf("stopped: %d consecutive errors", o.consecutiveErrs))
			return true
		}
		return false
	}

	o.consecutiveErrs = 0
	o.observe(verdictBody(reply))

	if o.spent >= o.budgetTokens {
		o.observe(fmt.Sprintf("stopped: token budget of %d tokens is spent (%d used); no further judgments",
			o.budgetTokens, o.spent))
		return true
	}
	return false
}

// observe prints one observation, reporting a write failure on stderr
// because there is nowhere else left to report it.
func (o *observer) observe(body string) {
	if err := emit(o.out, body); err != nil {
		fmt.Fprintf(o.stderr, "could not write the observation: %v\n", err)
	}
}
