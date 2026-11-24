# Writing a framework on the SDK

A Medios framework is a long-lived process that subscribes to a controller,
receives **resource offers**, launches **tasks** onto nodes, and reacts to their
**status updates**. This SDK gives you the wire client, the task primitives, the
sandbox reader, and a state store; you supply the policy, *what* to launch and
*when*. [`examples/summer`](../examples/summer) is the worked example referenced
throughout.

## 1. The shape of every framework

It is one loop over the subscription event stream:

```
Subscribe ──► SUBSCRIBED ──► OFFERS ──► (decide) ──► Accept/LAUNCH or Decline
                  │             ▲                          │
                  │             └──────────────────────────┘
                  └──► UPDATE (TASK_RUNNING / TASK_FINISHED / TASK_LOST / …)
                  └──► HEARTBEAT (ignore)         └──► Acknowledge, react
```

You never poll; offers and updates are pushed to you over the SUBSCRIBE
connection (RecordIO-framed JSON). When you are done, `Teardown`.

## 2. Construct the client

```go
client, err := scheduler.NewClient(controllerURL, "my-framework",
    scheduler.WithUser("alice"),
    scheduler.WithPrincipal("framework:my-framework"))
```

- `controllerURL` is the controller's HTTP base, e.g. `http://host:5150`.
- **`WithCapabilities` decides which offers you see at all.** Without
  `medios.CapabilityGPUResources` the allocator excludes every resource on a
  GPU-bearing node, not only its GPUs, so a CPU-only framework goes offerless
  against a GPU cluster and nothing explains why.
