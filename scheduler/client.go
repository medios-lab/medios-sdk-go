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
	"strings"
	"sync"
	"time"

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
// from any goroutine. Subscribe is not: call it at the start, and again only
// after the previous stream has ended.
//
// Controller failover: pass every controller's URL as one comma-separated
// string. Subscribe finds the leader: it follows a standby's 307 redirect,
// and moves to the next URL when a controller is unreachable or has no
// leader yet (HTTP 503). When the stream ends because the leader went away,
// call Subscribe again on the same client; it resubscribes under the
// framework_id it was assigned, so with WithFailoverTimeout the framework's
// tasks keep running.
//
// Defaults baked in (required by the Medios HTTP v1 scheduler API; see
// medios docs/scheduler-http-api.md):
//   - medios.FrameworkInfo.user defaults to os/user.Current()'s Username.
//     Empty value causes silent TASK_LOST on default node config, so it is
//     set defensively.
//   - Medios-Stream-Id header threaded on every non-SUBSCRIBE call.
type Client struct {
	controllerURLs []string // in the order Subscribe tries them
	frameworkInfo  medios.FrameworkInfo
	httpClient     *http.Client
	principal      string   // asserted in X-Medios-Principal on every call when non-empty
	role           string   // FrameworkInfo.role; empty means the default role
	roles          []string // FrameworkInfo.roles; set only through WithRoles
	rolesSet       bool     // WithRoles was given, even with no roles
	optErr         error    // first invalid option value; NewClient returns it

	// handshakeTimeout bounds one SUBSCRIBE attempt, from dialing to the
	// response headers. It does not bound the open stream.
	handshakeTimeout time.Duration

	mu          sync.RWMutex
	frameworkID string // populated from SUBSCRIBED event
	streamID    string // from SUBSCRIBE response Medios-Stream-Id header
	endpoint    string // scheduler URL of the controller the stream is open on
}

const schedulerPath = "/api/v1/scheduler"

// maxRedirects is a parameter: redirects followed from one configured URL. A
// standby points straight at the leader, so one hop is normal; more means
// leadership is moving, and Subscribe moves on to the next URL instead of
// chasing it.
const maxRedirects = 3

