// Package main is a framework that subscribes under the default role "*" and
// one more role, logs the node, role and resources of every offer it
// receives, and declines them all. It never launches a task, so it can run
// against a live cluster to show which supply each role reaches.
//
//	go run ./examples/multi-role -controller http://host:5150 -role pool/batch -duration 20s
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
	"github.com/medios-lab/medios-sdk-go/scheduler"
)

// declineRefuseSeconds is how long the controller withholds a declined offer
// from this framework. Parameter: 5 s brings each node back several times in
// a 20 s run, so the log shows the offers are stable.
const declineRefuseSeconds = 5.0

// teardownTimeout bounds the TEARDOWN sent on exit. Parameter: one call to a
// reachable controller returns well inside 10 s.
const teardownTimeout = 10 * time.Second

// run subscribes, logs and declines every offer until ctx ends or the stream
// does, then tears the framework down so the controller keeps nothing of it.
//
// The stream has its own context, closed only after TEARDOWN: the controller
// removes a framework with failover_timeout 0 as soon as its stream drops,
// and then answers TEARDOWN with "Framework cannot be found".
func run(ctx context.Context, c *scheduler.Client, logf func(string, ...any)) error {
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	events, errs, err := c.Subscribe(streamCtx)
	if err != nil {
		return err
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
		defer cancel()
		if err := c.Teardown(tctx); err != nil {
			logf("teardown: %v", err)
			return
		}
		logf("torn down")
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-errs:
			if ok && err != nil {
				return err
			}
			errs = nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			switch ev.Type {
			case medios.EventSubscribed:
				logf("subscribed framework_id=%s", c.FrameworkID())
			case medios.EventOffers:
				ids := make([]medios.ID, 0, len(ev.Offers.Offers))
				for _, o := range ev.Offers.Offers {
					logf("%s", describe(o))
					ids = append(ids, o.ID)
				}
				if err := c.Decline(ctx, ids, declineRefuseSeconds); err != nil && ctx.Err() == nil {
					return fmt.Errorf("decline: %w", err)
				}
			case medios.EventError:
				return errors.New("controller error: " + ev.Error.Message)
			}
		}
	}
}

// describe renders one offer as a log line: node, the offer's role, then each
// resource with its quantity and its own allocation role.
func describe(o medios.Offer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "offer %s node=%s host=%s role=%s", o.ID.Value, o.NodeID.Value, o.Hostname, roleOf(o.AllocationInfo))
	for _, r := range o.Resources {
		fmt.Fprintf(&b, " %s=%s@%s", r.Name, quantity(r), roleOf(r.AllocationInfo))
	}
	return b.String()
}

func roleOf(a *medios.AllocationInfo) string {
	if a == nil {
		return "(no allocation_info)"
	}
	return a.Role
}

func quantity(r medios.Resource) string {
	switch {
	case r.Scalar != nil:
		return fmt.Sprintf("%g", r.Scalar.Value)
	case r.Ranges != nil:
		parts := make([]string, 0, len(r.Ranges.Range))
		for _, rg := range r.Ranges.Range {
			parts = append(parts, fmt.Sprintf("%d-%d", rg.Begin, rg.End))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	return "?"
}

func main() {
	controller := flag.String("controller", "", "Medios controller URL (required)")
	role := flag.String("role", "", "role to subscribe under besides \"*\" (required)")
	duration := flag.Duration("duration", 0, "how long to run before tearing down (required, > 0)")
	principal := flag.String("principal", "framework:multi-role-example", "framework principal sent at SUBSCRIBE (medios#537)")
	flag.Parse()
	if *controller == "" || *role == "" || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "multi-role: -controller, -role and a positive -duration are required")
		flag.Usage()
		os.Exit(2)
	}

	c, err := scheduler.NewClient(*controller, "multi-role-example",
		scheduler.WithPrincipal(*principal),
		scheduler.WithRoles("*", *role))
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	if err := run(ctx, c, log.Printf); err != nil {
		log.Fatal(err)
	}
}
