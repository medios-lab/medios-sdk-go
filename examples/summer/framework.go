// Command summer is a demonstration Medios framework built on
// medios-sdk-go. It runs against a live medios cluster only, there is
// no mock or test harness.
//
// It computes 1 + 2 + ... + N by partitioning the range into M contiguous
// slices and launching exactly M tasks (one per slice). Each task computes the
// sum of its own slice on the node and writes that number to stdout. As each
// task finishes, the framework reads its stdout back from the node sandbox
// (via the SDK sandbox package), parses the partial sum, and aggregates. When
// all M have finished it verifies the total against N(N+1)/2, tears the
// framework down, and exits.
//
// The result comes from the tasks' actual output, not from any framework-side
// arithmetic. The constraint "M is the number of jobs" is literal: M is both
// the partition count and the task count; the framework never launches more.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/medios-lab/medios-sdk-go/medios"
	"github.com/medios-lab/medios-sdk-go/sandbox"
	"github.com/medios-lab/medios-sdk-go/scheduler"
)

// taskRecord is summer's own per-task tracking. A persisted task record with an
// owning entity is each framework's model, not the SDK's, so it lives here
// rather than in the library (see medios-sdk-go#4).
type taskRecord struct {
	ID          string
	OwnerID     string
	NodeID      string
	ContainerID string
	Condition   medios.TaskState
}

// Framework holds the run state for one sum computation.
type Framework struct {
	n       int64
	m       int
	cpusPer float64
	memPer  float64
	// principal is the submitter every task runs on behalf of, forwarded as
	// TaskInfo.Principal; checked once in NewFramework.
	principal string
	client    *scheduler.Client
	sandbox   *sandbox.Resolver
	log       *slog.Logger

	parts    []Partition
	launched map[int]bool           // partition index -> launched
	tasks    map[string]*taskRecord // task_id -> SDK task record
	taskPart map[string]int         // task_id -> partition index
	results  map[int]int64          // partition index -> partial sum read from task stdout
}

// NewFramework builds a Framework. cpusPer/memPer are the resources requested
// per task; principal is the submitter the tasks run on behalf of, refused
// here when empty or malformed. resolver reads task stdout from the nodes.
func NewFramework(client *scheduler.Client, resolver *sandbox.Resolver, principal string, n int64, m int, cpusPer, memPer float64, log *slog.Logger) (*Framework, error) {
	if err := medios.ValidatePrincipal(principal); err != nil {
		return nil, err
	}
	return &Framework{
		n: n, m: m, cpusPer: cpusPer, memPer: memPer, principal: principal,
		client:   client,
		sandbox:  resolver,
		log:      log,
		parts:    partition(n, m),
		launched: map[int]bool{},
		tasks:    map[string]*taskRecord{},
		taskPart: map[string]int{},
		results:  map[int]int64{},
	}, nil
}

// Run subscribes, drives the framework to completion, and returns the verified
// total. It returns an error if a task fails, a result can't be read or parsed,
// the stream closes early, or the aggregate doesn't match N(N+1)/2.
func (f *Framework) Run(ctx context.Context) (int64, error) {
	events, errs, err := f.client.Subscribe(ctx)
	if err != nil {
		return 0, fmt.Errorf("subscribe: %w", err)
	}
	f.log.Info("framework starting", "n", f.n, "m", f.m, "partitions", len(f.parts))

	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case err, ok := <-errs:
			if !ok {
				errs = nil // stream error channel closed; stop selecting on it
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("subscribe stream: %w", err)
			}
		case ev, ok := <-events:
			if !ok {
				return 0, fmt.Errorf("stream closed early: %d/%d partitions finished", len(f.results), f.m)
			}
			switch ev.Type {
			case medios.EventSubscribed:
				f.log.Info("subscribed", "framework_id", f.client.FrameworkID())
			case medios.EventOffers:
				if ev.Offers == nil {
					continue
				}
				if err := f.onOffers(ctx, ev.Offers.Offers); err != nil {
					return 0, err
				}
			case medios.EventUpdate:
				if ev.Update == nil {
					continue
				}
				done, err := f.onUpdate(ctx, ev.Update.Status)
				if err != nil {
					return 0, err
				}
				if done {
					return f.finish(ctx)
				}
			}
		}
	}
}

