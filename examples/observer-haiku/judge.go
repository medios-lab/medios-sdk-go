package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// messagesEndpoint is the Anthropic Messages API endpoint the agent sends
// judgments to.
//
// Type: default. Owner: this binary. A field on the judge rather than a
// constant at the call site so a test can point it at a local server; it is
// not a flag, because a task declaring a different endpoint is a different
// product, not a parameter of this one.
const messagesEndpoint = "https://api.anthropic.com/v1/messages"

// anthropicVersion is the API version header value the Messages API
// requires. Type: invariant of the API contract, not a parameter.
const anthropicVersion = "2023-06-01"

// judgmentMaxTokens bounds the model's reply, in output tokens.
//
// Type: parameter. Owner: this binary. The verdict is specified as one line
// beginning OK or ISSUE, which is a few dozen tokens; 120 leaves room for a
// clause of explanation and makes a model that starts writing prose run out
// rather than bill for it.
const judgmentMaxTokens = 120

// errorBodyPrefixBytes is how much of a non-200 response body is quoted in
// the error, in bytes. Enough to carry the API's own error message.
const errorBodyPrefixBytes = 300

// defaultPrompt is the system prompt used when --prompt-file is not given.
//
// Type: default. Owner: this binary. It states the one thing the observation
// format depends on, that the reply is one line beginning OK or ISSUE, and
// deliberately says nothing about any particular job: the agent sees only
// stdout and does not know whether it is watching a training run or a
// shell loop.
const defaultPrompt = `You are watching the recent standard output of a long-running compute task on a cluster. ` +
	`You are given the last lines of that output and nothing else. ` +
	`Decide whether the task looks like it is doing its work.` +
	"\n\n" +
	`Answer with exactly one line. Begin the line with OK if the output looks like a task that is working, ` +
	`or with ISSUE if the output shows an error, a repeated failure, a hang, output that has stopped changing, ` +
	`diverging or absent numbers, or anything else a person watching the job would want to know about. ` +
	`After that word, state in one short clause what you saw. ` +
	`Do not explain your reasoning, do not use more than one line, and do not guess at what the task is for.` +
	"\n\n" +
	`If the output is empty or too short to judge, answer OK and say that there is not yet enough output.`

// usage is the token accounting of one judgment.
type usage struct {
	Input  int
	Output int
}

// judge sends a tail of task output to the model and returns its reply.
type judge struct {
	client   *http.Client
	endpoint string
	cred     credential
	model    string
	prompt   string
}

type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system"`
	Messages  []messagesContent `json:"messages"`
}

type messagesContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// judgeTail asks the model about lines and returns the reply text and the
// tokens the call spent.
func (j *judge) judgeTail(ctx context.Context, lines []string) (string, usage, error) {
	// Read on every call: a key rotated in place takes effect here.
	key, err := j.cred.read()
	if err != nil {
		return "", usage{}, err
	}

	body, err := json.Marshal(messagesRequest{
		Model:     j.model,
		MaxTokens: judgmentMaxTokens,
		System:    j.prompt,
		Messages: []messagesContent{{
			Role:    "user",
			Content: "Recent standard output of the task:\n\n" + strings.Join(lines, "\n"),
		}},
	})
	if err != nil {
		return "", usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", usage{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("x-api-key", key)

	resp, err := j.client.Do(req)
	if err != nil {
		return "", usage{}, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", usage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", usage{}, fmt.Errorf("messages api returned %d: %s",
			resp.StatusCode, truncateBytes(strings.TrimSpace(buf.String()), errorBodyPrefixBytes))
	}

	var parsed messagesResponse
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		return "", usage{}, fmt.Errorf("messages api reply did not parse as json: %w", err)
	}
	u := usage{Input: parsed.Usage.InputTokens, Output: parsed.Usage.OutputTokens}

	var text strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	if text.Len() == 0 {
		return "", u, fmt.Errorf("messages api reply carried no text block")
	}
	return text.String(), u, nil
}

// verdictBody turns a model reply into the body of an observation line. A
// reply that does not begin with OK or ISSUE is recorded as UNPARSED rather
// than coerced into a verdict: what the model said instead is the evidence.
func verdictBody(reply string) string {
	flat := strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(reply))
	switch {
	case hasVerdictPrefix(flat, "OK"):
		return flat
	case hasVerdictPrefix(flat, "ISSUE"):
		return flat
	default:
		return "UNPARSED " + truncateBytes(flat, observationUnparsedPrefixBytes)
	}
}

// hasVerdictPrefix reports whether flat begins with the verdict word as a
// word, so that "OKAY, the loss is falling" is not read as a verdict.
func hasVerdictPrefix(flat, word string) bool {
	if !strings.HasPrefix(flat, word) {
		return false
	}
	rest := flat[len(word):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case ' ', ':', ',', '.', '-', ';':
		return true
	}
	return false
}
