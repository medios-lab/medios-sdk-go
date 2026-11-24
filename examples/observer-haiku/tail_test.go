package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTailLinesShorterThanRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	writeFile(t, path, "one\ntwo\nthree\n")

	got, err := tailLines(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two", "three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTailLinesWithoutTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	writeFile(t, path, "one\ntwo")

	got, err := tailLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "two" {
		t.Fatalf("got %q", got)
	}
}

func TestTailLinesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	writeFile(t, path, "")

	got, err := tailLines(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want no lines", got)
	}
}

func TestTailLinesKeepsOnlyTheLastN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "step %d\n", i)
	}
	writeFile(t, path, b.String())

	got, err := tailLines(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"step 47", "step 48", "step 49"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTailLinesSeesWritesBetweenReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	writeFile(t, path, "step 1\n")

	first, err := tailLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0] != "step 1" {
		t.Fatalf("first read got %q", first)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("step 2\nstep 3\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	second, err := tailLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"step 2", "step 3"}
	if strings.Join(second, "|") != strings.Join(want, "|") {
		t.Fatalf("second read got %q, want %q", second, want)
	}
}

// A 10 MB file is read by its tail: the reader touches one chunk, not the
// file, and a partial first line is dropped rather than returned.
func TestTailLinesDoesNotReadTheWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n" // 100 bytes
	for i := 0; i < 100000; i++ {          // 10 MB
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 10_000_000 {
		t.Fatalf("fixture is %d bytes", st.Size())
	}

	open, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()

	got, read, err := tailLinesFrom(open, 200, tailChunkBytes, tailMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 {
		t.Fatalf("got %d lines, want 200", len(got))
	}
	for _, l := range got {
		if len(l) != 99 {
			t.Fatalf("got a %d byte line, want whole lines only", len(l))
		}
	}
	// 200 lines of 100 bytes need 20001 bytes, so two 64 KiB chunks is the
	// most the reader may touch.
	if read > 2*tailChunkBytes {
		t.Fatalf("read %d bytes, want at most %d", read, 2*tailChunkBytes)
	}
}

// One line longer than the read bound still yields a tail rather than
// nothing, and the read stays bounded.
func TestTailLinesBoundsOneEnormousLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	writeFile(t, path, strings.Repeat("y", 3*1024*1024)+"\n")

	open, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()

	got, read, err := tailLinesFrom(open, 200, tailChunkBytes, tailMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if read > tailMaxBytes {
		t.Fatalf("read %d bytes, want at most %d", read, tailMaxBytes)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if int64(len(got[0])) > tailMaxBytes {
		t.Fatalf("line is %d bytes", len(got[0]))
	}
}
