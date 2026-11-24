package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"sync"

	"github.com/medios-lab/medios-sdk-go/medios"
)

// Client is a single-framework Medios HTTP v1 scheduler client.
//
// Lifecycle:
//
//  1. Construct with NewClient(controllerURL, frameworkName).
//  2. Subscribe(ctx) → returns a stream of Events. The first event is
//     SUBSCRIBED, which carries the framework_id; Client captures it
//     internally so subsequent calls can use it. The stream then continues
//     with OFFERS / UPDATE / HEARTBEAT / RESCIND / ERROR until the context
//     is cancelled or the connection breaks.
//  3. medios.Call Accept / Decline / Kill / Acknowledge in response to events.
//  4. medios.Call Teardown when finished.
//
// Concurrency: Accept / Decline / Kill / Acknowledge / Teardown are safe
// from any goroutine. Subscribe is not, call it once at the start.
//
// Defaults baked in (required by the Medios HTTP v1 scheduler API; see
// medios docs/scheduler-http-api.md):
//   - medios.FrameworkInfo.user defaults to os/user.Current()'s Username.
//     Empty value causes silent TASK_LOST on default node config, so it is
//     set defensively.
//   - Medios-Stream-Id header threaded on every non-SUBSCRIBE call.
type Client struct {
	controllerURL string // e.g. "http://127.0.0.1:5050"
	frameworkInfo medios.FrameworkInfo
	httpClient    *http.Client
	principal     string   // asserted in X-Medios-Principal on every call when non-empty
	role          string   // FrameworkInfo.role; empty means the default role
	roles         []string // FrameworkInfo.roles; set only through WithRoles
	rolesSet      bool     // WithRoles was given, even with no roles
	optErr        error    // first invalid option value; NewClient returns it

	mu          sync.RWMutex
	frameworkID string // populated from SUBSCRIBED event
	streamID    string // from SUBSCRIBE response Medios-Stream-Id header
}

// ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithUser overrides the default os user used in medios.FrameworkInfo.user.
func WithUser(u string) ClientOption {
	return func(c *Client) { c.frameworkInfo.User = u }
}

// WithCapabilities declares framework capabilities in FrameworkInfo, e.g.
// medios.CapabilityGPUResources. Repeated calls replace, they do not append.
//
// Declare a capability before the cluster starts producing what it gates.
// GPU_RESOURCES is the sharp case: a framework without it stops receiving
// offers from a node entirely the moment that node advertises a GPU, so
// non-GPU tasks placed there strand on their next relaunch.
func WithCapabilities(types ...string) ClientOption {
	return func(c *Client) {
		caps := make([]medios.Capability, 0, len(types))
		for _, t := range types {
			caps = append(caps, medios.Capability{Type: t})
		}
		c.frameworkInfo.Capabilities = caps
	}
}

// WithHTTPClient overrides the default *http.Client. Useful in tests.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.httpClient = h }
}

// WithFrameworkID re-subscribes under a framework_id captured from a previous
// run (via FrameworkID()), so the controller treats this as the same framework
// (failover) instead of a new one — the basis for restart survival. The
// captured id is also pre-populated so calls can be made before the SUBSCRIBED
// reply arrives. Pair with WithFailoverTimeout, or the controller kills the
// framework's tasks the instant it disconnects.
func WithFrameworkID(id string) ClientOption {
	return func(c *Client) {
		c.frameworkInfo.ID = &medios.ID{Value: id}
		c.frameworkID = id
	}
}

// WithFailoverTimeout sets framework_info.failover_timeout (seconds): how long
// the controller keeps this framework's tasks alive after it disconnects,
// waiting for it to re-subscribe. 0 (default) = tasks die on disconnect.
func WithFailoverTimeout(seconds float64) ClientOption {
	return func(c *Client) { c.frameworkInfo.FailoverTimeout = seconds }
}

// WithCheckpoint sets framework_info.checkpoint: each node records this
// framework's executors on disk, and a restarted node recovers its tasks
// instead of destroying them. Unset, a node restart (a config change, a
// release roll) kills every task the framework has on that node.
func WithCheckpoint() ClientOption {
	return func(c *Client) { c.frameworkInfo.Checkpoint = true }
}

