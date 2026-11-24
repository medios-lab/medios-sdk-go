package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// An observation carries no name prefix: DR-041 has the executor add the
// observer's name.
func TestObservationLineFormat(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"verdict", "OK loss falling, step 1240", "OK loss falling, step 1240"},
		{"issue", "ISSUE the same error repeats", "ISSUE the same error repeats"},
		{"flattens newlines", "OK first\nsecond", "OK first second"},
		{"trims", "  OK spaced  ", "OK spaced"},
		{"start failure", "did not start: no credential", "did not start: no credential"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := observationLine(c.body); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestObservationLineTruncatesToTheBound(t *testing.T) {
	got := observationLine("OK " + strings.Repeat("z", 5000))
	if len(got) != observationMaxBytes {
		t.Fatalf("got %d bytes, want %d", len(got), observationMaxBytes)
	}
	if !strings.HasPrefix(got, "OK zzz") {
		t.Fatalf("got %q", got[:20])
	}
}

// Truncation cuts at a rune boundary: a half-written multi-byte character
// would make the observation invalid UTF-8 on the wire.
func TestObservationLineTruncatesOnARuneBoundary(t *testing.T) {
	got := observationLine("OK " + strings.Repeat("é", 2000))
	if !utf8.ValidString(got) {
		t.Fatalf("got invalid utf-8: %q", got[len(got)-8:])
	}
	if len(got) > observationMaxBytes || len(got) < observationMaxBytes-1 {
		t.Fatalf("got %d bytes, want close to %d", len(got), observationMaxBytes)
	}
}

func TestEmitWritesOneLineEach(t *testing.T) {
	var out bytes.Buffer
	for _, body := range []string{"OK one", "ISSUE two", "error: three"} {
		if err := emit(&out, body); err != nil {
			t.Fatal(err)
		}
	}
	want := "OK one\nISSUE two\nerror: three\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestEmitReportsAWriteFailure(t *testing.T) {
	if err := emit(failingWriter{}, "OK one"); err == nil {
		t.Fatal("want an error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriteFailed }
