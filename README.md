# medios-sdk-go

The shared Go binding for the Medios HTTP v1 scheduler API. Every Go framework
on Medios needs the same scheduler client: the wire types, the RecordIO stream
codec, and the SUBSCRIBE to event to call driver, plus the surrounding
machinery (state store, task output retrieval, task-lifecycle types). This is
that, in one place, instead of each framework hand-rolling its own copy.

You bring the scheduling policy, what to launch and when. The SDK handles the
wire protocol, the task-lifecycle types, sandbox output retrieval, and a small
persistence layer.

**Status:** validated end-to-end against a live single-node Medios 1.0.0
cluster. The [`summer`](examples/summer) example computed `1+…+100` across two
real tasks and read each partial sum back from the node sandbox:

```
launching         host=10.0.0.12 tasks=2
partition finished index=0 range=1..50   partial=1275
partition finished index=1 range=51..100 partial=3775
sum(1..100) = 5050  [verified from 2 task results]
```

## Install

```sh
go get github.com/medios-lab/medios-sdk-go
```

Requires Go 1.25. No CGO (`modernc.org/sqlite` is pure Go). Pin a tagged version
(not `@latest`) so builds are reproducible; bump with `go get …@vX.Y.Z`.

## Quickstart

A framework is a `Subscribe` loop that reacts to two events: `OFFERS` (launch
work or decline) and `UPDATE` (a task changed state). Wire types live in
`medios`; the driver is `scheduler`. This snippet is abridged to the shape of
the loop — for a complete, correct framework (offer suppress/revive, decline
filters, terminal handling, teardown) read and run
[`examples/reference`](examples/reference), which the **Writing a framework**
section walks through.

```go
client, _ := scheduler.NewClient(
    "http://controller:5150", "my-framework",
    scheduler.WithUser("alice"), // MUST be a real user on the nodes — see Notes
    scheduler.WithPrincipal("framework:my-framework"),
)

events, errs, _ := client.Subscribe(ctx)

for {
    select {
    case <-ctx.Done():
        return ctx.Err()
    case err := <-errs:
        return err
    case ev := <-events:
        switch ev.Type {
        case medios.EventOffers:
            for _, o := range ev.Offers.Offers {
                if wantToLaunch {
                    // Carve what you need OUT of the offered resource. Do not
                    // build a new one of the same name — see Resource identity.
                    offered, _ := medios.FindResource(o, "cpus")
                    cpus, _ := offered.Carve(0.1)
                    // Every task names the submitter it runs for, as
                    // kind:name; NewTaskInfo refuses to build one without it.
                    task, err := medios.NewTaskInfo("operator:alice", "hello",
                        medios.ID{Value: "hello-1"}, o.NodeID,
                        []medios.Resource{cpus},
                        medios.CommandInfo{Shell: true, Value: "echo hi"})
                    if err != nil { /* refuse the request that named no principal */ }
                    client.Accept(ctx, []medios.ID{o.ID}, []medios.TaskInfo{task})
                } else {
                    // ALWAYS decline what you don't launch, with a refuse filter.
                    // An offer you neither accept nor decline is hoarded.
                    client.Decline(ctx, []medios.ID{o.ID}, 120)
                }
            }
        case medios.EventUpdate:
            st := ev.Update.Status
            if st.UUID != "" {
                client.Acknowledge(ctx, st.TaskID.Value, st.NodeID.Value, st.UUID)
            }
            if st.State.IsTerminal() { // st.State is a typed medios.TaskState
                // ... handle TASK_FINISHED / TASK_FAILED (st.State.IsFailure()) ...
            }
        }
    }
}
```

A real framework must also **SUPPRESS** offers when it has nothing to launch and
**REVIVE** when work appears, or it keeps taking the cluster's offers while idle.
That is the difference between the snippet above and `examples/reference`.

## Resource identity

A resource in an offer is not just a name and a number. It also carries the
role it belongs to, its reservations, its allocation info, the resource
provider that supplied it, and any metadata labels. The controller compares
those fields by equality when it validates an ACCEPT.

So a task must return the resources it was offered with only the quantity
changed. Constructing a fresh `Resource` with the same name silently drops
everything else, and the offer that comes back no longer matches what was sent.
The controller still accepts such a task, so the loss shows up only in the
node's own accounting.

```go
offered, ok := medios.FindResource(offer, "cpus") // the offered resource, whole
cpus, ok := offered.Carve(0.1)                    // a copy, quantity 0.1, identity intact
```

`Carve` is SCALAR only. A share of a RANGES resource such as `ports` is chosen
by picking ranges rather than by splitting a number, so carry the offered
resource and replace its ranges:

```go
offered, _ := medios.FindResource(offer, "ports")
offered.Ranges = chosen // whatever you picked out of offered.Ranges
```