// WithPrincipal sets the framework's identity in both FrameworkInfo.Principal
// in the SUBSCRIBE body (required by the controller on unauthenticated
// connections per medios#537) and the X-Medios-Principal header on every
// scheduler call (SUBSCRIBE and non-SUBSCRIBE alike).
//
// The value must match the framework identity the controller expects (for
// example "framework:my-framework"). When unset, FrameworkInfo.Principal is
// omitted and no X-Medios-Principal header is sent (which works only when
// HTTP framework authentication is enabled on the controller so the
// controller stamps the authenticated principal onto FrameworkInfo).
//
// See medios src/common/http.cpp (trusted-header authenticator) and
// medios#537 for the SUBSCRIBE principal requirement.
func WithPrincipal(principal string) ClientOption {
	return func(c *Client) {
		c.principal = principal
		c.frameworkInfo.Principal = principal
	}
}

// WithHostname overrides the default host name reported in
// medios.FrameworkInfo.hostname at SUBSCRIBE (medios#409).
func WithHostname(hostname string) ClientOption {
	return func(c *Client) { c.frameworkInfo.Hostname = hostname }
}

// WithRole sets the allocation role the framework subscribes under, reported
// as FrameworkInfo.role.
//
// Unset means the default role and is right for a single-tenant cluster. Set
// it when the framework should receive resources reserved for a role: the
// controller offers reserved supply only to a framework subscribed under that
// role, so without it reserved resources are never offered and any handling
// of them is unreachable.
func WithRole(role string) ClientOption {
	return func(c *Client) { c.role = role }
}

// WithRoles subscribes under several allocation roles, in the given order,
// and declares medios.CapabilityMultiRole. Include "*" to keep receiving
// unreserved supply. NewClient refuses an empty list, an empty or repeated
// role, and WithRoles combined with WithRole.
func WithRoles(roles ...string) ClientOption {
	return func(c *Client) {
		c.roles = append([]string(nil), roles...)
		c.rolesSet = true
	}
}

// WithWebUIURL sets FrameworkInfo.webui_url, the base URL clients use to reach
// this framework's API. The controller lists it per framework in /state, so
// it is how a client that knows only the controller address finds the
// framework.
//
// The value must be an http or https URL with a host that is not a wildcard
// bind address (0.0.0.0, ::); NewClient returns an error naming the problem
// otherwise. There is no default and no guess from the hostname: omit the
// option and webui_url stays empty.
func WithWebUIURL(rawURL string) ClientOption {
	return func(c *Client) {
		if err := validateWebUIURL(rawURL); err != nil {
			if c.optErr == nil {
				c.optErr = err
			}
			return
		}
		c.frameworkInfo.WebUIURL = rawURL
	}
}

