# Architecture and scope

`medios-sdk-go` is the layer between a scheduler's policy and the Medios
controller. It owns the mechanics every framework repeats; the framework owns
what is unique to it. The split is the whole point of the SDK, so keep it sharp.

## What the SDK owns

| Package | Responsibility |
|---|---|
| `medios` | The wire types for every Call and Event, the `New*Call` constructors, `NewTaskInfo` with the one check of a task's principal, multi-copy scalar resource carving and offer-matching helpers (`Offer.CarveScalar`, `Offer.CarveScalars`, `Offer.MatchesAttributes`, `Offer.SupportsContainer`), store-wait supply tracking (`ClusterView`), the task-state and status predicates (`IsActive`, `IsTerminal`, `IsFailure`, `TaskStatus.WorkExitCode`, `TaskStatus.StoreUnitRefused`), the progress types and their validation, and the RecordIO stream parser. |
| `scheduler` | The HTTP v1 client/driver: subscribe, offers, accept/launch, decline, request, reconcile, kill, acknowledge, teardown. Threads the `Medios-Stream-Id` header and captures `framework_id`. |
| `sandbox` | Locates a node from its id (address from the controller's `/nodes`, `work_dir` from the node's `/state`), reads a task's `stdout`/`stderr` from that node's `/files/read` endpoint, and captures bounded UTF-8 tails with retry backoff (`Resolver.CaptureTail`, `TrimUTF8Tail`). |
| `outputs` | Attempt output namespace layout, manifest v1 writer (`Publish`), and artifact verifier (`List`, `Verify`). |
| `store` | A narrow persistence contract (singletons + blob buckets) with SQLite and in-memory implementations and a shared contract test. |
| `timestamp` | Millisecond-precision UTC time with ISO-8601 JSON. |

## What moves into the SDK

The SDK owns any mechanic that two or more frameworks have specified
identically. A defect found in two frameworks is fixed once here and both adopt
the fix. Policy stays with the framework: the SDK takes the mechanic and leaves
the decision as an argument or a callback. The threshold is evidence, two
independent specifications of the same behaviour, so nothing moves in on the
expectation that a second framework will want it (medios-sdk-go#20).

## What the framework owns (deliberately not in the SDK)

- **Domain objects.** Apps, jobs, pipelines, whatever the framework schedules.
- **Scheduling policy.** Which offers to accept, what to launch, and when. The
  SDK delivers offers and sends your decisions; it never decides for you.
- **The API surface.** Any REST/RPC the framework exposes to its users.
- **Identity & the task record.** The SDK has no task-record type. How you track
  a launched task — its owning entity (an instance id, a job-run key, …), its
  lifecycle, and how task ids are formed — is the framework's. (A generic task
  record once lived here and was removed for crossing this very line.)

## Principles

- **Dependency-light.** The only non-stdlib runtime dependency is the pure-Go
  SQLite driver. New runtime dependencies are added reluctantly.
- **No native driver, no JNI.** Everything is HTTP and JSON over the wire; the
  result is a single static binary.
- **Narrow interfaces.** `store` exposes blob/singleton KV, not SQL. The wire
  types mirror the controller's JSON exactly, with no convenience layer that
  could drift from it.
- **Stable contracts.** The `store.Store` interface and the `scheduler` wire
  types are the stability anchors: implementations may change beneath them, the
  shapes should not.

## Package map

```
        framework A                         framework B
   ┌───────────────────┐              ┌───────────────────┐
   │ domain            │              │ domain            │
   │ scheduling policy │              │ scheduling policy │
   │ API surface       │              │ API surface       │
   └─────────┬─────────┘              └─────────┬─────────┘
             └──────────────┬───────────────────┘
              ┌─────────────┴──────────────┐
              │ scheduler · store · sandbox │
              │ medios · timestamp          │
              └─────────────────────────────┘
                   medios-sdk-go
```

## Versioning

Pre-1.0. The `medios` wire types track the Medios HTTP v1 contract; when the
contract revs, the SDK revs and consumers bump together. Tag `v0.x` per
milestone; consumers pin an exact version rather than tracking `main`.
