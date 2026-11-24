package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
	"github.com/medios-lab/medios-sdk-go/scheduler"
)

func offer(id, role string, res ...string) medios.Offer {
	o := medios.Offer{
		ID:             medios.ID{Value: id},
		NodeID:         medios.ID{Value: "N-" + id},
		Hostname:       "host-" + id,
		AllocationInfo: &medios.AllocationInfo{Role: role},
	}
	for _, r := range res {
		name, v, _ := strings.Cut(r, "=")
		q, _ := strconv.ParseFloat(v, 64)
		o.Resources = append(o.Resources, medios.Resource{
			Name: name, Type: "SCALAR", Scalar: &medios.Scalar{Value: q},
			AllocationInfo: &medios.AllocationInfo{Role: role},
		})
	}
	return o
}

// A fake controller sends one SUBSCRIBED and one OFFERS event carrying an
// offer for each role and keeps the stream open. The framework must log both
// roles, decline both offers, launch nothing, and send TEARDOWN while its
// stream is still connected: like the real controller, the fake answers a
// TEARDOWN after the stream has dropped with 400.
func TestRunLogsAndDeclinesOffersForBothRoles(t *testing.T) {
	var (
		mu        sync.Mutex
		calls     []medios.Call
		sub       string
		connected bool
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := []medios.Event{
		{Type: medios.EventSubscribed, Subscribed: &medios.SubscribedBody{FrameworkID: medios.ID{Value: "F-1"}}},
		{Type: medios.EventOffers, Offers: &medios.OffersBody{Offers: []medios.Offer{
			offer("O-1", "*", "cpus=4", "mem=1024"),
			offer("O-2", "pool/batch", "cpus=2", "exclusive_slot=1"),
		}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var call medios.Call
		if err := json.Unmarshal(b, &call); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		switch call.Type {
		case medios.CallSubscribe:
		case medios.CallTeardown:
			mu.Lock()
			up := connected
			mu.Unlock()
			if !up {
				http.Error(w, "Framework cannot be found", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		case medios.CallDecline:
			w.WriteHeader(http.StatusAccepted)
			cancel() // the framework has acted on every offer; end the run
			return
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		mu.Lock()
		sub, connected = string(b), true
		mu.Unlock()
		defer func() {
			mu.Lock()
			connected = false
			mu.Unlock()
		}()
		w.Header().Set("Medios-Stream-Id", "S-1")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			p, _ := json.Marshal(ev)
			fmt.Fprintf(w, "%d\n%s", len(p), p)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := scheduler.NewClient(srv.URL, "multi-role-example", scheduler.WithRoles("*", "pool/batch"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := run(ctx, c, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatal(err)
	}
	log := strings.Join(lines, "\n")

	for _, want := range []string{
		"offer O-1 node=N-O-1 host=host-O-1 role=* cpus=4@* mem=1024@*",
		"offer O-2 node=N-O-2 host=host-O-2 role=pool/batch cpus=2@pool/batch exclusive_slot=1@pool/batch",
		"torn down",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(sub, `"roles":["*","pool/batch"]`) {
		t.Errorf("SUBSCRIBE lacks the roles: %s", sub)
	}
	var types []string
	declined := map[string]bool{}
	for _, call := range calls {
		types = append(types, call.Type)
		if call.Decline != nil {
			for _, id := range call.Decline.OfferIDs {
				declined[id.Value] = true
			}
		}
	}
	if got, want := strings.Join(types, ","), "SUBSCRIBE,DECLINE,TEARDOWN"; got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
	if !declined["O-1"] || !declined["O-2"] {
		t.Errorf("declined %v, want O-1 and O-2", declined)
	}
}