func validateWebUIURL(rawURL string) error {
	if rawURL == "" {
		return errors.New("scheduler: webui_url is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("scheduler: webui_url %q is not a URL: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheduler: webui_url %q must use http or https", rawURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("scheduler: webui_url %q has no host", rawURL)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("scheduler: webui_url %q has a wildcard host; give the address clients use", rawURL)
	}
	return nil
}

// NewClient builds a scheduler client.
//
// controllerURL is the Medios controller, e.g. "http://127.0.0.1:5050".
// frameworkName is the human-readable name reported in medios.FrameworkInfo.
//
// The framework's OS user defaults to the current user (required by the
// Medios HTTP v1 scheduler API; empty string causes TASK_LOST on default
// node config). Override with WithUser if running under a different account
// than os/user reports.
func NewClient(controllerURL, frameworkName string, opts ...ClientOption) (*Client, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("scheduler: cannot resolve current user: %w", err)
	}
	hostname, _ := os.Hostname()
	c := &Client{
		controllerURL: controllerURL,
		frameworkInfo: medios.FrameworkInfo{
			User:     u.Username,
			Name:     frameworkName,
			Hostname: hostname,
		},
		httpClient: &http.Client{
			// No global timeout, SUBSCRIBE is long-lived. The non-stream
			// calls get their context's deadline (or none).
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.optErr != nil {
		return nil, c.optErr
	}
	// Applied after the options so WithRole and WithRoles reach the wire
	// struct the SUBSCRIBE body is built from, rather than only the client
	// fields.
	if !c.rolesSet {
		c.frameworkInfo.Role = c.role
		return c, nil
	}
	if err := checkRoles(c.role, c.roles); err != nil {
		return nil, fmt.Errorf("scheduler: %w", err)
	}
	c.frameworkInfo.Roles = c.roles
	declared := false
	for _, cp := range c.frameworkInfo.Capabilities {
		declared = declared || cp.Type == medios.CapabilityMultiRole
	}
	if !declared {
		c.frameworkInfo.Capabilities = append(c.frameworkInfo.Capabilities,
			medios.Capability{Type: medios.CapabilityMultiRole})
	}
	return c, nil
}

// checkRoles refuses the role options the controller would refuse, or that
// could only be a caller mistake. Role names themselves are the controller's
// to validate.
func checkRoles(role string, roles []string) error {
	if role != "" {
		return fmt.Errorf("WithRole and WithRoles are mutually exclusive")
	}
	if len(roles) == 0 {
		return fmt.Errorf("WithRoles needs at least one role")
	}
	seen := make(map[string]bool, len(roles))
	for _, r := range roles {
		if r == "" {
			return fmt.Errorf("WithRoles: empty role")
		}
		if seen[r] {
			return fmt.Errorf("WithRoles: role %q given twice", r)
		}
		seen[r] = true
	}
	return nil
}

// FrameworkID returns the framework_id assigned by the controller in the
// SUBSCRIBED event. Returns "" if no SUBSCRIBED event has been seen yet.
func (c *Client) FrameworkID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.frameworkID
}

// Subscribe opens the SUBSCRIBE call and returns a channel of Events.
// The channel is closed when the stream ends (server hangup or ctx done).
//
// The first medios.Event will be SUBSCRIBED, Client captures the framework_id
// from it before delivering, so a consumer reading the channel can issue
// follow-up calls (Accept, Decline) immediately when OFFERS arrives.
//
// A SUBSCRIBE the controller answers with any status other than 200 returns a
// *CallError, so Rejected reports a 4xx refusal.
//
// Errors that terminate the stream are exposed via the second return
// channel; it sends at most one error and is closed alongside the events
// channel.
func (c *Client) Subscribe(ctx context.Context) (<-chan medios.Event, <-chan error, error) {
	call := medios.NewSubscribeCall(c.frameworkInfo)
	body, err := json.Marshal(call)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: marshal SUBSCRIBE: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.controllerURL+"/api/v1/scheduler", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: build SUBSCRIBE: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.principal != "" {
		req.Header.Set("X-Medios-Principal", c.principal)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: SUBSCRIBE request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		// Typed, so a caller can tell a refusal (Rejected) from a failure
		// worth retrying.
		return nil, nil, &CallError{
			Call:       string(medios.CallSubscribe),
			StatusCode: resp.StatusCode,
			Body:       string(bytes.TrimSpace(b)),
		}
	}

	c.mu.Lock()
	c.streamID = resp.Header.Get("Medios-Stream-Id")
	c.mu.Unlock()

	events := make(chan medios.Event, 16)
	errs := make(chan error, 1)
	go c.pumpStream(ctx, resp, events, errs)
	return events, errs, nil
}

// pumpStream reads RecordIO records from resp.Body, decodes each as an
// medios.Event, captures framework_id from SUBSCRIBED, and forwards to events.
// Closes events + errs when done.
func (c *Client) pumpStream(ctx context.Context, resp *http.Response, events chan<- medios.Event, errs chan<- error) {
	defer close(events)
	defer close(errs)
	defer resp.Body.Close()

	err := medios.ParseRecordIOJSON(
		resp.Body,
		func() *medios.Event { return &medios.Event{} },
		func(ev *medios.Event) error {
			if ev.Type == medios.EventSubscribed && ev.Subscribed != nil {
				c.mu.Lock()
				c.frameworkID = ev.Subscribed.FrameworkID.Value
				c.mu.Unlock()
			}
			select {
			case events <- *ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		errs <- err
	}
}

// Accept sends an ACCEPT call with a single LAUNCH operation over offerIDs
// and tasks. Per medios HTTP v1, all offers in a single ACCEPT must come
// from the same node (the controller enforces this).
//
// Every task's volumes are checked before anything is sent: a volume whose
// source is not a store is refused here with the same sentence the
// controller would use, so a host path never leaves the framework.
func (c *Client) Accept(ctx context.Context, offerIDs []medios.ID, tasks []medios.TaskInfo) error {
	for _, t := range tasks {
		if err := medios.ValidateVolumes(t); err != nil {
			return fmt.Errorf("task %s: %w", t.TaskID.Value, err)
		}
	}
	return c.post(ctx, medios.NewAcceptCall(c.FrameworkID(), offerIDs, tasks))
}

// Decline an offer (or several). refuseSeconds attaches a Filters.refuse_seconds
// so the controller waits that long before re-offering these resources; pass 0
// to use the controller's default (5s). Always decline offers you do not
// ACCEPT — an offer that is neither accepted nor declined stays outstanding to
// this framework (the offer-hoarding failure mode).
func (c *Client) Decline(ctx context.Context, offerIDs []medios.ID, refuseSeconds float64) error {
	return c.post(ctx, medios.NewDeclineCall(c.FrameworkID(), offerIDs, refuseSeconds))
}

// Suppress tells the controller to stop sending offers to this framework. Use
// when there is nothing to launch; pair with Revive when work appears.
func (c *Client) Suppress(ctx context.Context) error {
	return c.post(ctx, medios.NewSuppressCall(c.FrameworkID()))
}

// Revive clears any active decline/accept filters and resumes offers. Use when
// new work appears after a Suppress.
func (c *Client) Revive(ctx context.Context) error {
	return c.post(ctx, medios.NewReviveCall(c.FrameworkID()))
}

// Kill asks the controller to kill the given task on the given node.
func (c *Client) Kill(ctx context.Context, taskID, nodeID string) error {
	return c.post(ctx, medios.NewKillCall(c.FrameworkID(), taskID, nodeID))
}

// Acknowledge a status update. Required for updates that carry a UUID
// (the node retransmits unacknowledged updates).
func (c *Client) Acknowledge(ctx context.Context, taskID, nodeID, uuid string) error {
	return c.post(ctx, medios.NewAcknowledgeCall(c.FrameworkID(), taskID, nodeID, uuid))
}

// Reconcile asks the controller to resend the current status of the given
// tasks; pass nil to reconcile all of this framework's tasks. The replies
// arrive as UPDATE events on the subscription stream. This is how a framework
// re-learns task state after a restart (restart survival).
func (c *Client) Reconcile(ctx context.Context, tasks []medios.ReconcileTask) error {
	return c.post(ctx, medios.NewReconcileCall(c.FrameworkID(), tasks))
}

// Request sends a REQUEST call to the controller's allocator for the given
// per-node resource shapes.
func (c *Client) Request(ctx context.Context, requests []medios.Request) error {
	return c.post(ctx, medios.NewRequestCall(c.FrameworkID(), requests))
}

// Teardown removes the framework from the controller.
// After Teardown, the SUBSCRIBE stream will close.
func (c *Client) Teardown(ctx context.Context) error {
	return c.post(ctx, medios.NewTeardownCall(c.FrameworkID()))
}

// post is the shared call path for non-SUBSCRIBE calls.
// Threads the Medios-Stream-Id header (required on every non-SUBSCRIBE call
// by the Medios HTTP v1 scheduler API) and reports any non-2xx with the body.
func (c *Client) post(ctx context.Context, call medios.Call) error {
	c.mu.RLock()
	streamID := c.streamID
	c.mu.RUnlock()
	if streamID == "" {
		return errors.New("scheduler: no Medios-Stream-Id (call Subscribe first)")
	}

	body, err := json.Marshal(call)
	if err != nil {
		return fmt.Errorf("scheduler: marshal %s: %w", call.Type, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.controllerURL+"/api/v1/scheduler", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("scheduler: build %s: %w", call.Type, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Medios-Stream-Id", streamID)
	if c.principal != "" {
		req.Header.Set("X-Medios-Principal", c.principal)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("scheduler: %s: %w", call.Type, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		// Typed, because the status is the only thing that says whether the
		// call took effect. See CallError and Rejected.
		return &CallError{
			Call:       string(call.Type),
			StatusCode: resp.StatusCode,
			Body:       string(bytes.TrimSpace(b)),
		}
	}
	return nil
}
