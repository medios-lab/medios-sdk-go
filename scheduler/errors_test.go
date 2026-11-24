package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A controller that answers and refuses is a different fact from a controller
// that never answers: only the first makes rolling back safe.

func clientAgainst(t *testing.T, h http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c, err := NewClient(srv.URL, "f")
	if err != nil {
		srv.Close()
		t.Fatalf("NewClient: %v", err)
	}
	// Subscribe so the stream id exists; the handler decides what happens.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _ = c.Subscribe(ctx)
	return c, srv.Close
}

func TestRejected_OnControllerRefusal(t *testing.T) {
	c, done := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Medios-Stream-Id") == "" {
			w.Header().Set("Medios-Stream-Id", "s-1")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "offer is no longer valid", http.StatusConflict)
	})
	defer done()

	err := c.Decline(context.Background(), nil, 0)
	if err == nil {
		// No skip here on purpose: a skip would let this test pass without
		// ever reaching the refusal path it exists to check.
		t.Fatal("the controller refused; Decline should have returned an error")
	}
	if !Rejected(err) {
		t.Fatalf("a controller refusal must be Rejected: %v", err)
	}
	var re *CallError
	if !errors.As(err, &re) {
		t.Fatalf("error should unwrap to *CallError: %v", err)
	}
	if re.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", re.StatusCode)
	}
	if re.Call == "" || re.Body == "" {
		t.Errorf("rejection should carry the call and body: %+v", re)
	}
}

// Ambiguity is not rejection. A transport failure leaves the caller unable to
// know whether the controller acted, so Rejected must be false and a caller
// must not roll back on it.
func TestRejected_FalseOnAmbiguousFailure(t *testing.T) {
	for name, err := range map[string]error{
		"timeout":   context.DeadlineExceeded,
		"cancelled": context.Canceled,
		"transport": errors.New("scheduler: ACCEPT: dial tcp: connection reset by peer"),
		"nil":       nil,
	} {
		if Rejected(err) {
			t.Errorf("%s must not be reported as a rejection", name)
		}
	}
}

// Wrapping survives, since callers wrap before deciding.
func TestRejected_ThroughWrapping(t *testing.T) {
	base := &CallError{Call: "ACCEPT", StatusCode: 400, Body: "bad task"}
	wrapped := errors.Join(errors.New("launching /svc/web"), base)
	if !Rejected(wrapped) {
		t.Fatal("a wrapped rejection is still a rejection")
	}
}

// A 5xx is not proof the call had no effect, and this is the case that makes
// the predicate worth having. A 500 can come from a controller that crashed
// after acting; a 502 or 504 from an intermediary that forwarded successfully
// and lost the answer. Rolling back on those forgets work that exists.
func TestRejected_FiveHundredsAreAmbiguous(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		err := &CallError{Call: "ACCEPT", StatusCode: code, Body: "upstream"}
		if Rejected(err) {
			t.Errorf("HTTP %d is not proof the call had no effect", code)
		}
		// It is still an error worth surfacing, just not a safe rollback.
		if err.Error() == "" {
			t.Errorf("HTTP %d should still report itself", code)
		}
	}
}

// A 4xx is a refusal: the request was declined before anything was done with
// it, so undoing locally is safe.
func TestRejected_FourHundredsAreRefusals(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 422, 429} {
		if !Rejected(&CallError{Call: "ACCEPT", StatusCode: code, Body: "no"}) {
			t.Errorf("HTTP %d should be a proven refusal", code)
		}
	}
}

// 3xx is neither. An unfollowed redirect is not an answer about the call.
func TestRejected_RedirectsAreNotRefusals(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		if Rejected(&CallError{Call: "ACCEPT", StatusCode: code, Body: ""}) {
			t.Errorf("HTTP %d is not a refusal", code)
		}
	}
}

// A gateway that forwards ACCEPT, sees it dispatched, and then cannot deliver
// the answer is the case this predicate exists to get right: the task may be
// running. Asked for by medios-eng, whose review names the dispatch point at
// medios src/controller/http.cpp:649.
func TestRejected_ForwardedThenLostResponse(t *testing.T) {
	var forwarded []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Medios-Stream-Id") == "" {
			w.Header().Set("Medios-Stream-Id", "s-1")
			w.WriteHeader(http.StatusOK)
			return
		}
		// The controller received and dispatched it.
		forwarded = append(forwarded, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	for _, gatewayCode := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Medios-Stream-Id") == "" {
				w.Header().Set("Medios-Stream-Id", "s-1")
				w.WriteHeader(http.StatusOK)
				return
			}
			// Forward, then lose the answer.
			req, _ := http.NewRequest(r.Method, upstream.URL+r.URL.Path, r.Body)
			req.Header = r.Header.Clone()
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
			w.WriteHeader(gatewayCode)
		}))

		c, err := NewClient(gateway.URL, "f")
		if err != nil {
			gateway.Close()
			t.Fatalf("NewClient: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _, _ = c.Subscribe(ctx)
		before := len(forwarded)
		acceptErr := c.Accept(ctx, nil, nil)
		cancel()
		gateway.Close()

		if len(forwarded) == before {
			t.Fatalf("HTTP %d: the fixture did not forward; the case was not exercised", gatewayCode)
		}
		if acceptErr == nil {
			t.Fatalf("HTTP %d: expected the gateway error to surface", gatewayCode)
		}
		if Rejected(acceptErr) {
			t.Errorf("HTTP %d after a forwarded ACCEPT must not read as a refusal: %v", gatewayCode, acceptErr)
		}
	}
}

// SUBSCRIBE answered with a 4xx is a refusal the caller must not retry
// unchanged; a 5xx is not. Both carry the status and body.
func TestRejected_OnSubscribe(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusForbidden, true},
		{http.StatusInternalServerError, false},
		{http.StatusServiceUnavailable, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "subscribe answer", tc.status)
		}))
		c, err := NewClient(srv.URL, "f")
		if err != nil {
			srv.Close()
			t.Fatalf("NewClient: %v", err)
		}
		_, _, err = c.Subscribe(context.Background())
		srv.Close()
		var ce *CallError
		if !errors.As(err, &ce) {
			t.Fatalf("HTTP %d: error should be a *CallError: %v", tc.status, err)
		}
		if ce.Call != "SUBSCRIBE" || ce.StatusCode != tc.status || ce.Body != "subscribe answer" {
			t.Errorf("HTTP %d: CallError = %+v", tc.status, ce)
		}
		if got := Rejected(err); got != tc.want {
			t.Errorf("HTTP %d: Rejected = %v, want %v", tc.status, got, tc.want)
		}
	}
}
