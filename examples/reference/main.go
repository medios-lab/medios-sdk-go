// Package main is a complete reference framework on medios-sdk-go — the
// canonical "how to write a well-behaved Medios framework." Where the README's
// snippet is deliberately minimal, this covers every behavior a real framework
// needs to be correct AND a good cluster citizen:
//
//   - WithUser: framework_info.user must be a real user on the nodes, or every
//     task silently goes TASK_LOST.
//   - Launch pending work on offers that fit; build a TaskInfo from the offer.
//   - DECLINE every offer you do not launch against, with a refuse filter. An
//     offer you neither ACCEPT nor DECLINE stays outstanding to you — that is
//     offer hoarding, and it starves every other framework on the cluster.
//   - DECLINE on accept error — never leak the offer.
//   - SUPPRESS when you have nothing to launch; REVIVE (rate-limited) when work
//     appears. An idle framework that keeps taking offers is the same hoard.
//   - ACKNOWLEDGE status updates that carry a UUID (or the node retransmits).
//   - Classify lifecycle via medios.TaskState (IsTerminal / IsFailure).
//   - TEARDOWN on exit.
//
// The suppress/revive + decline-with-filter policy is Marathon's offer-revive
// lifecycle; the parameters (120s refuse, 5s revive throttle) are Marathon's
// defaults. The SDK provides the calls; this is one faithful way to drive them.
//
// This framework runs entirely on one goroutine (the event loop), so it needs
// no locks. A framework that also mutates state from another goroutine (e.g. a
// REST API) must guard shared state.
//
// Workload: launch N short tasks, wait for all to reach a terminal state, exit.
//
//	go run . -controller http://127.0.0.1:5150 -user <a-real-node-user> -n 3
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
	"github.com/medios-lab/medios-sdk-go/scheduler"
)

const (
	// Marathon's offer-handling defaults.
	declineRefuseSeconds = 120.0           // refuse re-offer of a declined offer for 120s
	minReviveInterval    = 5 * time.Second // throttle REVIVE so we don't spam the controller
)

type framework struct {
	client    *scheduler.Client
	cpus, mem float64
	cmd       string
	principal string // forwarded as TaskInfo.Principal on every launch

	pending  int // tasks still to launch
	running  int // launched, awaiting a terminal update
	launched int // total launched, for unique task ids

	suppressed   bool
	lastReviveAt time.Time
}

// onOffers launches pending work on offers that fit and declines the rest.
// The invariant: every offer is ACCEPTed or DECLINEd before we return — none
// is ever held.
func (f *framework) onOffers(ctx context.Context, offers []medios.Offer) {
	for _, o := range offers {
		if f.pending > 0 && offerFits(o, f.cpus, f.mem) {
			id := fmt.Sprintf("ref-%d", f.launched)
			task, err := medios.NewTaskInfo(f.principal, id, medios.ID{Value: id}, o.NodeID,
				[]medios.Resource{
					{Name: "cpus", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.cpus}},
					{Name: "mem", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.mem}},
				},
				medios.CommandInfo{Shell: true, Value: f.cmd})
			if err != nil {
				log.Fatalf("build task: %v", err) // the principal was checked at startup
			}
			if err := f.client.Accept(ctx, []medios.ID{o.ID}, []medios.TaskInfo{task}); err != nil {
				log.Printf("accept %s failed: %v — declining offer (no leak)", o.ID.Value, err)
				f.decline(ctx, o.ID) // never leak an offer on accept error
				continue
			}
			f.pending--
			f.running++
			f.launched++
			log.Printf("launched %s on node %s (pending=%d running=%d)", id, o.NodeID.Value, f.pending, f.running)
		} else {
			// Nothing to launch against this offer → decline with a filter so
			// the controller doesn't immediately re-offer it.
			f.decline(ctx, o.ID)
		}
	}
	f.reconcileOffers(ctx)
}

func (f *framework) decline(ctx context.Context, id medios.ID) {
	if err := f.client.Decline(ctx, []medios.ID{id}, declineRefuseSeconds); err != nil {
		log.Printf("decline %s failed: %v", id.Value, err)
	}
}