// onOffers launches one task per still-unlaunched partition that fits in an
// offer, declining offers it can't use. The M-task ceiling is enforced by only
// ever iterating the M partitions and skipping any already launched.
func (f *Framework) onOffers(ctx context.Context, offers []medios.Offer) error {
	for _, off := range offers {
		cpus := scalarResource(off.Resources, "cpus")
		mem := scalarResource(off.Resources, "mem")

		var launch []medios.TaskInfo
		for i := range f.parts {
			p := f.parts[i]
			if f.launched[p.Index] {
				continue
			}
			if cpus < f.cpusPer || mem < f.memPer {
				break // this offer is exhausted
			}
			ti, err := f.buildTask(off, p)
			if err != nil {
				return err
			}
			launch = append(launch, ti)
			f.launched[p.Index] = true
			f.taskPart[ti.TaskID.Value] = p.Index
			f.tasks[ti.TaskID.Value] = &taskRecord{
				ID:        ti.TaskID.Value,
				OwnerID:   fmt.Sprintf("partition-%d", p.Index),
				NodeID:    off.NodeID.Value,
				Condition: medios.TaskStaging,
			}
			cpus -= f.cpusPer
			mem -= f.memPer
		}

		if len(launch) > 0 {
			f.log.Info("launching", "offer", off.ID.Value, "host", off.Hostname, "tasks", len(launch))
			if err := f.client.Accept(ctx, []medios.ID{off.ID}, launch); err != nil {
				return fmt.Errorf("accept offer %s: %w", off.ID.Value, err)
			}
		} else {
			if err := f.client.Decline(ctx, []medios.ID{off.ID}, 5.0); err != nil {
				return fmt.Errorf("decline offer %s: %w", off.ID.Value, err)
			}
		}
	}
	return nil
}

// onUpdate folds a task status into run state, acknowledges it, captures the
// container id on RUNNING, and on FINISHED reads the task's stdout to obtain
// the real partial sum. Reports done==true once all M partitions are in.
func (f *Framework) onUpdate(ctx context.Context, st medios.TaskStatus) (bool, error) {
	id := st.TaskID.Value

	t := f.tasks[id]
	if t == nil {
		f.log.Warn("status for unknown task", "task_id", id, "state", st.State)
	} else {
		t.Condition = st.State
		if st.ContainerStatus != nil {
			t.ContainerID = st.ContainerStatus.ContainerID.Value
		}
	}

	if st.UUID != "" {
		if err := f.client.Acknowledge(ctx, id, st.NodeID.Value, st.UUID); err != nil {
			return false, fmt.Errorf("acknowledge %s: %w", id, err)
		}
	}

	if st.State.IsFailure() {
		return false, fmt.Errorf("task %s failed (state=%s): %s", id, st.State, st.Message)
	}

	if st.State == medios.TaskFinished {
		idx, ok := f.taskPart[id]
		if !ok {
			return false, fmt.Errorf("finished task %s has no partition mapping", id)
		}
		if _, seen := f.results[idx]; !seen {
			partial, err := f.readResult(ctx, st.NodeID.Value, id)
			if err != nil {
				return false, fmt.Errorf("read result for partition %d (task %s): %w", idx, id, err)
			}
			f.results[idx] = partial
			p := f.parts[idx]
			f.log.Info("partition finished",
				"index", idx,
				"range", fmt.Sprintf("%d..%d", p.Lo, p.Hi),
				"partial", partial,
				"progress", fmt.Sprintf("%d/%d", len(f.results), f.m))
		}
		return len(f.results) == f.m, nil
	}
	return false, nil
}

// readResult fetches the task's stdout from the node sandbox and parses the
// single integer the task printed. No fallback: a missing or unparseable
// result is an error, not a guess.
func (f *Framework) readResult(ctx context.Context, nodeID, taskID string) (int64, error) {
	t := f.tasks[taskID]
	if t == nil || t.ContainerID == "" {
		return 0, fmt.Errorf("no container id (never saw TASK_RUNNING)")
	}
	node, err := f.sandbox.Locate(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	p := node.PathFor(f.client.FrameworkID(), taskID, t.ContainerID, "stdout")
	res, err := f.sandbox.Read(ctx, node, p, 0, -1)
	if err != nil {
		return 0, err
	}
	val, err := strconv.ParseInt(strings.TrimSpace(res.Data), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stdout %q is not an integer: %w", res.Data, err)
	}
	return val, nil
}

// finish aggregates the real per-task results, tears down, and verifies.
func (f *Framework) finish(ctx context.Context) (int64, error) {
	var total int64
	for _, v := range f.results {
		total += v
	}
	f.log.Info("all partitions finished", "sum", total)
	if err := f.client.Teardown(ctx); err != nil {
		f.log.Warn("teardown", "err", err)
	}
	if want := totalSum(f.n); total != want {
		return total, fmt.Errorf("sum mismatch: tasks reported %d, want %d", total, want)
	}
	return total, nil
}

// buildTask turns a partition into a Medios TaskInfo whose command sums the
// slice on the node and prints the result to stdout (awk for portability;
// empty slices print 0).
func (f *Framework) buildTask(off medios.Offer, p Partition) (medios.TaskInfo, error) {
	id := fmt.Sprintf("summer.%d", p.Index)
	return medios.NewTaskInfo(f.principal, id, medios.ID{Value: id}, off.NodeID,
		[]medios.Resource{
			{Name: "cpus", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.cpusPer}},
			{Name: "mem", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.memPer}},
		},
		medios.CommandInfo{
			Shell: true,
			Value: fmt.Sprintf("awk 'BEGIN{s=0; for(i=%d;i<=%d;i++) s+=i; print s}'", p.Lo, p.Hi),
		})
}

// scalarResource returns the value of the named SCALAR resource, or 0.
func scalarResource(rs []medios.Resource, name string) float64 {
	for _, r := range rs {
		if r.Name == name && r.Scalar != nil {
			return r.Scalar.Value
		}
	}
	return 0
}
