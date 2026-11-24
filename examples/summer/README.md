# summer

A demonstration framework on `medios-sdk-go`. It computes `1+…+N` by
splitting the range into `M` contiguous partitions and launching exactly `M`
tasks, one per partition. Each task sums its slice on the node and prints the
result; the framework reads each result back from the node sandbox, aggregates,
and verifies the total against `N(N+1)/2`. `M` is both the partition count and
the task count, the framework never launches more.

The result comes from the tasks' real output, read over `/files/read`, not from
any framework-side arithmetic. Runs against a live Medios cluster only, no mock.

## Run

```sh
go run ./examples/summer \
  -controller http://10.0.0.12:5150 \
  -user alice \
  -n 100 -m 2
```

Expected output:

```
launching         host=10.0.0.12 tasks=2
partition finished index=0 range=1..50   partial=1275
partition finished index=1 range=51..100 partial=3775
sum(1..100) = 5050  [verified from 2 task results]
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-controller` | (required) | Medios controller URL. |
| `-user` | local OS user | Framework user; must exist on the node. |
| `-n` | 100 | Compute the sum `1..N`. |
| `-m` | 4 | Number of jobs (partitions / tasks). Must satisfy `1 <= M <= N`. |
| `-cpus` | 0.1 | cpus requested per task. |
| `-mem` | 32 | mem (MB) requested per task. |

## What it exercises

| SDK package | Used for |
|---|---|
| `scheduler` | subscribe, offer matching, accept/launch, acknowledge, teardown |
| `sandbox` | reading each task's `stdout` from the node |
| `condition` | mapping `TASK_*` states and detecting finished/failed |
| `task` | per-task lifecycle record |

## Files

- `partition.go`: split `1..N` into `M` partitions; sum helpers.
- `framework.go`: the subscribe loop, launch on offers, read results on finish, aggregate.
- `main.go`: flags and wiring.
