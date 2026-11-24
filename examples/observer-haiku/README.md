# observer-haiku

A demonstration task observer (DR-041, medios#562) that judges with Haiku. It is example code, not part of the SDK: a framework builds its own observer command, and this shows one. The
medios executor runs it in the task's sandbox beside the task. On its own
timer the observer tails the task's stdout file, asks the model whether the
run looks healthy, and prints the answer as one line to its own stdout. The
executor prefixes each line with the observer's name and the newest line
becomes the task's progress message.

The observer has no authority. It never sets the task's work value, never
arms the stall clock and never ends a task. Its output is observation lines
on stdout and token accounting on stderr, which the executor keeps in the
sandbox as `observers/<name>.stderr`.

## What medios passes it

Nothing. No positional arguments, no observation file, no cadence on the
wire. A positional argument exits 2.

What it relies on is the working directory: the executor runs an observer
with the task's sandbox as its current directory, and the task's output is
already the sandbox file `stdout`, for the command executor and for the
docker executor alike. The observer reads `./stdout` unless `--stdout` names
something else.

Everything else is its own flags, which the task's `TaskObserver` command
carries. A new kind of observer is a new program, not a new proto field.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--credential-file` | none, required | File holding the API key. Read on every judgment, so a key rotated in place takes effect at the next interval with no restart. Not given, unreadable or empty: one `did not start:` line and exit 1. |
| `--stdout` | `stdout` | File to tail, relative to the sandbox. |
| `--every` | `300` | Seconds between judgments. Parameter: cost binds, and at about 2000 tokens a judgment this is roughly 24000 tokens an hour per task. |
| `--model` | `claude-haiku-4-5-20251001` | Model to judge with. Parameter: the cheapest current model, and the judgment asked of it is coarse. |
| `--lines` | `200` | Lines of task output sent per judgment. Parameter: a job printing one line per step puts its last few minutes in 200 lines. |
| `--budget-tokens` | `200000` | Input plus output tokens for this task's whole run. Parameter: about 100 calls, so about eight hours at the default interval. When it is spent the observer prints one last line and exits 0. |
| `--max-consecutive-errors` | `5` | Failed calls in a row after which the observer prints `stopped: <n> consecutive errors` and exits 0. Parameter: long enough to ride out a rate limit, short enough that a dead credential is one short burst on the task record. |
| `--prompt-file` | embedded prompt | File holding the system prompt. |
| `--once` | off | Make one judgment, print it, and exit. For hand testing and for a cue scenario. |

There is no `ANTHROPIC_API_KEY` fallback and no credential in the task's
environment. What the node puts in the executor's environment is an
operations choice; the observer's command points `--credential-file` at it.

## Observation lines

One line per judgment on stdout, each truncated to 1000 bytes, with no name
prefix: the executor adds the name.

```
OK loss falling, step 1240 of 5000
ISSUE the same CUDA out-of-memory traceback repeats
UNPARSED <first 200 characters of a reply that did not begin OK or ISSUE>
error: messages api returned 429: rate limit exceeded
stopped: 5 consecutive errors
stopped: token budget of 200000 tokens is spent (200412 used); no further judgments
did not start: --credential-file was not given
```

The model is told to answer in one line beginning OK or ISSUE. A reply that
does not is printed as `UNPARSED` rather than coerced into a verdict.

An error is an observation and not a reason to stop; the timer is kept and a
successful call resets the run. A judgment that outlasts the interval skips
the tick it overran rather than making a second call at once.

The observer exits 0 on SIGTERM, which is how the executor kills it with the
task, and on its own when the directory holding the task's stdout
disappears. It cannot outlive the sandbox.

Token accounting goes to stderr, one line per call:

```
usage input=1904 output=18 total_so_far=1922
```

## By hand

One judgment over a sandbox of your own, against the live API:

```sh
mkdir -p /tmp/sandbox
printf 'step 1 loss 2.0\nstep 2 loss 1.8\nstep 3 loss 1.7\n' > /tmp/sandbox/stdout
echo "$ANTHROPIC_API_KEY" > /tmp/anthropic.key
cd /tmp/sandbox && go run github.com/medios-lab/medios-sdk-go/examples/observer-haiku \
  --credential-file /tmp/anthropic.key --once
```
