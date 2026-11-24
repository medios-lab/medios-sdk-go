package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
)

// fakeController is a minimal httptest server that records calls and lets
// each test drive the SUBSCRIBE response stream.
type fakeController struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []recordedCall
	streamID string
	server   *httptest.Server

	// subscribeDone is signaled when the test wants to stop the stream.
	subscribeDone chan struct{}

	// events is the queue of events the stream should write.
	eventsToSend chan medios.Event
}

type recordedCall struct {
	headers http.Header
	body    medios.Call
}

func newFakeController(t *testing.T) *fakeController {
	f := &fakeController{
		t:             t,
		streamID:      "stream-test-12345",
		subscribeDone: make(chan struct{}),
		eventsToSend:  make(chan medios.Event, 16),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/scheduler", f.handleScheduler)
	f.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(f.subscribeDone)
		f.server.Close()
	})
	return f
}

func (f *fakeController) URL() string { return f.server.URL }

func (f *fakeController) handleScheduler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var call medios.Call
	if err := json.Unmarshal(body, &call); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{headers: r.Header.Clone(), body: call})
	f.mu.Unlock()

	if call.Type == medios.CallSubscribe {
		f.handleSubscribe(w, r)
		return
	}
	// All other calls: 202 Accepted with empty body.
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeController) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Medios-Stream-Id", f.streamID)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// Flush headers immediately so the client's Do() returns and pumpStream
	// can begin reading. Without this, Go's response buffer holds the headers
	// until the first body write, and the client side blocks on Do().
	if flusher != nil {
		flusher.Flush()
	}

	for {
		select {
		case ev := <-f.eventsToSend:
			payload, _ := json.Marshal(ev)
			_, _ = io.WriteString(w, strconv.Itoa(len(payload))+"\n")
			_, _ = w.Write(payload)
			if flusher != nil {
				flusher.Flush()
			}
		case <-f.subscribeDone:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// sendEvent queues an event for the SUBSCRIBE stream to emit.
func (f *fakeController) sendEvent(ev medios.Event) { f.eventsToSend <- ev }

// recordedCalls returns a snapshot of recorded calls.
func (f *fakeController) recordedCalls() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// =============================================================================

func TestClient_SubscribeReceivesSubscribedAndCapturesFrameworkID(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fc.sendEvent(medios.Event{
		Type:       medios.EventSubscribed,
		Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "fw-001"}, HeartbeatIntervalSeconds: 15.0},
	})

	select {
	case ev := <-events:
		if ev.Type != medios.EventSubscribed {
			t.Fatalf("first event: want SUBSCRIBED, got %s", ev.Type)
		}
	case err := <-errs:
		t.Fatalf("error before event: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SUBSCRIBED")
	}

	if got := client.FrameworkID(); got != "fw-001" {
		t.Errorf("FrameworkID: want fw-001, got %q", got)
	}
}

func TestClient_SubscribeSendsFrameworkInfoWithCurrentUser(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err = client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Give the request a beat to land.
	time.Sleep(50 * time.Millisecond)

	calls := fc.recordedCalls()
	if len(calls) == 0 {
		t.Fatal("no calls recorded")
	}
	c := calls[0]
	if c.body.Type != medios.CallSubscribe || c.body.Subscribe == nil {
		t.Fatalf("first call should be SUBSCRIBE, got %+v", c.body)
	}
	if c.body.Subscribe.FrameworkInfo.User == "" {
		t.Error("medios.FrameworkInfo.user should be auto-populated, was empty")
	}
	if c.body.Subscribe.FrameworkInfo.Name != "test-fw" {
		t.Errorf("medios.FrameworkInfo.name: want test-fw, got %q",
			c.body.Subscribe.FrameworkInfo.Name)
	}
}

