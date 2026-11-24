// Package main is a ports round-trip framework on medios-sdk-go: it requests N
// dynamic ports, launches one task that binds the first of them, then connects
// to that port from here (the scheduler host) to prove the grant is reachable.
//
// A framework that wants ports asks for a `ports` RANGES resource,
// reads the assigned ports off the offer, reserves a sub-range onto the task,
// and injects $PORT0..$PORT(N-1) into the task env (the Marathon convention —
// the platform offers/accounts ports; the framework assigns them to env).
//
// What it proves end-to-end:
//   - the offer carries a ports RANGES (medios advertises ports)
//   - ACCEPT reserves a sub-range onto the task (medios accounts ports)
//   - the task binds $PORT0 and is reachable at (offer.hostname, port) from the
//     scheduler host (host-network reachability)
//
// Scope: host-network only. The task binds 0.0.0.0 and runs without network
// isolation, so reachability is the node's host IP with no NAT. Bridge /
// container-network reachability needs a port-mapping isolator that medios
// does not ship, and is out of scope here.
//
//	go run . -controller http://controller:5150 -user <a-real-node-user> -ports 5
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
	"github.com/medios-lab/medios-sdk-go/scheduler"
)

const declineRefuseSeconds = 120.0

// taskListener binds $PORT0 on all interfaces and answers every connection with
// a token naming the port, so the reachability check confirms it reached OUR
// task and not some other listener. python3 is the one interpreter we can count
// on across the node fleet; iterate here if a node lacks it.
const taskListener = `python3 -c '
import socket, os
p = int(os.environ["PORT0"])
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", p))
s.listen(8)
print("listening on", p, flush=True)
while True:
    c, _ = s.accept()
    c.sendall(("medios-ports-ok:%d\n" % p).encode())
    c.close()
'`

// request is the immutable ask: the resource shape to request and an optional
// node constraint. Set once from flags; never mutated.
type request struct {
	nports int
	cpus   float64
	mem    float64
	node   string // if set, only launch on offers whose hostname contains this
	// principal is the submitter the task runs on behalf of, forwarded as
	// TaskInfo.Principal.
	principal string
}

// assignment is what a matched offer granted: the task id, the host it landed
// on, and the concrete ports it holds. Recorded at launch and read by the
// reachability check when the task reaches RUNNING. nil until launch.
type assignment struct {
	taskID string
	host   string
	ports  []uint64
}

type framework struct {
	client *scheduler.Client
	req    request
	asn    *assignment
}

// onOffers launches the single ports task on the first offer that fits, and
// declines everything else (never hoard).
func (f *framework) onOffers(ctx context.Context, offers []medios.Offer) {
	for _, o := range offers {
		if f.asn != nil {
			f.decline(ctx, o.ID)
			continue
		}

		assigned, ok := f.fit(o)
		if !ok {
			f.decline(ctx, o.ID)
			continue
		}

		// Reserve exactly the N assigned ports and inject them as $PORT0..
		env := make([]medios.EnvVar, len(assigned))
		for i, p := range assigned {
			env[i] = medios.EnvVar{Name: fmt.Sprintf("PORT%d", i), Value: strconv.FormatUint(p, 10)}
		}

		id := "ports-1"
		task, err := medios.NewTaskInfo(f.req.principal, id, medios.ID{Value: id}, o.NodeID,
			[]medios.Resource{
				{Name: "cpus", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.req.cpus}},
				{Name: "mem", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.req.mem}},
				medios.NewPortsResource(assigned...),
			},
			medios.CommandInfo{
				Shell:       true,
				Value:       taskListener,
				Environment: &medios.Environment{Variables: env},
			})
		if err != nil {
			log.Fatalf("build task: %v", err) // the principal was checked at startup
		}

		if err := f.client.Accept(ctx, []medios.ID{o.ID}, []medios.TaskInfo{task}); err != nil {
			log.Printf("accept failed: %v — declining (no leak)", err)
			f.decline(ctx, o.ID)
			continue
		}

		f.asn = &assignment{taskID: id, host: o.Hostname, ports: assigned}
		log.Printf("launched %s on %s (node %s); assigned ports=%v", id, o.Hostname, o.NodeID.Value, assigned)
	}
}