Fields this library does not model are preserved verbatim through decode and
encode, so a resource round-trips unchanged whether or not the Go type knows
about every field the controller sent. You do not need an SDK release to stop
losing a field the controller added.

## Framework capabilities

`FrameworkInfo.Capabilities` declares what the framework supports, and the
controller gates parts of the offer stream on it. The one that surprises people
is `CapabilityGPUResources`: without it the allocator excludes *every* resource
on a node that has GPUs, not only the GPUs. A framework that never asks for a
GPU still needs to declare it to receive ordinary CPU and memory offers from a
GPU-bearing node.

```go
scheduler.WithCapabilities(medios.CapabilityGPUResources)
```

## Packages

| Package | What it gives you |
|---|---|
| [`medios`](medios) | The wire format: JSON-tagged Go types for every Call and Event (`Offer`, `TaskInfo`, `TaskStatus`, `Call`, `Event`, …), the `New*Call` constructors, the RecordIO codec for the framed SUBSCRIBE response, and `TaskState` — the typed wire task state (`TASK_RUNNING`, …) with `IsActive`/`IsTerminal`/`IsFailure`. |
| [`scheduler`](scheduler) | The driver: `NewClient`, `Subscribe` (event stream), `Accept`, `Decline` (with a refuse filter), `Suppress`/`Revive`, `Request`, `Reconcile`, `Kill`, `Acknowledge`, `Teardown`; `framework_id` capture and `Medios-Stream-Id` threading. |
| [`sandbox`](sandbox) | Read a task's `stdout`/`stderr` from a node's `/files/read` endpoint. This is how a framework collects a task's output. |
| [`outputs`](outputs) | Attempt output namespace layout (`AttemptDir`), manifest v1 writer (`Publish`), and artifact verifier (`List`, `Verify`). |
| [`store`](store) | A narrow persistence contract (`Singleton` + `Blob` buckets), with `sqlite` (WAL, embedded migrations, no CGO) and in-memory implementations, plus a shared `storetest` contract suite. |
| [`timestamp`](timestamp) | Millisecond-precision UTC time with ISO-8601 JSON. |

## Example frameworks

The [`examples/`](examples) directory holds complete, runnable frameworks. They
are not tests — each is a real `main` that subscribes to a live controller and
launches actual tasks.

**[`examples/reference`](examples/reference) — start here.** The canonical
well-behaved framework: it launches N tasks and demonstrates *every* behavior a
real framework needs — `WithUser`, launching on offers that fit, declining the
rest with a refuse filter, declining on accept-error (no leak), SUPPRESS when
idle / REVIVE when work appears, acknowledging updates, classifying terminal
state via `medios.TaskState`, and teardown. If you copy one thing, copy this.

```sh
go run ./examples/reference -controller http://127.0.0.1:5150 -user <node-user> -n 3
```

Validated live: launches the tasks, SUPPRESSes once they're all out, reports
each `TASK_FINISHED`, and tears down.

**[`examples/summer`](examples/summer)** — additionally shows reading task
*output* back from the node sandbox (computes `1+…+100` across real tasks).

```sh
go run ./examples/summer -controller http://127.0.0.1:5150 -user <node-user> \
  -n 100 -m 2
```

## Notes

1. The SDK threads the `Medios-Stream-Id` header on every non-`SUBSCRIBE` call;
   the controller requires it. You don't set it yourself.
2. **`framework.user` must exist on the node.** It defaults to the local OS
   user, which is wrong for a remote cluster: the task returns
   `TASK_LOST: User '<x>' does not exist on this node`. Pass
   `scheduler.WithUser("<a-real-node-user>")`.
3. **Reading task output needs the node to be network-reachable.** `sandbox`
   reads from the node's own address and port, as the controller's `/nodes`
   reports them, and takes the sandbox root from the node's `/state`. If the
   node binds loopback, cross-host reads fail even though launches succeed.
   The node must advertise a routable address.
4. The scheduler stream works over HTTP/1.1; HTTP/2 is not required.
5. **Don't hoard offers.** Decline every offer you don't `Accept` (with a refuse
   filter, e.g. `120`), decline on accept-error too, and `Suppress` while you
   have nothing to launch (`Revive` when work appears). An offer you neither
   accept nor decline stays outstanding to your framework and starves the
   others on the cluster. `examples/reference` implements the full lifecycle;
   it mirrors Marathon's offer-revive behavior.

## Design

- [`docs/boundary.md`](docs/boundary.md), scope and architecture.
- [`docs/writing-a-framework.md`](docs/writing-a-framework.md), a walkthrough
  using `summer` as the worked example.

## License

Apache 2.0. See [LICENSE](LICENSE).
