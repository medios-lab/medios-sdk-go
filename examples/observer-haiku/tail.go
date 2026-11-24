package main

import (
	"bytes"
	"io"
	"os"
)

// tailChunkBytes is how much the tail reader reads per backward step, in
// bytes.
//
// Type: default. Owner: this binary. It trades read calls against resident
// bytes and changes no observation. 64 KiB holds roughly 600 log lines of
// 100 bytes, so the common case of a 200-line tail finishes in one read.
const tailChunkBytes int64 = 64 * 1024

// tailMaxBytes bounds how much of the stdout file the tail reader will read
// for one judgment, in bytes.
//
// Type: default. Owner: this binary. The reason the reader exists is that a
// task's stdout grows without bound, so the bound is on the read and not on
// the file: 1 MiB is about 10,000 ordinary log lines, well past the
// 200-line default, and it caps what a job writing one enormous line can
// make this process hold. When the bound cuts the read, the tail is the
// newest bytes within it and the oldest partial line is dropped.
const tailMaxBytes int64 = 1024 * 1024

// tailLines returns the last n lines of the file at path, reading only the
// end of the file.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lines, _, err := tailLinesFrom(f, n, tailChunkBytes, tailMaxBytes)
	return lines, err
}

// tailLinesFrom reads the last n lines from f and also reports how many
// bytes it read, which is what lets a test assert that a large file was not
// loaded.
//
// Each call stats f afresh, so a file that grew since the previous call
// yields the newer tail.
func tailLinesFrom(f *os.File, n int, chunk, max int64) ([]string, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if n <= 0 || size == 0 {
		return nil, 0, nil
	}

	var (
		chunks   [][]byte
		newlines int
		pos      = size
		read     int64
	)
	for pos > 0 && newlines <= n && read < max {
		want := chunk
		if want > pos {
			want = pos
		}
		if read+want > max {
			want = max - read
		}
		pos -= want
		b := make([]byte, want)
		if _, err := f.ReadAt(b, pos); err != nil && err != io.EOF {
			return nil, read, err
		}
		newlines += bytes.Count(b, []byte{'\n'})
		chunks = append(chunks, b)
		read += want
	}

	var data []byte
	for i := len(chunks) - 1; i >= 0; i-- {
		data = append(data, chunks[i]...)
	}
	// A trailing newline terminates the last line rather than starting an
	// empty one.
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) == 0 {
		return nil, read, nil
	}

	lines := bytes.Split(data, []byte("\n"))
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, string(bytes.TrimSuffix(l, []byte("\r"))))
	}
	// Reading backwards from the middle of the file starts inside a line.
	if pos > 0 && len(out) > 1 {
		out = out[1:]
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, read, nil
}
