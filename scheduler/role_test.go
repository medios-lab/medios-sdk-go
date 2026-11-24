package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/medios-lab/medios-sdk-go/medios"
)

// A framework only receives resources reserved for a role if it subscribes
// under that role, so the role has to reach SUBSCRIBE or reserved supply is
// never offered and the code that handles it is unreachable.

func subscribeBody(t *testing.T, opts ...ClientOption) string {
	t.Helper()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case got <- string(b):
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "f", opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _ = c.Subscribe(ctx)
	select {
	case b := <-got:
		return b
	case <-ctx.Done():
		t.Fatal("controller never received a SUBSCRIBE")
		return ""
	}
}

func TestWithRole_ReachesSubscribe(t *testing.T) {
	body := subscribeBody(t, WithRole("fennel"))
	if !strings.Contains(body, `"role":"fennel"`) {
		t.Fatalf("role did not reach SUBSCRIBE: %s", body)
	}
}

// Unset means the default role, and the field is omitted rather than sent
// empty, which a controller would have to interpret.
func TestWithRole_OmittedWhenUnset(t *testing.T) {
	body := subscribeBody(t)
	if strings.Contains(body, `"role"`) {
		t.Fatalf("an unset role should be omitted entirely: %s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("SUBSCRIBE body is not valid JSON: %v", err)
	}
}

// The role coexists with the other framework options rather than replacing
// them, since a role-bearing framework still declares capabilities and a user.
func TestWithRole_CoexistsWithOtherOptions(t *testing.T) {
	body := subscribeBody(t,
		WithRole("fennel"),
		WithUser("alice"),
		WithCapabilities("GPU_RESOURCES", "PARTITION_AWARE"),
	)
	for _, want := range []string{`"role":"fennel"`, `"user":"alice"`, `"GPU_RESOURCES"`, `"PARTITION_AWARE"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}

// A multi-role SUBSCRIBE carries roles and the capability, and no role: the
// controller refuses role alongside MULTI_ROLE (medios
// src/controller/validation.cpp, validateRoles).
func TestWithRoles_ReachesSubscribe(t *testing.T) {
	body := subscribeBody(t, WithRoles("*", "pool/batch"), WithCapabilities(medios.CapabilityPartitionAware))
	var call struct {
		Subscribe struct {
			FrameworkInfo map[string]json.RawMessage `json:"framework_info"`
		} `json:"subscribe"`
	}
	if err := json.Unmarshal([]byte(body), &call); err != nil {
		t.Fatalf("decode SUBSCRIBE: %v", err)
	}
	info := call.Subscribe.FrameworkInfo
	if got := string(info["roles"]); got != `["*","pool/batch"]` {
		t.Errorf("roles = %s, want [\"*\",\"pool/batch\"]", got)
	}
	if _, ok := info["role"]; ok {
		t.Errorf("multi-role SUBSCRIBE carries role: %s", body)
	}
	if got, want := string(info["capabilities"]), `[{"type":"PARTITION_AWARE"},{"type":"MULTI_ROLE"}]`; got != want {
		t.Errorf("capabilities = %s, want %s", got, want)
	}
}

// Declaring MULTI_ROLE by hand as well does not send it twice.
func TestWithRoles_CapabilityNotDuplicated(t *testing.T) {
	body := subscribeBody(t, WithCapabilities(medios.CapabilityMultiRole), WithRoles("*"))
	if n := strings.Count(body, `"MULTI_ROLE"`); n != 1 {
		t.Fatalf("MULTI_ROLE appears %d times: %s", n, body)
	}
}

// Without WithRoles the body is the single-role body it always was.
func TestWithRoles_SingleRoleBodyUnchanged(t *testing.T) {
	body := subscribeBody(t, WithRole("fennel"), WithCapabilities(medios.CapabilityGPUResources))
	for _, absent := range []string{`"roles"`, `"MULTI_ROLE"`} {
		if strings.Contains(body, absent) {
			t.Errorf("single-role SUBSCRIBE carries %s: %s", absent, body)
		}
	}
	if !strings.Contains(body, `"role":"fennel"`) {
		t.Errorf("role missing: %s", body)
	}
}

func TestWithRoles_Refused(t *testing.T) {
	cases := map[string][]ClientOption{
		"with WithRole": {WithRole("x"), WithRoles("y")},
		"no roles":      {WithRoles()},
		"empty role":    {WithRoles("*", "")},
		"repeated role": {WithRoles("x", "x")},
	}
	for name, opts := range cases {
		if _, err := NewClient("http://localhost", "test", opts...); err == nil {
			t.Errorf("%s: NewClient accepted it", name)
		}
	}
}