func TestClient_AcceptThreadsStreamIDHeader(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, _, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fc.sendEvent(medios.Event{Type: medios.EventSubscribed, Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "fw-001"}}})
	<-events // drain SUBSCRIBED so FrameworkID is set

	task := medios.TaskInfo{
		Name:    "hello",
		TaskID:  medios.ID{Value: "t-1"},
		NodeID:  medios.ID{Value: "S0"},
		Command: medios.CommandInfo{Shell: true, Value: "echo hi"},
	}
	if err := client.Accept(ctx, []medios.ID{{Value: "O-1"}}, []medios.TaskInfo{task}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	calls := fc.recordedCalls()
	// last call is the ACCEPT
	last := calls[len(calls)-1]
	if last.body.Type != medios.CallAccept {
		t.Fatalf("last call: want ACCEPT, got %s", last.body.Type)
	}
	if got := last.headers.Get("Medios-Stream-Id"); got != fc.streamID {
		t.Errorf("Medios-Stream-Id: want %q, got %q", fc.streamID, got)
	}
	if last.body.FrameworkID == nil || last.body.FrameworkID.Value != "fw-001" {
		t.Errorf("framework_id: %+v", last.body.FrameworkID)
	}
}

func TestClient_AcceptBeforeSubscribeFails(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = client.Accept(context.Background(), []medios.ID{{Value: "O-1"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "Stream-Id") {
		t.Fatalf("want Stream-Id error, got %v", err)
	}
}

func TestClient_DeclineKillAcknowledgeTeardown_SendCorrectCallTypes(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, _, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fc.sendEvent(medios.Event{Type: medios.EventSubscribed, Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "fw-001"}}})
	<-events

	if err := client.Decline(ctx, []medios.ID{{Value: "O-2"}}, 120); err != nil {
		t.Errorf("Decline: %v", err)
	}
	if err := client.Suppress(ctx); err != nil {
		t.Errorf("Suppress: %v", err)
	}
	if err := client.Revive(ctx); err != nil {
		t.Errorf("Revive: %v", err)
	}
	if err := client.Kill(ctx, "t-1", "S0"); err != nil {
		t.Errorf("Kill: %v", err)
	}
	if err := client.Acknowledge(ctx, "t-1", "S0", "uuid-abc"); err != nil {
		t.Errorf("Acknowledge: %v", err)
	}
	if err := client.Teardown(ctx); err != nil {
		t.Errorf("Teardown: %v", err)
	}

	calls := fc.recordedCalls()
	// SUBSCRIBE + 6 follow-ups
	wantTypes := []string{medios.CallSubscribe, medios.CallDecline, medios.CallSuppress, medios.CallRevive, medios.CallKill, medios.CallAcknowledge, medios.CallTeardown}
	if len(calls) != len(wantTypes) {
		t.Fatalf("call count: want %d, got %d", len(wantTypes), len(calls))
	}
	for i, want := range wantTypes {
		if calls[i].body.Type != want {
			t.Errorf("calls[%d].type: want %s, got %s", i, want, calls[i].body.Type)
		}
	}
}

func TestClient_SubscribeNonOKBubblesUp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "go away", http.StatusForbidden)
	}))
	defer ts.Close()

	client, err := NewClient(ts.URL, "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, err = client.Subscribe(context.Background())
	if err == nil {
		t.Fatal("Subscribe: want error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention 403: %v", err)
	}
}

func TestClient_StreamCloseDeliversIO(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Two events then close the subscription.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errs, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fc.sendEvent(medios.Event{Type: medios.EventSubscribed, Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "fw-001"}}})
	fc.sendEvent(medios.Event{Type: medios.EventHeartbeat})

	var got []string
	for i := 0; i < 2; i++ {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events closed early")
			}
			got = append(got, ev.Type)
		case err := <-errs:
			t.Fatalf("error before drain: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}
	want := []string{medios.EventSubscribed, medios.EventHeartbeat}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events[%d]: want %s, got %s", i, want[i], got[i])
		}
	}

	// Cancel the context, the stream pump should close the channels.
	cancel()
	// Drain events until close. Allow up to 2s.
	closed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-events:
			if !ok {
				closed = true
			}
		case <-time.After(50 * time.Millisecond):
		}
		if closed {
			break
		}
	}
	if !closed {
		t.Error("events channel should close after ctx cancel")
	}

	// errs may have an unrelated wrap of context.Canceled, but we filter
	// that in pumpStream, so errs should be drained empty.
	select {
	case err, ok := <-errs:
		if ok && err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("unexpected error on close: %v", err)
		}
	default:
	}
}

func TestClient_WithFrameworkID_Prepopulates(t *testing.T) {
	c, err := NewClient("http://x:5150", "fw", WithFrameworkID("fw-prev"), WithFailoverTimeout(60))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.FrameworkID() != "fw-prev" {
		t.Errorf("WithFrameworkID should pre-populate FrameworkID(); got %q", c.FrameworkID())
	}
}

