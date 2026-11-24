// Package sandbox reads task stdout/stderr from a medios-node's /files/read
// HTTP endpoint.
//
// A framework launches a task with the default command executor; the task's
// stdout/stderr land in the node's sandbox. This package turns
// (node_id, framework_id, task_id, container_id, file) into the node-side
// path and reads the bytes back through the node's HTTP API. Status updates
// carry a task's lifecycle state; its output is read here.
//
// Sandbox path layout:
//
//	<work_dir>/nodes/<node_id>/frameworks/<framework_id>/executors/<executor_id>/runs/<container_id>/<file>
//
// For default-command-executor tasks, executor_id == task_id. The response
// is {data, offset} from the node's /files/read route.
//
// Where a node listens and where its sandboxes live are facts about that
// node, not about the cluster: one host can run several nodes, each on its
// own port with its own --work_dir. The Resolver learns both per node id. The
// controller's /nodes names the node's address; the node's own /state names
// its work_dir.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when the medios-node reports the sandbox path
// doesn't exist (post-GC, or the task never ran on this node).
var ErrNotFound = errors.New("sandbox: file not found on node")

// ErrNodeUnreachable is returned when the node cannot be reached within the
// resolver's timeout.
var ErrNodeUnreachable = errors.New("sandbox: node unreachable")

// ErrControllerUnreachable is returned when the controller cannot be reached
// within the resolver's timeout. It says nothing about any node.
var ErrControllerUnreachable = errors.New("sandbox: controller unreachable")

// ErrNodeUnknown is returned when the controller does not list the node id.
// The node has been removed, or the id belongs to another cluster.
var ErrNodeUnknown = errors.New("sandbox: node not known to the controller")

// DefaultTimeout caps each HTTP call the Resolver makes when Timeout is zero.
// A default, not tuned: long enough for a node on the same network to answer
// /files/read, short enough that an API request proxying it does not hang.
const DefaultTimeout = 5 * time.Second

// Node is where one medios-node serves its HTTP API and keeps its sandboxes.
type Node struct {
	ID      string
	Host    string // from the node's pid as the controller reports it
	Port    int    // likewise
	WorkDir string // the node's --work_dir, from its own /state
}

// Resolver locates nodes and reads sandbox files through their HTTP API.
// Construct one per framework process and share it; it is safe for
// concurrent use.
type Resolver struct {
	// ControllerURL is the controller's base URL, e.g. "http://host:5150".
	// Required: node ids are resolved against its /nodes.
	ControllerURL string
	// Timeout caps each HTTP call, whichever Client makes it: it is applied
	// to the call's context. Zero means DefaultTimeout.
	Timeout time.Duration
	// Client is the HTTP client used for every call. Defaults to a plain
	// &http.Client{}; Timeout bounds the call either way.
	Client *http.Client

	mu    sync.Mutex
	nodes map[string]Node
}

// ReadResult mirrors the medios-node's /files/read response shape
// (object keys "data"/"offset", per src/files/files.cpp).
type ReadResult struct {
	Data   string `json:"data"`
	Offset int64  `json:"offset"`
}

func (r *Resolver) client() *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Client == nil {
		r.Client = &http.Client{}
	}
	return r.Client
}

