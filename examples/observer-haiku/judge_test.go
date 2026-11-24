package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// replyServer answers every Messages request with one text block and fixed
// usage, and records the last request it received.
func replyServer(t *testing.T, text string, in, out int) (*httptest.Server, *messagesRequest, *http.Header) {
	t.Helper()
	var got messagesRequest
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request did not parse: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"usage":   map[string]int{"input_tokens": in, "output_tokens": out},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

// testCredential writes a key file and returns the credential naming it.
func testCredential(t *testing.T, key string) credential {
	t.Helper()
	path := filepath.Join(t.TempDir(), "anthropic")
	writeFile(t, path, key+"\n")
	return credential{path: path}
}

func testJudge(t *testing.T, endpoint string) *judge {
	t.Helper()
	return &judge{
		client:   &http.Client{},
		endpoint: endpoint,
		cred:     testCredential(t, "test-key"),
		model:    defaultModel,
		prompt:   defaultPrompt,
	}
}

func TestJudgeTailSendsTheContract(t *testing.T) {
	srv, req, hdr := replyServer(t, "OK loss falling", 1900, 20)
	j := testJudge(t, srv.URL)

	reply, used, err := j.judgeTail(context.Background(), []string{"step 1 loss 2.0", "step 2 loss 1.8"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "OK loss falling" {
		t.Fatalf("reply %q", reply)
	}
	if used.Input != 1900 || used.Output != 20 {
		t.Fatalf("usage %+v", used)
	}
	if req.Model != defaultModel {
		t.Fatalf("model %q", req.Model)
	}
	if req.MaxTokens != judgmentMaxTokens {
		t.Fatalf("max_tokens %d, want %d", req.MaxTokens, judgmentMaxTokens)
	}
	if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content, "step 2 loss 1.8") {
		t.Fatalf("messages %+v", req.Messages)
	}
	if got := hdr.Get("anthropic-version"); got != anthropicVersion {
		t.Fatalf("anthropic-version %q", got)
	}
	if got := hdr.Get("x-api-key"); got != "test-key" {
		t.Fatalf("x-api-key %q", got)
	}
}

func TestJudgeTailReportsAnApiError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	_, _, err := testJudge(t, srv.URL).judgeTail(context.Background(), []string{"step 1"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("error %q", err)
	}
}

func TestJudgeTailReportsAReplyWithNoText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[],"usage":{"input_tokens":5,"output_tokens":0}}`)
	}))
	defer srv.Close()

	_, used, err := testJudge(t, srv.URL).judgeTail(context.Background(), []string{"step 1"})
	if err == nil {
		t.Fatal("want an error")
	}
	// The call is still billed, so its usage must come back with the error.
	if used.Input != 5 {
		t.Fatalf("usage %+v", used)
	}
}

func TestVerdictBodyParsing(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
	}{
		{"ok", "OK loss falling", "OK loss falling"},
		{"issue", "ISSUE cuda out of memory repeats", "ISSUE cuda out of memory repeats"},
		{"ok alone", "OK", "OK"},
		{"ok with colon", "OK: step count rising", "OK: step count rising"},
		{"trailing newline", "OK steady\n", "OK steady"},
		{"multiline verdict is flattened", "ISSUE a hang\nsince step 4", "ISSUE a hang since step 4"},
		{"prose is unparsed", "The job looks fine to me.", "UNPARSED The job looks fine to me."},
		{"okay is not a verdict", "OKAY the loss is falling", "UNPARSED OKAY the loss is falling"},
		{"empty is unparsed", "", "UNPARSED "},
		{"lowercase is unparsed", "ok loss falling", "UNPARSED ok loss falling"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdictBody(c.reply); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestVerdictBodyTruncatesAnUnparsedReply(t *testing.T) {
	got := verdictBody(strings.Repeat("w", 900))
	if len(got) != len("UNPARSED ")+observationUnparsedPrefixBytes {
		t.Fatalf("got %d bytes", len(got))
	}
}

// The key is read on every call, so a key rotated in place is used by the
// next judgment without a restart.
func TestJudgeTailReadsTheKeyOnEveryCall(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("x-api-key"))
		io.WriteString(w, `{"content":[{"type":"text","text":"OK fine"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	j := testJudge(t, srv.URL)
	if _, _, err := j.judgeTail(context.Background(), []string{"step 1"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, j.cred.path, "rotated-key\n")
	if _, _, err := j.judgeTail(context.Background(), []string{"step 2"}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "test-key" || keys[1] != "rotated-key" {
		t.Fatalf("keys sent %q", keys)
	}
}

// A credential that disappears mid-run is an error on the call, not a panic
// and not a request with no key.
func TestJudgeTailFailsWhenTheCredentialGoesAway(t *testing.T) {
	srv, _, _ := replyServer(t, "OK fine", 1, 1)
	j := testJudge(t, srv.URL)
	if err := os.Remove(j.cred.path); err != nil {
		t.Fatal(err)
	}

	_, _, err := j.judgeTail(context.Background(), []string{"step 1"})
	if err == nil || !strings.Contains(err.Error(), "could not read the credential file") {
		t.Fatalf("error %v", err)
	}
}

func TestCredentialRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anthropic")
	c := credential{path: path}

	if _, err := c.read(); err == nil {
		t.Fatal("want an error for an absent file")
	}
	writeFile(t, path, "  \n")
	if _, err := c.read(); err == nil {
		t.Fatal("want an error for an empty file")
	}
	writeFile(t, path, "sk-key\n")
	got, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-key" {
		t.Fatalf("key %q, want the trailing newline trimmed", got)
	}
}