func TestClient_WithPrincipal_ThreadsHeaderOnSubscribeAndAccept(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw", WithPrincipal("fennel-dev"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, _, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fc.sendEvent(medios.Event{Type: medios.EventSubscribed, Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "fw-001"}}})
	<-events

	if err := client.Accept(ctx, []medios.ID{{Value: "O-1"}}, nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	calls := fc.recordedCalls()
	if len(calls) < 2 {
		t.Fatalf("expected at least SUBSCRIBE + ACCEPT, got %d calls", len(calls))
	}
	for i, c := range calls {
		if got := c.headers.Get("X-Medios-Principal"); got != "fennel-dev" {
			t.Errorf("call %d (%s): X-Medios-Principal want %q, got %q", i, c.body.Type, "fennel-dev", got)
		}
	}
	if got := calls[0].body.Subscribe.FrameworkInfo.Principal; got != "fennel-dev" {
		t.Errorf("SUBSCRIBE framework_info.principal: want %q, got %q", "fennel-dev", got)
	}
}

func TestClient_NoPrincipal_HeaderAbsent(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := client.Subscribe(ctx); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	calls := fc.recordedCalls()
	if len(calls) == 0 {
		t.Fatalf("no calls recorded")
	}
	if got := calls[0].headers.Get("X-Medios-Principal"); got != "" {
		t.Errorf("X-Medios-Principal should be absent when principal unset; got %q", got)
	}
}

func TestWithCapabilities(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:5050", "fw",
		WithCapabilities(medios.CapabilityGPUResources))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got := c.frameworkInfo.Capabilities
	if len(got) != 1 || got[0].Type != "GPU_RESOURCES" {
		t.Fatalf("capabilities = %+v, want one GPU_RESOURCES", got)
	}
}

// Repeated calls replace rather than append, so a rebuilt client cannot
// accumulate duplicates across re-subscribes.
func TestWithCapabilities_Replaces(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:5050", "fw",
		WithCapabilities("A", "B"), WithCapabilities(medios.CapabilityGPUResources))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.frameworkInfo.Capabilities; len(got) != 1 || got[0].Type != "GPU_RESOURCES" {
		t.Fatalf("capabilities = %+v, want only GPU_RESOURCES", got)
	}
}

func TestWithCapabilities_NoneOmits(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:5050", "fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.frameworkInfo.Capabilities; len(got) != 0 {
		t.Fatalf("default client should declare no capabilities, got %+v", got)
	}
}

func TestClient_DefaultAndOverrideHostname(t *testing.T) {
	c1, err := NewClient("http://127.0.0.1:5050", "fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c1.frameworkInfo.Hostname == "" {
		t.Errorf("expected default framework_info.hostname from os.Hostname(), got empty")
	}

	c2, err := NewClient("http://127.0.0.1:5050", "fw", WithHostname("scheduler.example.internal"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c2.frameworkInfo.Hostname; got != "scheduler.example.internal" {
		t.Errorf("WithHostname: want %q, got %q", "scheduler.example.internal", got)
	}
}

func TestClient_RequestSendsRequestCall(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, _, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fc.sendEvent(medios.Event{
		Type:       medios.EventSubscribed,
		Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "fw-001"}},
	})
	<-events

	nodeID := medios.ID{Value: "S0"}
	reqs := []medios.Request{{
		NodeID: &nodeID,
		Resources: []medios.Resource{
			{Name: "cpus", Type: "SCALAR", Scalar: &medios.Scalar{Value: 2}},
		},
	}}
	if err := client.Request(ctx, reqs); err != nil {
		t.Fatalf("Request: %v", err)
	}

	calls := fc.recordedCalls()
	last := calls[len(calls)-1]
	if last.body.Type != medios.CallRequest || last.body.Request == nil {
		t.Fatalf("last call: want REQUEST with body, got %+v", last.body)
	}
	if len(last.body.Request.Requests) != 1 ||
		last.body.Request.Requests[0].NodeID == nil ||
		last.body.Request.Requests[0].NodeID.Value != "S0" {
		t.Fatalf("request body = %+v", last.body.Request)
	}
}
