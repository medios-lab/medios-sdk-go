// Package main runs a Jupyter kernel host as a medios task and proves it serves.
//
// It requests one port, launches a task that runs `jupyter kernelgateway`
// bound to that port on 0.0.0.0, then, from the scheduler host, calls the
// gateway's `/api/kernelspecs` to confirm a Jupyter kernel host is running and
// reachable from another machine.
//
// The kernel gateway serves the standard `/api/kernels/<id>/channels` HTTP and
// WebSocket API, so any Jupyter kernel client can connect to the task at
// (host, port).
//
// The task bootstraps the gateway with `uvx`, so the only node requirement is
// `uv` on PATH.
//
// Scope: host-network only (the gateway binds 0.0.0.0). Bridge/NAT is out of scope.
//
//	go run . -controller http://controller:5150 -user alice -node 10.0.0.12
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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

// kernelHost binds the gateway to $PORT0 on all interfaces. uvx fetches the
// gateway + ipykernel on first run; the PATH export covers the usual uv install
// locations for a non-interactive shell.
const kernelHost = `export PATH="/opt/homebrew/bin:$HOME/.local/bin:$PATH"
exec uvx --python 3.12 --from jupyter-kernel-gateway --with ipykernel jupyter kernelgateway \
  --ip=0.0.0.0 --port=$PORT0 --KernelGatewayApp.allow_origin='*'`

type request struct {
	cpus float64
	mem  float64
	node string
	hold bool // after serving, keep the task up (don't teardown) until interrupt
	// principal is the submitter the task runs on behalf of, forwarded as
	// TaskInfo.Principal.
	principal string
}

type assignment struct {
	taskID string
	host   string
	port   uint64
}

type framework struct {
	client *scheduler.Client
	req    request
	asn    *assignment
}

func (f *framework) onOffers(ctx context.Context, offers []medios.Offer) {
	for _, o := range offers {
		if f.asn != nil {
			f.decline(ctx, o.ID)
			continue
		}
		port, ok := f.fit(o)
		if !ok {
			f.decline(ctx, o.ID)
			continue
		}

		id := "kernel-host-1"
		task, err := medios.NewTaskInfo(f.req.principal, id, medios.ID{Value: id}, o.NodeID,
			[]medios.Resource{
				{Name: "cpus", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.req.cpus}},
				{Name: "mem", Type: "SCALAR", Scalar: &medios.Scalar{Value: f.req.mem}},
				medios.NewPortsResource(port),
			},
			medios.CommandInfo{
				Shell: true,
				Value: kernelHost,
				Environment: &medios.Environment{Variables: []medios.EnvVar{
					{Name: "PORT0", Value: strconv.FormatUint(port, 10)},
				}},
			})
		if err != nil {
			log.Fatalf("build task: %v", err) // the principal was checked at startup
		}

		if err := f.client.Accept(ctx, []medios.ID{o.ID}, []medios.TaskInfo{task}); err != nil {
			log.Printf("accept failed: %v — declining (no leak)", err)
			f.decline(ctx, o.ID)
			continue
		}
		f.asn = &assignment{taskID: id, host: o.Hostname, port: port}
		log.Printf("launched %s on %s (node %s); gateway port=%d", id, o.Hostname, o.NodeID.Value, port)
	}
}

// fit takes the first available port from an offer that meets cpus/mem and the
// optional node filter.
func (f *framework) fit(o medios.Offer) (uint64, bool) {
	if f.req.node != "" && !strings.Contains(o.Hostname, f.req.node) {
		return 0, false
	}
	if scalarFromOffer(o, "cpus") < f.req.cpus || scalarFromOffer(o, "mem") < f.req.mem {
		return 0, false
	}
	ports := medios.PortRangesFromOffer(o).Ports()
	if len(ports) == 0 {
		return 0, false
	}
	return ports[0], true
}

func (f *framework) decline(ctx context.Context, id medios.ID) {
	if err := f.client.Decline(ctx, []medios.ID{id}, declineRefuseSeconds); err != nil {
		log.Printf("decline failed: %v", err)
	}
}

func (f *framework) onUpdate(ctx context.Context, st medios.TaskStatus) (done, ok bool) {
	if st.UUID != "" {
		_ = f.client.Acknowledge(ctx, st.TaskID.Value, st.NodeID.Value, st.UUID)
	}
	if f.asn == nil || st.TaskID.Value != f.asn.taskID {
		return false, false
	}
	log.Printf("task %s: %s %s", st.TaskID.Value, st.State, st.Message)
	switch {
	case st.State == "TASK_RUNNING":
		return true, f.checkServing()
	case st.State.IsTerminal():
		log.Printf("task ended before RUNNING: %s (%s)", st.State, st.Message)
		return true, false
	}
	return false, false
}

// checkServing hits the gateway's /api/kernelspecs at the task's host and confirms
// a Jupyter kernel host is serving. Retries generously: TASK_RUNNING precedes the
// gateway binding, and a cold uvx fetch on the node can take a while.
func (f *framework) checkServing() bool {
	url := fmt.Sprintf("http://%s/api/kernelspecs", net.JoinHostPort(f.asn.host, strconv.FormatUint(f.asn.port, 10)))
	deadline := time.Now().Add(120 * time.Second)
	client := &http.Client{Timeout: 3 * time.Second}

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && strings.Contains(string(body), "python3") {
			log.Printf("SERVING: %s reports a python3 kernelspec — a Jupyter kernel host is live on the cluster", url)
			return true
		}
		time.Sleep(time.Second)
	}
	log.Printf("NOT SERVING: %s never returned a kernelspec within deadline", url)
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
	cpus := flag.Float64("cpus", 0.5, "cpus for the kernel host")
	mem := flag.Float64("mem", 512, "mem (MB) for the kernel host")
	node := flag.String("node", "", "only launch on offers whose hostname contains this substring")
	hold := flag.Bool("hold", false, "after serving, keep the kernel host up until interrupt")
	principal := flag.String("principal", "", "submitter the tasks run on behalf of, kind:name with kind operator, team or run (required)")
	frameworkPrincipal := flag.String("framework-principal", "framework:kernel-host", "framework's own principal sent at SUBSCRIBE (medios#537)")
	flag.Parse()
	if err := medios.ValidatePrincipal(*principal); err != nil {
		log.Fatalf("-principal: %v", err)
	}

	client, err := scheduler.NewClient(*controller, "kernel-host",
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
	log.Printf("subscribed to %s as user %q; launching a kernel host", *controller, *user)

	f := &framework{client: client, req: request{cpus: *cpus, mem: *mem, node: *node, hold: *hold, principal: *principal}}
	_ = client.Revive(ctx)

	exit := func(ok bool) {
		_ = client.Teardown(context.Background())
		if ok {
			log.Print("PASS: a Jupyter kernel host runs as a medios task and serves cross-host; torn down")
			os.Exit(0)
		}
		log.Print("FAIL: kernel host did not come up serving; torn down")
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
					if ok && f.req.hold {
						log.Printf("HOLDING %s — kernel host stays up until interrupt (Ctrl-C tears down)",
							net.JoinHostPort(f.asn.host, strconv.FormatUint(f.asn.port, 10)))
					} else {
						exit(ok)
					}
				}
			}
		}
	}
}
