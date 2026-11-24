package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox makes a directory holding a task's stdout file and makes it the
// working directory, as the executor does when it launches an observer.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, defaultStdoutPath), "step 1\n")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	return dir
}

func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "anthropic")
	writeFile(t, path, "sk-key\n")
	return path
}

// No credential flag is a refusal: the observer says so on its stdout, where
// the executor turns it into the task's progress message, and exits 1.
func TestRunRefusesWithoutTheCredentialFlag(t *testing.T) {
	sandbox(t)
	var out, stderr syncBuffer
	if code := run(nil, &out, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	got := observations(&out)
	if len(got) != 1 || got[0] != "did not start: --credential-file was not given" {
		t.Fatalf("observations %q", got)
	}
}

// The credential comes from the flag and from nowhere else.
func TestRunIgnoresAnthropicApiKeyInTheEnvironment(t *testing.T) {
	sandbox(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-never-be-read")
	var out, stderr syncBuffer
	if code := run(nil, &out, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := observations(&out); len(got) != 1 {
		t.Fatalf("observations %q", got)
	}
}

func TestRunRefusesAnUnreadableOrEmptyCredentialFile(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"absent": func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		"empty": func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "anthropic")
			writeFile(t, path, "\n")
			return path
		},
	}
	for name, makePath := range cases {
		t.Run(name, func(t *testing.T) {
			sandbox(t)
			var out, stderr syncBuffer
			if code := run([]string{"--credential-file", makePath(t)}, &out, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			got := observations(&out)
			if len(got) != 1 || !strings.HasPrefix(got[0], "did not start: ") {
				t.Fatalf("observations %q", got)
			}
		})
	}
}

func TestRunRefusesAnEmptyPromptFile(t *testing.T) {
	sandbox(t)
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	writeFile(t, promptPath, "")

	var out, stderr syncBuffer
	code := run([]string{"--credential-file", keyFile(t), "--prompt-file", promptPath}, &out, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	got := observations(&out)
	if len(got) != 1 || !strings.Contains(got[0], "is empty") {
		t.Fatalf("observations %q", got)
	}
}

// medios passes the observer no arguments, so a positional argument means
// the invocation is not the one this binary has a contract for.
func TestRunRejectsPositionalArguments(t *testing.T) {
	sandbox(t)
	var out, stderr syncBuffer
	code := run([]string{"--credential-file", keyFile(t), "stdout", "health", "300"}, &out, &stderr)
	if code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if got := observations(&out); got != nil {
		t.Fatalf("observations %q, want none", got)
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	sandbox(t)
	var out, stderr syncBuffer
	if code := run([]string{"--kill"}, &out, &stderr); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
}

func TestRunRejectsNonPositiveParameters(t *testing.T) {
	cases := map[string][]string{
		"every must be a positive number of seconds": {"--every", "0"},
		"lines must be positive":                     {"--lines", "0"},
		"budget-tokens must be positive":             {"--budget-tokens", "0"},
		"max-consecutive-errors must be positive":    {"--max-consecutive-errors", "0"},
	}
	for reason, flags := range cases {
		t.Run(reason, func(t *testing.T) {
			sandbox(t)
			args := append([]string{"--credential-file", keyFile(t)}, flags...)
			var out, stderr syncBuffer
			if code := run(args, &out, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			got := observations(&out)
			if len(got) != 1 || !strings.Contains(got[0], "--"+reason) {
				t.Fatalf("observations %q", got)
			}
		})
	}
}
