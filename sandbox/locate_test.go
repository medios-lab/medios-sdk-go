package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNode serves /state with its work_dir and /files/read with a body that
// names the node, so a read that reaches the wrong node is visible.
func fakeNode(t *testing.T, workDir, says string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var stateCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		stateCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"flags": map[string]string{"work_dir": workDir}})
	})
	mux.HandleFunc("/files/read", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ReadResult{Data: says + " read " + r.URL.Query().Get("path")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &stateCalls
}

// fakeController lists nodes by id with the pid medios gives them.
func fakeController(t *testing.T, pids map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes" {
			t.Errorf("controller path: want /nodes, got %s", r.URL.Path)
		}
		calls.Add(1)
		var nodes []map[string]string
		for id, pid := range pids {
			nodes = append(nodes, map[string]string{"id": id, "pid": pid})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func pidFor(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	host, port := splitHostPort(t, srv.URL)
	return fmt.Sprintf("node(1)@%s:%s", host, port)
}

// One host, two nodes, two ports, two work dirs: each task's sandbox is read
// from the node it ran on. With one port and one work_dir per process, the
// second node's tasks were looked for in the first node's tree.
func TestResolver_Locate_TwoNodesOnOneHostResolveSeparately(t *testing.T) {
	first, _ := fakeNode(t, "/var/lib/medios/node", "first")
	second, _ := fakeNode(t, "/var/lib/medios/node2", "second")
	ctl, _ := fakeController(t, map[string]string{"S1": pidFor(t, first), "S3": pidFor(t, second)})
	r := &Resolver{ControllerURL: ctl.URL}
	ctx := context.Background()

	for _, tc := range []struct{ nodeID, workDir, says string }{
		{"S1", "/var/lib/medios/node", "first"},
		{"S3", "/var/lib/medios/node2", "second"},
	} {
		n, err := r.Locate(ctx, tc.nodeID)
		if err != nil {
			t.Fatalf("Locate(%s): %v", tc.nodeID, err)
		}
		if n.WorkDir != tc.workDir {
			t.Errorf("Locate(%s).WorkDir = %q, want %q", tc.nodeID, n.WorkDir, tc.workDir)
		}
		p := n.PathFor("fw-1", "task-1", "ctr-1", "stderr")
		want := tc.workDir + "/nodes/" + tc.nodeID + "/frameworks/fw-1/executors/task-1/runs/ctr-1/stderr"
		if p != want {
			t.Errorf("PathFor on %s = %q, want %q", tc.nodeID, p, want)
		}
		out, err := r.Read(ctx, n, p, 0, -1)
		if err != nil {
			t.Fatalf("Read on %s: %v", tc.nodeID, err)
		}
		if out.Data != tc.says+" read "+want {
			t.Errorf("read for %s answered by the wrong node: %q", tc.nodeID, out.Data)
		}
	}
}

func TestResolver_Locate_CachesPerNodeAndForgetAsksAgain(t *testing.T) {
	node, stateCalls := fakeNode(t, "/w", "n")
	ctl, ctlCalls := fakeController(t, map[string]string{"S1": pidFor(t, node)})
	r := &Resolver{ControllerURL: ctl.URL}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := r.Locate(ctx, "S1"); err != nil {
			t.Fatalf("Locate: %v", err)
		}
	}
	if ctlCalls.Load() != 1 || stateCalls.Load() != 1 {
		t.Fatalf("three Locates asked the controller %d times and the node %d times, want 1 and 1", ctlCalls.Load(), stateCalls.Load())
	}
	r.Forget("S1")
	if _, err := r.Locate(ctx, "S1"); err != nil {
		t.Fatalf("Locate after Forget: %v", err)
	}
	if ctlCalls.Load() != 2 {
		t.Fatalf("Locate after Forget should ask the controller again, calls = %d", ctlCalls.Load())
	}
}

func TestResolver_Locate_UnknownNode(t *testing.T) {
	ctl, _ := fakeController(t, map[string]string{})
	r := &Resolver{ControllerURL: ctl.URL}
	if _, err := r.Locate(context.Background(), "S9"); !errors.Is(err, ErrNodeUnknown) {
		t.Fatalf("a node the controller does not list should be ErrNodeUnknown, got %v", err)
	}
}

func TestResolver_Locate_RefusesWithoutAControllerOrAWorkDir(t *testing.T) {
	if _, err := (&Resolver{}).Locate(context.Background(), "S1"); err == nil {
		t.Fatal("Locate with no ControllerURL should fail, not guess")
	}
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"flags": map[string]string{}})
	}))
	defer silent.Close()
	ctl, _ := fakeController(t, map[string]string{"S1": pidFor(t, silent)})
	if _, err := (&Resolver{ControllerURL: ctl.URL}).Locate(context.Background(), "S1"); err == nil {
		t.Fatal("a node that reports no work_dir should fail, not resolve to an empty path")
	}
}