// onUpdate acknowledges the update and tracks terminal state.
func (f *framework) onUpdate(ctx context.Context, st medios.TaskStatus) {
	if st.UUID != "" {
		// Updates with a UUID must be acknowledged or the node retransmits them.
		if err := f.client.Acknowledge(ctx, st.TaskID.Value, st.NodeID.Value, st.UUID); err != nil {
			log.Printf("ack %s failed: %v", st.TaskID.Value, err)
		}
	}
	if st.State.IsTerminal() {
		f.running--
		outcome := "ok"
		if st.State.IsFailure() {
			outcome = "FAILURE"
		}
		log.Printf("task %s terminal: %s (%s) (running=%d)", st.TaskID.Value, st.State, outcome, f.running)
	}
	f.reconcileOffers(ctx)
}

// reconcileOffers matches the controller's offer flow to whether we have work:
// SUPPRESS when there's nothing to launch, REVIVE (throttled) when there is.
func (f *framework) reconcileOffers(ctx context.Context) {
	switch {
	case f.pending == 0 && !f.suppressed:
		if err := f.client.Suppress(ctx); err != nil {
			log.Printf("suppress failed: %v", err)
			return
		}
		f.suppressed = true
		log.Print("suppressed offers (nothing to launch)")
	case f.pending > 0 && (f.suppressed || time.Since(f.lastReviveAt) >= minReviveInterval):
		if err := f.client.Revive(ctx); err != nil {
			log.Printf("revive failed: %v", err)
			return
		}
		f.suppressed = false
		f.lastReviveAt = time.Now()
		log.Printf("revived offers (pending=%d)", f.pending)
	}
}

func (f *framework) done() bool { return f.pending == 0 && f.running == 0 }

// offerFits reports whether the offer has at least the requested cpus and mem.
func offerFits(o medios.Offer, cpus, mem float64) bool {
	return scalarFromOffer(o, "cpus") >= cpus && scalarFromOffer(o, "mem") >= mem
}

func scalarFromOffer(o medios.Offer, name string) float64 {
	var total float64
	for _, r := range o.Resources {
		if r.Name == name && r.Scalar != nil {
			total += r.Scalar.Value
		}
	}
	return total
}

func main() {
	controller := flag.String("controller", "http://127.0.0.1:5150", "Medios controller URL")
	user := flag.String("user", os.Getenv("USER"), "framework_info.user — MUST be a real user on the nodes")
	n := flag.Int("n", 3, "number of tasks to launch")
	cpus := flag.Float64("cpus", 0.1, "cpus per task")
	mem := flag.Float64("mem", 32, "mem (MB) per task")
	cmd := flag.String("cmd", "echo hello-from-reference", "shell command each task runs")
	principal := flag.String("principal", "", "submitter the tasks run on behalf of, kind:name with kind operator, team or run (required)")
	frameworkPrincipal := flag.String("framework-principal", "framework:medios-reference", "framework's own principal sent at SUBSCRIBE (medios#537)")
	flag.Parse()
	if err := medios.ValidatePrincipal(*principal); err != nil {
		log.Fatalf("-principal: %v", err)
	}

	client, err := scheduler.NewClient(*controller, "reference",
		scheduler.WithUser(*user),
		scheduler.WithPrincipal(*frameworkPrincipal))
	if err != nil {
		log.Fatalf("new client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	events, errs, err := client.Subscribe(ctx)
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}
	log.Printf("subscribed to %s as user %q; launching %d task(s)", *controller, *user, *n)

	f := &framework{client: client, cpus: *cpus, mem: *mem, cmd: *cmd, principal: *principal, pending: *n}
	f.reconcileOffers(ctx) // pending > 0 → REVIVE so offers start flowing

	for {
		select {
		case <-ctx.Done():
			_ = client.Teardown(context.Background())
			log.Print("interrupted; torn down")
			return
		case err := <-errs:
			log.Fatalf("stream error: %v", err)
		case ev := <-events:
			switch ev.Type {
			case medios.EventSubscribed:
				log.Printf("framework_id=%s", client.FrameworkID())
			case medios.EventOffers:
				f.onOffers(ctx, ev.Offers.Offers)
			case medios.EventUpdate:
				f.onUpdate(ctx, ev.Update.Status)
			}
			if f.done() {
				log.Printf("all %d task(s) terminal; tearing down", *n)
				_ = client.Teardown(context.Background())
				return
			}
		}
	}
}