// fit reports whether the offer satisfies cpus, mem, and >= nports ports, and
// if so returns the first nports individual port numbers to reserve.
func (f *framework) fit(o medios.Offer) ([]uint64, bool) {
	if f.req.node != "" && !strings.Contains(o.Hostname, f.req.node) {
		return nil, false
	}
	if scalarFromOffer(o, "cpus") < f.req.cpus || scalarFromOffer(o, "mem") < f.req.mem {
		return nil, false
	}
	avail := medios.PortRangesFromOffer(o).Ports()
	if len(avail) < f.req.nports {
		log.Printf("offer from %s short on ports: have %d, need %d", o.Hostname, len(avail), f.req.nports)
		return nil, false
	}
	return avail[:f.req.nports], true
}

func (f *framework) decline(ctx context.Context, id medios.ID) {
	if err := f.client.Decline(ctx, []medios.ID{id}, declineRefuseSeconds); err != nil {
		log.Printf("decline failed: %v", err)
	}
}

// onUpdate runs the reachability check when the task reaches RUNNING, and
// reports failure for any terminal-before-running state.
func (f *framework) onUpdate(ctx context.Context, st medios.TaskStatus) (done bool, ok bool) {
	if st.UUID != "" {
		_ = f.client.Acknowledge(ctx, st.TaskID.Value, st.NodeID.Value, st.UUID)
	}
	if f.asn == nil || st.TaskID.Value != f.asn.taskID {
		return false, false
	}

	log.Printf("task %s: %s %s", st.TaskID.Value, st.State, st.Message)

	switch {
	case st.State == "TASK_RUNNING":
		reachable := f.checkReachable()
		return true, reachable
	case st.State.IsTerminal():
		log.Printf("task ended before RUNNING: %s (%s)", st.State, st.Message)
		return true, false
	}
	return false, false
}

// checkReachable dials the first assigned port at the task's host and verifies
// the task's token comes back. Retries briefly: TASK_RUNNING can precede the
// listener actually binding.
func (f *framework) checkReachable() bool {
	addr := net.JoinHostPort(f.asn.host, strconv.FormatUint(f.asn.ports[0], 10))
	deadline := time.Now().Add(15 * time.Second)
	want := fmt.Sprintf("medios-ports-ok:%d", f.asn.ports[0])

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		buf := make([]byte, 64)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		conn.Close()
		if err == nil && len(buf[:n]) > 0 {
			got := string(buf[:n])
			if len(got) >= len(want) && got[:len(want)] == want {
				log.Printf("REACHABLE: %s returned %q", addr, got[:len(want)])
				return true
			}
			log.Printf("connected to %s but token mismatch: %q", addr, got)
			return false
		}
	}
	log.Printf("NOT REACHABLE: %s never answered within deadline", addr)
	return false
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
	nports := flag.Int("ports", 5, "number of dynamic ports to request (5 = the ZMQ kernel case)")
	cpus := flag.Float64("cpus", 0.1, "cpus for the task")
	mem := flag.Float64("mem", 64, "mem (MB) for the task")
	node := flag.String("node", "", "only launch on offers whose hostname contains this substring")
	principal := flag.String("principal", "", "submitter the tasks run on behalf of, kind:name with kind operator, team or run (required)")
	frameworkPrincipal := flag.String("framework-principal", "framework:medios-ports", "framework's own principal sent at SUBSCRIBE (medios#537)")
	flag.Parse()
	if err := medios.ValidatePrincipal(*principal); err != nil {
		log.Fatalf("-principal: %v", err)
	}

	client, err := scheduler.NewClient(*controller, "ports",
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
	log.Printf("subscribed to %s as user %q; requesting %d ports", *controller, *user, *nports)

	f := &framework{
		client: client,
		req:    request{nports: *nports, cpus: *cpus, mem: *mem, node: *node, principal: *principal},
	}
	_ = client.Revive(ctx)

	exit := func(ok bool) {
		_ = client.Teardown(context.Background())
		if ok {
			log.Print("PASS: ports round-trip reachable end-to-end; torn down")
			os.Exit(0)
		}
		log.Print("FAIL: ports round-trip did not complete; torn down")
		os.Exit(1)
	}

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
				if done, ok := f.onUpdate(ctx, ev.Update.Status); done {
					exit(ok)
				}
			}
		}
	}
}
