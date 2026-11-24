# multi-role

Subscribes under the default role `*` and one more role with
`scheduler.WithRoles`, which also declares `MULTI_ROLE`. For every offer it
logs the node, the offer's `allocation_info.role`, and each resource with its
quantity and its own allocation role, then declines the offer. It never
launches a task. On exit, at `-duration` or on SIGINT, it sends `TEARDOWN`.

```sh
go run ./examples/multi-role -controller http://host:5150 -role pool/batch -duration 20s
```

A line looks like:

```
offer O-2 node=N-2 host=n2 role=pool/batch cpus=2@pool/batch exclusive_slot=1@pool/batch
```

The controller builds one offer per node and role, so a node with supply for
both roles shows up in two offers. A resource is offered to a role only if it
is unreserved (`*`) or reserved to that role or one of its ancestors.

`go test ./examples/multi-role` runs the framework against a fake controller
that offers supply for two roles.