- **`WithUser` is not optional in practice.** The default is your local OS user,
  which almost never exists on the node, the task will return
  `TASK_LOST: User '<x>' does not exist on this node`. Pass a user that exists on
  the node (find one via the node's own `/state` `flags`, or ask the operator).

## 3. The event loop

```go
events, errs, err := client.Subscribe(ctx)
// select over ctx.Done(), errs, events; switch on ev.Type
```

Handle four event types; ignore the rest:

- `EventSubscribed`, `client.FrameworkID()` is now populated. Log it.
- `EventOffers`, see §4.
- `EventUpdate`, see §5.
- `EventHeartbeat`, no body; ignore. (Its absence over ~5× the interval means
  the stream is dead; the `errs` channel will surface a read error.)

The `errs` channel sends at most once then closes; set it to `nil` after a
closed read so `select` stops waking on it (see `summer`'s loop).

## 4. Launching from an offer

An offer advertises a node's free resources. Take what you need out of it, build
a `TaskInfo`, and `Accept`; `Decline` what you won't use so it returns to the
pool.

The rule that matters: **carve the task's resources out of the offered ones
rather than building new ones with the same names.** An offered resource carries
role, reservations, allocation info, the supplying resource provider, and
metadata labels, and the controller compares those by equality. A fresh
`Resource` with a matching name and quantity is a different resource, and the
identity you dropped is not recoverable from the ACCEPT.

```go
offeredCPU, ok := medios.FindResource(o, "cpus")
if !ok { /* this offer has no cpus */ }
cpus, ok := offeredCPU.Carve(0.1) // 0.1 of it, everything else preserved
if !ok { /* the offer has less than you asked for */ }

offeredMem, _ := medios.FindResource(o, "mem")
mem, _ := offeredMem.Carve(32)

task, err := medios.NewTaskInfo(submitter, "work-0",
    medios.ID{Value: "work-0"}, o.NodeID,
    []medios.Resource{cpus, mem},
    medios.CommandInfo{Shell: true, Value: "echo hello"})
if err != nil { /* no principal, or a malformed one: refuse the caller's request */ }
client.Accept(ctx, []medios.ID{o.ID}, []medios.TaskInfo{task})
```

Every task names the submitter it runs on behalf of, as `TaskInfo.Principal`.
The value is `kind:name` with kind `operator`, `team` or `run`
(`operator:alice`, `team:research`, `run:research-164-s5-r3`); the
controller refuses a launch without one. It is a declared value your caller
gives you, forwarded as given. It is not the framework's own principal
(`WithPrincipal`) and not the OS user (`WithUser`, `CommandInfo.User`), and
`NewTaskInfo` does not fall back to either: a framework whose caller named no
submitter returns that error to the caller. Status updates carry the
controller's record of the pair back as `TaskStatus.Principal` and
`TaskStatus.FrameworkPrincipal`.

`Carve` handles SCALAR resources. `ports` is RANGES, where a share is chosen by
picking ranges instead of splitting a number, so carry the offered resource and
replace only its ranges:

```go
offeredPorts, _ := medios.FindResource(o, "ports")
offeredPorts.Ranges = picked
```

Two things to know before you rely on `FindResource`. It returns the first
resource with that name, and one name can legitimately appear several times in
an offer, because a reservation splits a quantity across role-bearing copies.
If your framework sums a name across the offer to decide it fits, then carves
the whole amount from the first copy, it will refuse offers it could actually
satisfy. Use `o.CarveScalar("cpus", 3.0)` (or `o.CarveScalars(...)` for multiple
scalars at once) to sum across every matching copy in controller order, emit one
carved `Resource` per contributing copy with its reservation/allocation JSON
identity intact, and receive a copy of the remaining `Offer` for fitting
subsequent tasks in the same cycle. For node attribute constraints and
containerizer routing, `o.MatchesAttributes(reqs)` checks `TEXT` attribute
equality and `o.SupportsContainer(hasImage, hasFinish)` checks `containerizer`
(`medios` vs. `docker_shell`) and `images:docker` (including DR-042's requirement
that tasks with both an image and a finish hook require `containerizer: medios`,
and that bare tasks without an image require `containerizer: medios`). Fields
this library does not model are preserved verbatim through decode and encode, so
you do not lose a field the controller added just because the Go type predates
it.

Record the offer's `NodeID` per task; you need it to read the sandbox later
(§6).

A job says what kind of work it is and the SDK expands that into the task's
progress settings: `medios.ProgressPolicyForProfile(medios.ProfileTraining)`
returns a policy that reads progress from the task's stdout and kills it after
two minutes with no advance, and `medios.ProfileService` returns none, because a
process with no end has no completion to be a fraction of. Set the result on
`TaskInfo.ProgressPolicy`; a framework that builds a policy by hand instead
should pass it through `medios.ValidateProgressPolicy` first, since
`NewTaskInfo` does not see it. The profile's sample interval and stall window
are provisional defaults, not measured ones.

### Data: stores, not host paths

A task that needs data names a store the node declares and the access it
wants; it never names a path on a node. A node advertises each store as a
shared resource `store/<name>` with its declared `readers` and `writers`
as metadata (DR-036). Add each store from the offer:

```go
task, err = task.WithStore(o, "tabicl-tables", "tables", medios.StoreRO)
task, err = task.WithStore(o, "results", "out", medios.StoreRWExclusive)
```

The container path says where the task finds the store. A relative path is
one name inside the task's sandbox, so the store above is at
`$MEDIOS_SANDBOX/tables`, or `./tables` from a command task's working
directory. Every node delivers that form: Linux binds the store there and a
Mac links it there. An absolute path such as `/data` is delivered on Linux
only, and a Mac node refuses the launch. A nested relative path
(`data/tables`), `.`, `..`, an empty path and two volumes at one path are
refused, as the controller refuses them. A Mac node delivers RW only, so a
task that only reads a Mac store asks for `StoreRW`.

`WithStore` appends the volume and the offered `store/<name>` resource,
identity intact, which is what puts the task on the controller's holder
ledger. A task with no `Container` gets a MEDIOS container with no image,
so a bare command can mount a store too; a `Container` already set is
kept. Whether the node delivers the mount is the node's answer at launch,
and a node without a filesystem isolator refuses the launch rather than
run the command without its data. `StoreRO` takes one of the store's reader units, `StoreRW` one
writer unit, and `StoreRWExclusive` every writer unit so nothing else
writes beside the task. The volume reaches the wire as `source: {type:
"STORE", store: {name, exclusive}}`. A volume with a `HOST_PATH` source, or
none, or without its store resource on the task, is refused by `WithStore`
and again by `Accept` before anything is sent, with the sentence the
controller would use.

Placement follows the store: `medios.FitsStores(offer, task)` says whether
an offer carries every store the task asks for with a declared unit for
each mode, and names the first one that does not. When a task with stores stays
pending across match cycles, `medios.NewClusterView()` tracks the latest offer
per `(node_id, role)` via `view.ObserveOffer(o)` and classifies why the task is
waiting via `view.ClassifyStoreWait(reqs, nodePred)` into `waiting_store`
(undeclared on every known node), `waiting_store_mode` (insufficient declared
reader/writer units), or `waiting_store_placement` (no single node satisfies all
stores plus `nodePred`). Whether a unit is free
is the controller's tally, decided at launch. Status updates carry the
controller's record of what the task was given as `TaskStatus.Stores`.

## 5. Tracking task state

`st.State` is a typed `medios.TaskState` (the wire value, e.g.
`medios.TaskRunning`), with classification helpers on it directly:

- If `st.UUID != ""`, you **must** `client.Acknowledge(...)` — unacknowledged
  updates are retransmitted by the node.
- `st.State.IsFailure()` → the task failed (`TASK_FAILED`, `TASK_LOST`, …);
  decide whether to relaunch or abort. Don't treat a failure as success.
- `st.State.IsTerminal()` → the task is done; if it's not a failure it finished.
  Collect its result (§6).
- `st.WorkExitCode()` → extracts the integer exit status of the work command
  from a terminal `TaskStatus` (preferring typed `Work.ExitStatus`, then `0` on
  `TASK_FINISHED`, or parsing `"Command exited with status N"` on `TASK_FAILED`),
  returning `(0, false)` for signal kills or launch failures.
- `st.StoreUnitRefused()` → on `TASK_ERROR`, detects whether the controller's
  store validator refused the launch due to transient reader/writer unit
  exhaustion (returning `(storeName, true)`) rather than a permanent
  configuration error.
- Capture `st.ContainerStatus.ContainerID` on the first `TASK_RUNNING`; you need
  it for the sandbox path.
- To mirror what a task reports about its own progress, hold a `*Progress` per
  task and fold every update into it: `rec.Progress = rec.Progress.Fold(&st)`.
  Most updates carry no progress, so `Fold` keeps the value you already hold on
  those and replaces it only on a report with a higher `Sequence`; the
  controller carries progress forward on its own record the same way, so
  folding is what keeps your view and `/state` saying the same thing.

The SDK does not impose a task-record type — hold per-task state in your own
struct, keyed by whatever your owning entity is: `summer` uses `"partition-N"`;
a services framework an instance id; a cron framework a job-run key. See
[`examples/reference`](../examples/reference) for a minimal in-memory version.

### Observers

A task that prints nothing about itself still gets a status message if it
carries an observer: a second command the executor runs in the sandbox beside
the task, whose newest stdout line becomes the task's progress message
(DR-041). Three rules bound it:

- An observer's output reaches the task record only through the progress
  message. It never sets the work value, so nothing it says can create a
  stall or hide one, and it has no authority to kill the task.
- An observer's name prefixes its lines in the record. It matches
  `[a-z0-9-]{1,32}`, is unique within the task, and a task carries at most
  eight. `WithObserver` refuses all three before you submit.
- The command runs in the task's sandbox, as the task's uid, with the
  executor's environment, and inside the task's container or cgroup. Its CPU
  and memory count against the task's own request, so add them when you add
  an observer.

What an observer watches is its own business: medios carries no path, no
cadence and no credential for it. The observer's working directory is the
sandbox, so one that reads the task's output opens `./stdout`.

An observer is any command; you build its `CommandInfo` yourself. For
example, a shell observer that tails the task's output and asks a model is:

```go
cmd := medios.CommandInfo{Shell: true,
    Value: "my-observer --credential-file \"$CREDENTIAL_FILE\" --every 300"}
if err := task.WithObserver("health", cmd); err != nil {
    return err // a bad or duplicate name, or a ninth observer, before submit
}
```

A demonstration observer that calls Haiku lives under `examples/observer-haiku`.

The credential path is the node's, and putting a credential there is an
operations choice: the node names it through `--executor_environment_variables`
and your command refers to that variable. Every look costs tokens, so no profile adds an observer for you —
`ProgressPolicyForProfile` expands into progress settings and nothing else.

## 6. Getting a task's output

A status update reports a task's lifecycle state (`TASK_FINISHED`), not what it
printed. Read the task's output from its sandbox `stdout`:

```go
r := &sandbox.Resolver{ControllerURL: controllerURL} // one per process
node, err := r.Locate(ctx, nodeID)                    // cached per node id
p := node.PathFor(client.FrameworkID(), taskID, containerID, "stdout")
res, err := r.Read(ctx, node, p, 0, -1) // offset 0, all
value := strings.TrimSpace(res.Data)
```

`Locate` asks the controller's `/nodes` for the node's address and the node's
own `/state` for its `--work_dir`, so neither is configured. One host can run
several nodes, each on its own port with its own work dir; a single port and
work dir per process reads the wrong node's tree for every node but one.

`Locate` fails with `ErrControllerUnreachable` when the controller does not
answer, `ErrNodeUnknown` when the controller does not list the id, and
`ErrNodeUnreachable` when the node's `/state` does not answer.

Two things must line up or you get `ErrNotFound` / `ErrNodeUnreachable`:

- The node's address, as the controller reports it, is **network-reachable from
  your process**. A node bound to `127.0.0.1` is unreachable from any other
  machine: launches still work, but reads fail. The node must advertise a
  routable address.
- `containerID` came from a `TASK_RUNNING` update (executor_id == task_id for the
  default command executor, which is what `PathFor` assumes).

When you only want the bounded trailing `maxBytes` of `stdout` or `stderr` after
a task terminates (with automatic UTF-8 rune boundary alignment, multi-chunk
reads past the node's 64 KiB per-request cap, retry backoff on transient
`controller_unreachable`/`node_unreachable` errors, and typed `TailError.Reason`
classification), call `r.CaptureTail(ctx, nodeID, sandboxPath, maxBytes, opts)`.

## 7. Persisting state (optional in v1, required for restart-safety)

[`store.Store`](../store) is a narrow blob/singleton KV. Marshal your domain
objects to JSON and put them in named buckets; keep your `framework_id` as a
singleton so you can re-subscribe with it after a restart. `store/sqlite` is the
production impl; `store/memory` is for tests. Don't leak SQL above the boundary.

## 8. Finishing

When your work is done, `client.Teardown(ctx)` removes the framework from the
controller and ends the stream. (If you intend to fail over and resume, set a
non-zero failover timeout and *don't* tear down, out of scope for v1.)

## Run it

```sh
go run ./examples/summer \
  -controller http://CONTROLLER:5150 -user NODE_USER \
  -work-dir NODE_WORK_DIR -node-port NODE_HTTP_PORT -n 100 -m 4
```

Read the [README notes](../README.md#notes) before your first real run. Each
one cost a debugging session against a real cluster.
