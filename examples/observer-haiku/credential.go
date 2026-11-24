package main

import (
	"fmt"
	"os"
	"strings"
)

// credential is the file holding the API key.
//
// DR-041 keeps credentials out of medios: an observer inherits the
// executor's environment, which the node sets, and the observer's command
// refers to what the operator put there. This observer takes the path as
// --credential-file and reads nothing else. There is no ANTHROPIC_API_KEY
// fallback, because an observer that silently fell back would bill an
// unrelated account and the operator would learn nothing about the
// misconfigured node.
type credential struct {
	path string
}

// read returns the key, reading the file afresh on every call so that a key
// rotated in place takes effect at the next judgment with no restart.
//
// An unreadable file and an empty file are one reason to the caller: the
// credential is not readable at that path.
func (c credential) read() (string, error) {
	b, err := os.ReadFile(c.path)
	if err != nil {
		return "", fmt.Errorf("could not read the credential file %s: %v", c.path, err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("the credential file %s is empty", c.path)
	}
	return key, nil
}