// defaultHandshakeTimeout is a default: how long one SUBSCRIBE attempt may
// take from dialing to the response headers before Subscribe moves on to the
// next controller. A controller answers SUBSCRIBE with its headers as soon as
// it has read the call, so a healthy one needs a network round trip; ten
// seconds leaves room for a slow link and has not been measured against a
// loaded controller.
const defaultHandshakeTimeout = 10 * time.Second

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
// controllerURL is the Medios controller, e.g. "http://127.0.0.1:5050", or
// several controllers separated by commas (see Client: controller failover).
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
	var urls []string
	for _, u := range strings.Split(controllerURL, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("scheduler: controllerURL: at least one controller URL is required")
	}
	hostname, _ := os.Hostname()
	c := &Client{
		controllerURLs:   urls,
		handshakeTimeout: defaultHandshakeTimeout,
		frameworkInfo: medios.FrameworkInfo{
			User:     u.Username,
			Name:     frameworkName,
			Hostname: hostname,
		},
		httpClient: &http.Client{
			// No global timeout, SUBSCRIBE is long-lived. The non-stream
			// calls get their context's deadline (or none).
			//
			// Redirects are not followed here: Subscribe follows a standby's
			// 307 itself, so it can bound the hops and record the leader.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
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
// Controllers are tried in the order given. A 307 from a standby is followed
// to the leader; an unreachable controller, one that sends no response
// headers within ten seconds, or a 503 (no leader elected yet) moves on to
// the next URL. Any other status returns a *CallError at once, so
// Rejected reports a 4xx refusal. When no controller accepts, the returned
// error wraps what each one answered; the caller decides when to try again.
//
// Errors that terminate the stream are exposed via the second return
// channel; it sends at most one error and is closed alongside the events
// channel.
func (c *Client) Subscribe(ctx context.Context) (<-chan medios.Event, <-chan error, error) {
	c.mu.RLock()
	call := medios.NewSubscribeCall(c.frameworkInfo)
	c.mu.RUnlock()
	body, err := json.Marshal(call)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: marshal SUBSCRIBE: %w", err)
	}

	var failures []error
	for _, base := range c.controllerURLs {
		endpoint := base + schedulerPath
		resp, err := c.subscribeFollowing(ctx, endpoint, body)
		if err != nil {
			var ce *CallError
			if errors.As(err, &ce) && ce.StatusCode != http.StatusServiceUnavailable {
				return nil, nil, err
			}
			if ctx.Err() != nil {
				return nil, nil, fmt.Errorf("scheduler: SUBSCRIBE request: %w", err)
			}
			failures = append(failures, err)
			continue
		}

		c.mu.Lock()
		c.streamID = resp.Header.Get("Medios-Stream-Id")
		c.endpoint = resp.Request.URL.String()
		c.mu.Unlock()

		events := make(chan medios.Event, 16)
		errs := make(chan error, 1)
		go c.pumpStream(ctx, resp, events, errs)
		return events, errs, nil
	}
	return nil, nil, fmt.Errorf("scheduler: SUBSCRIBE: no controller accepted: %w", errors.Join(failures...))
}

// subscribeFollowing sends SUBSCRIBE to endpoint, following up to
// maxRedirects 307s. It returns the open 200 response, a *CallError for any
// other status, or the transport error, each prefixed with the URL tried.
//
// Each hop gets its own context, cancelled when handshakeTimeout passes
// before the response headers arrive. The caller's context is left alone, so
// Subscribe can still try the next controller. On a 200 the hop's context
// lives until the response body is closed.
func (c *Client) subscribeFollowing(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	for hop := 0; hop <= maxRedirects; hop++ {
		hopCtx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(hopCtx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("scheduler: build SUBSCRIBE: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if c.principal != "" {
			req.Header.Set("X-Medios-Principal", c.principal)
		}

		timer := time.AfterFunc(c.handshakeTimeout, cancel)
		resp, err := c.httpClient.Do(req)
		if timedOut := !timer.Stop(); timedOut && ctx.Err() == nil {
			if err == nil {
				_ = resp.Body.Close()
			}
			return nil, fmt.Errorf("%s: no response headers within %s", endpoint, c.handshakeTimeout)
		}
		if err != nil {
			cancel()
			return nil, fmt.Errorf("%s: %w", endpoint, err)
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if loc := resp.Header.Get("Location"); resp.StatusCode == http.StatusTemporaryRedirect && loc != "" {
			next, err := resp.Request.URL.Parse(loc)
			if err != nil {
				return nil, fmt.Errorf("%s: bad redirect Location %q: %w", endpoint, loc, err)
			}
			endpoint = next.String()
			continue
		}
		// Typed, so a caller can tell a refusal (Rejected) from a failure
		// worth retrying.
		return nil, &CallError{
			Call:       string(medios.CallSubscribe),
			StatusCode: resp.StatusCode,
			Body:       string(bytes.TrimSpace(b)),
		}
	}
	return nil, fmt.Errorf("%s: more than %d redirects", endpoint, maxRedirects)
}

// cancelOnClose releases a response's request context when its body is
// closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
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
				// A later Subscribe is a failover of this framework.
				c.frameworkInfo.ID = &medios.ID{Value: c.frameworkID}
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
	streamID, endpoint := c.streamID, c.endpoint
	c.mu.RUnlock()
	if streamID == "" {
		return errors.New("scheduler: no Medios-Stream-Id (call Subscribe first)")
	}

	body, err := json.Marshal(call)
	if err != nil {
		return fmt.Errorf("scheduler: marshal %s: %w", call.Type, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