// bounded returns ctx with the Resolver's timeout applied, so the cap holds
// for a caller-supplied Client as well as the default one.
func (r *Resolver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// Locate returns where the node serves its API and keeps its sandboxes. The
// answer is cached per node id: a node id is bound to one work_dir for its
// life, since a node started on a different work_dir registers as a new node.
// Forget drops an entry, and Read drops it on its own when the node cannot be
// reached, so an address that has changed is looked up again.
//
// Errors: ErrControllerUnreachable when the controller's /nodes cannot be
// reached; ErrNodeUnknown when the controller does not list the id;
// ErrNodeUnreachable when the node's /state cannot be reached.
func (r *Resolver) Locate(ctx context.Context, nodeID string) (Node, error) {
	r.mu.Lock()
	n, ok := r.nodes[nodeID]
	r.mu.Unlock()
	if ok {
		return n, nil
	}
	if r.ControllerURL == "" {
		return Node{}, errors.New("sandbox: Resolver.ControllerURL is not set")
	}

	var listing struct {
		Nodes []struct {
			ID  string `json:"id"`
			PID string `json:"pid"`
		} `json:"nodes"`
	}
	if err := r.getJSON(ctx, strings.TrimRight(r.ControllerURL, "/")+"/nodes", ErrControllerUnreachable, &listing); err != nil {
		return Node{}, err
	}
	pid := ""
	for _, c := range listing.Nodes {
		if c.ID == nodeID {
			pid = c.PID
			break
		}
	}
	if pid == "" {
		return Node{}, fmt.Errorf("%w: %s", ErrNodeUnknown, nodeID)
	}
	host, port, err := addressOf(pid)
	if err != nil {
		return Node{}, fmt.Errorf("sandbox: node %s: %w", nodeID, err)
	}

	var state struct {
		Flags struct {
			WorkDir string `json:"work_dir"`
		} `json:"flags"`
	}
	if err := r.getJSON(ctx, fmt.Sprintf("http://%s/state", net.JoinHostPort(host, strconv.Itoa(port))), ErrNodeUnreachable, &state); err != nil {
		return Node{}, err
	}
	if state.Flags.WorkDir == "" {
		return Node{}, fmt.Errorf("sandbox: node %s at %s:%d reports no flags.work_dir", nodeID, host, port)
	}

	n = Node{ID: nodeID, Host: host, Port: port, WorkDir: state.Flags.WorkDir}
	r.mu.Lock()
	if r.nodes == nil {
		r.nodes = make(map[string]Node)
	}
	r.nodes[nodeID] = n
	r.mu.Unlock()
	return n, nil
}

// Forget drops the cached location of a node, so the next Locate asks again.
func (r *Resolver) Forget(nodeID string) {
	r.mu.Lock()
	delete(r.nodes, nodeID)
	r.mu.Unlock()
}

// addressOf splits a node pid, "node(1)@10.0.0.7:5152", into host and port.
func addressOf(pid string) (string, int, error) {
	_, addr, ok := strings.Cut(pid, "@")
	if !ok {
		return "", 0, fmt.Errorf("pid %q has no address", pid)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("pid %q: %w", pid, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("pid %q: port: %w", pid, err)
	}
	return host, port, nil
}

// getJSON wraps a transport failure in unreachable, which names whose
// endpoint rawURL is.
func (r *Resolver) getJSON(ctx context.Context, rawURL string, unreachable error, into any) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("sandbox: new request: %w", err)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", unreachable, req.URL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox: %s returned %d", rawURL, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("sandbox: decode %s: %w", rawURL, err)
	}
	return nil
}

// PathFor returns the node-side absolute path of a file inside the task's
// sandbox, the value sent as the ?path= query parameter to /files/read.
//
// frameworkID is passed per call because the controller assigns it at
// SUBSCRIBE time, after the Resolver is constructed.
func (n Node) PathFor(frameworkID, taskID, containerID, file string) string {
	return path.Join(n.WorkDir, "nodes", n.ID, "frameworks", frameworkID,
		"executors", taskID, "runs", containerID, file)
}

// Read fetches a slice of a sandbox file from the node. offset=-1 means
// "from current end"; length=-1 means "read all remaining" (length omitted).
//
// Returns the bytes (as a string per the wire shape), the next offset, or:
//   - ErrNotFound: file doesn't exist on the node
//   - ErrNodeUnreachable: transport error / timeout; the node's cached
//     location is dropped, so the next Locate asks the controller again
//   - other errors: non-200, malformed response
//
// Known limit: the cached location is dropped on a transport error only. If
// the node's address is later served by a different node, that node answers
// 404 and the caller sees ErrNotFound, which is also the true answer once a
// sandbox has been garbage-collected. The two cannot be told apart from the
// response, so a 404 does not invalidate the cache; Forget does.
func (r *Resolver) Read(ctx context.Context, n Node, sandboxPath string, offset, length int64) (*ReadResult, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(n.Host, strconv.Itoa(n.Port)),
		Path:   "/files/read",
	}
	q := u.Query()
	q.Set("path", sandboxPath)
	q.Set("offset", strconv.FormatInt(offset, 10))
	if length >= 0 {
		q.Set("length", strconv.FormatInt(length, 10))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: new request: %w", err)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		r.Forget(n.ID)
		return nil, fmt.Errorf("%w: %s: %v", ErrNodeUnreachable, u.Host, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out ReadResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("sandbox: decode response: %w", err)
		}
		return &out, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("sandbox: node returned %d for %s", resp.StatusCode, sandboxPath)
	}
}
