package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/medios-lab/medios-sdk-go/sandbox"
	"github.com/medios-lab/medios-sdk-go/scheduler"
)

func main() {
	n := flag.Int64("n", 100, "compute the sum 1..N")
	m := flag.Int("m", 4, "number of jobs (partitions / parallelism); M is the task count")
	controller := flag.String("controller", "", "Medios controller URL, e.g. http://host:5050 (required)")
	cpus := flag.Float64("cpus", 0.1, "cpus requested per task")
	mem := flag.Float64("mem", 32, "mem (MB) requested per task")
	user := flag.String("user", "", "framework user; MUST exist on the node (defaults to local OS user, which is wrong for a remote cluster)")
	principal := flag.String("principal", "", "submitter the tasks run on behalf of, kind:name with kind operator, team or run (required)")
	frameworkPrincipal := flag.String("framework-principal", "framework:summer", "framework's own principal sent at SUBSCRIBE (medios#537)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Required config is explicit; no fallbacks. A missing value fails loudly
	// here rather than producing a silently misdirected sandbox read later.
	switch {
	case *n < 1 || *m < 1:
		fatal(log, "n and m must both be >= 1")
	case int64(*m) > *n:
		fatal(log, fmt.Sprintf("m (%d) must be <= n (%d): each job covers >= 1 number", *m, *n))
	case *controller == "":
		fatal(log, "-controller is required")
	case *principal == "":
		fatal(log, "-principal is required: the submitter the tasks run on behalf of, as kind:name")
	}

	opts := []scheduler.ClientOption{
		scheduler.WithPrincipal(*frameworkPrincipal),
	}
	if *user != "" {
		opts = append(opts, scheduler.WithUser(*user))
	}
	client, err := scheduler.NewClient(*controller, "summer", opts...)
	if err != nil {
		fatal(log, fmt.Sprintf("new client: %v", err))
	}
	resolver := &sandbox.Resolver{ControllerURL: *controller}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fw, err := NewFramework(client, resolver, *principal, *n, *m, *cpus, *mem, log)
	if err != nil {
		fatal(log, fmt.Sprintf("-principal: %v", err))
	}
	total, err := fw.Run(ctx)
	if err != nil {
		fatal(log, fmt.Sprintf("framework failed: %v", err))
	}
	fmt.Printf("sum(1..%d) = %d  [verified from %d task results]\n", *n, total, *m)
}

func fatal(log *slog.Logger, msg string) {
	log.Error(msg)
	os.Exit(2)
}
