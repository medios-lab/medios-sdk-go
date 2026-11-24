package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// binaryBytes has quotes, backslashes, control bytes, a multi-byte rune
// and bytes that are not valid UTF-8: everything /files/read would mangle
// (medios#582) and /files/download must not.
var binaryBytes = []byte("q\" b\\ \n\t\x01\x1f \xc3\xa9 \xff\xfe\x80 end")

func TestResolver_BrowseAndDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		switch r.URL.Path {
		case "/files/browse":
			if p != "/stores/results/ns" {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode([]FileEntry{
				{Path: "/stores/results/ns/job", Size: 96, Mode: "drwxr-xr-x", NLink: 3},
				{Path: "/stores/results/ns/note.bin", Size: int64(len(binaryBytes)), Mode: "-rw-r--r--", NLink: 1},
			})
		case "/files/download":
			switch p {
			case "/stores/results/ns/note.bin":
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write(binaryBytes)
			case "/stores/results/ns":
				http.Error(w, "Cannot download a directory.\n", http.StatusBadRequest)
			default:
				http.NotFound(w, r)
			}
		}
	}))
	defer srv.Close()
	host, portStr := splitHostPort(t, srv.URL)
	port, _ := strconv.Atoi(portStr)
	n := Node{Host: host, Port: port}
	r := &Resolver{}
	ctx := context.Background()

	entries, err := r.Browse(ctx, n, n.StorePathFor("results", "ns"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("browse: %v %+v", err, entries)
	}
	if !entries[0].IsDir() || entries[0].Name() != "job" || entries[1].IsDir() || entries[1].Name() != "note.bin" || entries[1].Size != int64(len(binaryBytes)) {
		t.Fatalf("entries: %+v", entries)
	}
	if _, err := r.Browse(ctx, n, "/stores/results/absent"); err != ErrNotFound {
		t.Fatalf("absent dir: %v", err)
	}

	body, err := r.Download(ctx, n, "/stores/results/ns/note.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(got) != string(binaryBytes) {
		t.Fatalf("download: %v got %q want %q", err, got, binaryBytes)
	}
	if _, err := r.Download(ctx, n, "/stores/results/ns/absent.bin"); err != ErrNotFound {
		t.Fatalf("absent file: %v", err)
	}
	if _, err := r.Download(ctx, n, "/stores/results/ns"); err == nil || err == ErrNotFound {
		t.Fatalf("a directory must be an error naming the refusal, got %v", err)
	}
}