// A node that moves is found again: an unreachable read drops the cached
// location, and the next Locate takes the address the controller now reports.
func TestResolver_Read_UnreachableDropsTheCachedLocation(t *testing.T) {
	node, _ := fakeNode(t, "/w", "n")
	pids := map[string]string{"S1": "node(1)@127.0.0.1:1"}
	ctl, _ := fakeController(t, pids)
	r := &Resolver{ControllerURL: ctl.URL, Timeout: 50 * time.Millisecond}
	r.nodes = map[string]Node{"S1": {ID: "S1", Host: "127.0.0.1", Port: 1, WorkDir: "/w"}}
	ctx := context.Background()

	stale, _ := r.Locate(ctx, "S1")
	if _, err := r.Read(ctx, stale, "/w/x", 0, -1); !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("read of a dead address: want ErrNodeUnreachable, got %v", err)
	}
	pids["S1"] = pidFor(t, node)
	r.Timeout, r.Client = 0, nil
	fresh, err := r.Locate(ctx, "S1")
	if err != nil {
		t.Fatalf("Locate after the drop: %v", err)
	}
	_, wantPort := splitHostPort(t, node.URL)
	if strconv.Itoa(fresh.Port) != wantPort {
		t.Fatalf("Locate after the drop = port %d, want the node's new port %s", fresh.Port, wantPort)
	}
}

func TestAddressOf(t *testing.T) {
	host, port, err := addressOf("node(1)@10.0.0.7:5152")
	if err != nil || host != "10.0.0.7" || port != 5152 {
		t.Fatalf("addressOf = %q %d %v", host, port, err)
	}
	for _, bad := range []string{"", "node(1)", "node(1)@nohost", "node(1)@h:notaport"} {
		if _, _, err := addressOf(bad); err == nil {
			t.Errorf("addressOf(%q) should fail", bad)
		}
	}
}

// Timeout bounds a call made through a caller-supplied Client too: a node that
// accepts and never answers is given up on at the Resolver's timeout, not held
// for as long as that Client would wait.
func TestResolver_TimeoutHoldsForASuppliedClient(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hung.Close()
	defer close(release)
	host, portStr := splitHostPort(t, hung.URL)
	port, _ := strconv.Atoi(portStr)

	r := &Resolver{Timeout: 100 * time.Millisecond, Client: &http.Client{}} // the Client has no timeout of its own
	start := time.Now()
	_, err := r.Read(context.Background(), Node{ID: "S1", Host: host, Port: port}, "/w/x", 0, -1)
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("a node that never answers: want ErrNodeUnreachable, got %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Read returned after %v; the 100ms Timeout did not bound the supplied Client", took)
	}
}

// A controller that does not answer is not a node that does not answer. The
// two call for different actions from an operator, so Locate names which.
func TestResolver_Locate_UnreachableControllerIsNotAnUnreachableNode(t *testing.T) {
	r := &Resolver{ControllerURL: "http://127.0.0.1:1", Timeout: 50 * time.Millisecond}
	_, err := r.Locate(context.Background(), "S1")
	if !errors.Is(err, ErrControllerUnreachable) {
		t.Fatalf("dead controller: want ErrControllerUnreachable, got %v", err)
	}
	if errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("dead controller must not read as an unreachable node: %v", err)
	}
}

func TestResolver_Locate_UnreachableNodeIsNotAnUnreachableController(t *testing.T) {
	ctrl, _ := fakeController(t, map[string]string{"S1": "node(1)@127.0.0.1:1"})
	r := &Resolver{ControllerURL: ctrl.URL, Timeout: 50 * time.Millisecond}
	_, err := r.Locate(context.Background(), "S1")
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("dead node /state: want ErrNodeUnreachable, got %v", err)
	}
	if errors.Is(err, ErrControllerUnreachable) {
		t.Fatalf("dead node must not read as an unreachable controller: %v", err)
	}
}
