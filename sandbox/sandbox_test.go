package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNode_PathFor_ComposesMediosLayout(t *testing.T) {
	n := Node{ID: "node-7", WorkDir: "/var/lib/medios"}
	got := n.PathFor("fw-123", "task-abc", "ctr-xyz", "stdout")
	want := "/var/lib/medios/nodes/node-7/frameworks/fw-123/executors/task-abc/runs/ctr-xyz/stdout"
	if got != want {
		t.Errorf("PathFor:\n  got  %s\n  want %s", got, want)
	}
}

func TestResolver_Read_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files/read" {
			t.Errorf("path: want /files/read, got %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("path") != "/x/stdout" {
			t.Errorf("path qp: got %q", q.Get("path"))
		}
		if q.Get("offset") != "100" {
			t.Errorf("offset qp: got %q", q.Get("offset"))
		}
		_ = json.NewEncoder(w).Encode(ReadResult{Data: "5050\n", Offset: 5})
	}))
	defer srv.Close()

	host, portStr := splitHostPort(t, srv.URL)
	port, _ := strconv.Atoi(portStr)
	r := &Resolver{}

	out, err := r.Read(context.Background(), Node{Host: host, Port: port}, "/x/stdout", 100, -1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out.Data != "5050\n" || out.Offset != 5 {
		t.Errorf("unexpected result: %+v", out)
	}
}

func TestResolver_Read_NotFoundMapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	host, portStr := splitHostPort(t, srv.URL)
	port, _ := strconv.Atoi(portStr)
	r := &Resolver{}
	_, err := r.Read(context.Background(), Node{Host: host, Port: port}, "/missing", 0, -1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("404 should map to ErrNotFound, got %v", err)
	}
}

func TestResolver_Read_OtherStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	host, portStr := splitHostPort(t, srv.URL)
	port, _ := strconv.Atoi(portStr)
	r := &Resolver{}
	_, err := r.Read(context.Background(), Node{Host: host, Port: port}, "/path", 0, -1)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500 should produce a 500-mentioning error, got %v", err)
	}
}

func TestResolver_Read_UnreachableNode(t *testing.T) {
	r := &Resolver{Timeout: 50 * time.Millisecond}
	_, err := r.Read(context.Background(), Node{Host: "127.0.0.1", Port: 1}, "/anything", 0, -1)
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Errorf("connect-refused should map to ErrNodeUnreachable, got %v", err)
	}
}

func TestResolver_Read_OmitsLengthWhenNegative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("length") {
			t.Errorf("length should be omitted when caller passes -1, got %q", r.URL.Query().Get("length"))
		}
		_ = json.NewEncoder(w).Encode(ReadResult{Data: "", Offset: 0})
	}))
	defer srv.Close()
	host, portStr := splitHostPort(t, srv.URL)
	port, _ := strconv.Atoi(portStr)
	r := &Resolver{}
	_, err := r.Read(context.Background(), Node{Host: host, Port: port}, "/x", 0, -1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
}

// splitHostPort tears apart an httptest URL like "http://127.0.0.1:54321" into
// ("127.0.0.1", "54321").
func splitHostPort(t *testing.T, raw string) (string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		t.Fatalf("bad httptest URL %q", raw)
	}
	return host, port
}
