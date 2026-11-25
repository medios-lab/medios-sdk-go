package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
)

// newStandby is a standby controller: it answers every POST with a 307 to
// location, or with 503 when location is empty.
func newStandby(t *testing.T, location string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(s.Close)
	return s
}

func subscribed(t *testing.T, fc *fakeController, events <-chan medios.Event, id string) {
	t.Helper()
	fc.sendEvent(medios.Event{
		Type:       medios.EventSubscribed,
		Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: id}},
	})
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SUBSCRIBED")
	}
}

func callTypes(fc *fakeController) []string {
	var out []string
	for _, c := range fc.recordedCalls() {
		out = append(out, string(c.body.Type))
	}
	return out
}

func TestClient_SubscribeFollowsStandbyRedirectAndPostsToLeader(t *testing.T) {
	fc := newFakeController(t)
	// Scheme-relative, as the controller sends it.
	standby := newStandby(t, "//"+strings.TrimPrefix(fc.URL(), "http://")+schedulerPath)
	client, err := NewClient(standby.URL, "test-fw")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, _, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	subscribed(t, fc, events, "fw-001")
	if err := client.Revive(ctx); err != nil {
		t.Fatalf("Revive: %v", err)
	}
	got := callTypes(fc)
	if len(got) != 2 || got[0] != medios.CallSubscribe || got[1] != medios.CallRevive {
		t.Fatalf("leader saw %v, want [SUBSCRIBE REVIVE]", got)
	}
}

func TestClient_SubscribeSkipsUnreachableAndLeaderlessControllers(t *testing.T) {
	fc := newFakeController(t)
	leaderless := newStandby(t, "")
	client, err := NewClient("http://127.0.0.1:1,"+leaderless.URL+","+fc.URL(), "test-fw")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := client.Subscribe(ctx); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got := callTypes(fc); len(got) != 1 || got[0] != medios.CallSubscribe {
		t.Fatalf("leader saw %v, want [SUBSCRIBE]", got)
	}
}

func TestClient_SubscribeReportsEveryControllerWhenNoneAccepts(t *testing.T) {
	leaderless := newStandby(t, "")
	client, err := NewClient("http://127.0.0.1:1,"+leaderless.URL, "test-fw")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Subscribe(context.Background())
	if err == nil {
		t.Fatal("Subscribe succeeded with no leader")
	}
	var ce *CallError
	if !errors.As(err, &ce) || ce.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want a wrapped 503 CallError, got %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error does not name the unreachable controller: %v", err)
	}
}

func TestClient_ResubscribeReusesAssignedFrameworkID(t *testing.T) {
	fc := newFakeController(t)
	client, err := NewClient(fc.URL(), "test-fw")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, _, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subscribed(t, fc, events, "fw-001")
	if _, _, err := client.Subscribe(ctx); err != nil {
		t.Fatal(err)
	}
	var subs []medios.Call
	for _, c := range fc.recordedCalls() {
		if c.body.Type == medios.CallSubscribe {
			subs = append(subs, c.body)
		}
	}
	if len(subs) != 2 {
		t.Fatalf("want 2 SUBSCRIBEs, got %d", len(subs))
	}
	if subs[0].Subscribe.FrameworkInfo.ID != nil {
		t.Errorf("first SUBSCRIBE carried an id: %v", subs[0].Subscribe.FrameworkInfo.ID)
	}
	if id := subs[1].Subscribe.FrameworkInfo.ID; id == nil || id.Value != "fw-001" {
		t.Errorf("second SUBSCRIBE id: want fw-001, got %v", id)
	}
}

func TestNewClient_RefusesEmptyControllerURL(t *testing.T) {
	if _, err := NewClient(" , ", "test-fw"); err == nil {
		t.Fatal("NewClient accepted an empty controller URL")
	}
}
