# Example frameworks

Complete, runnable frameworks built on `medios-sdk-go`.

These are not tests. The package tests (`go test ./...`) run with no cluster and
exercise each package in isolation. An example is a real `main` that subscribes
to a live Medios controller, launches actual tasks, and reads their results
back. Reach for one when you want to see a whole framework wired together, or to
reproduce end-to-end behavior against a cluster.

Every example runs against a live cluster only, no mock, and shares a few flags:

| Flag | Meaning |
|---|---|
| `-controller` | Medios controller URL, e.g. `http://host:5150`. |
| `-user` | Framework user; must be a real user on the node, or tasks return `TASK_LOST`. |
| `-principal` | The submitter the tasks run on behalf of, `kind:name` with kind `operator`, `team` or `run`, e.g. `operator:alice`. Required; the controller refuses a task without one. |
| `-work-dir` | The node's `--work_dir`; required to locate task sandboxes. |
| `-node-port` | The node's HTTP port, used to read task output. |

## `summer`

Computes `1+…+N` as `M` partition tasks: each task sums its slice on the node
and prints the result; the framework reads each result back and aggregates,
verifying the total against `N(N+1)/2`. It touches the full SDK surface,
`scheduler` (subscribe, offer matching, accept/launch, acknowledge, teardown),
`sandbox` (reading task stdout), and `condition`/`task` (lifecycle tracking),
which makes it a good smoke test of a cluster as well as a worked example.

```sh
go run ./examples/summer \
  -controller http://10.0.0.12:5150 \
  -user alice \
  -principal operator:alice \
  -work-dir /tmp/medios-node-work \
  -node-port 5151 \
  -n 100 -m 2
```

Files: `partition.go` (range math), `framework.go` (the subscribe/launch/read
loop), `main.go` (flags and wiring).

## `multi-role`

Subscribes under `*` and one more role, logs each offer's node, role and
resources, and declines everything; it never launches. See
[`multi-role/README.md`](multi-role/README.md).
