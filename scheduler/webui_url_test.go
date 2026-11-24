package scheduler

import (
	"strings"
	"testing"
)

// The controller lists webui_url per framework in /state from the value sent
// at SUBSCRIBE, so the value has to reach the SUBSCRIBE body.
func TestWithWebUIURL_ReachesSubscribe(t *testing.T) {
	body := subscribeBody(t, WithWebUIURL("http://svc.example:8080"))
	if !strings.Contains(body, `"webui_url":"http://svc.example:8080"`) {
		t.Fatalf("webui_url did not reach SUBSCRIBE: %s", body)
	}
}

// Unset is omitted rather than sent empty.
func TestWithWebUIURL_OmittedWhenUnset(t *testing.T) {
	body := subscribeBody(t)
	if strings.Contains(body, "webui_url") {
		t.Fatalf("an unset webui_url should be omitted: %s", body)
	}
}

func TestWithWebUIURL_Rejects(t *testing.T) {
	cases := []struct{ name, url, want string }{
		{"empty", "", "is empty"},
		{"not http", "ftp://svc.example", "http or https"},
		{"no scheme", "svc.example:8080", "http or https"},
		{"no host", "http://:8080", "no host"},
		{"wildcard v4", "http://0.0.0.0:8080", "wildcard"},
		{"wildcard v6", "http://[::]:8080", "wildcard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient("http://127.0.0.1:5050", "f", WithWebUIURL(tc.url))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
