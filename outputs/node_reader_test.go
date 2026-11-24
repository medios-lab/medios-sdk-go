package outputs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/medios-lab/medios-sdk-go/sandbox"
)

// fakeFilesNode serves a directory tree the way a node's files API serves
// an attached store at /stores/<name>: /files/browse lists a directory as
// FileInfo objects and /files/download streams a file's raw bytes,
// refusing a directory with 400. /files/read is not served: nothing here
// may use it (medios#582).
func fakeFilesNode(t *testing.T, root, store string) (sandbox.Node, *sandbox.Resolver, *int) {
	t.Helper()
	calls := 0
	prefix := path.Join(sandbox.StoresRoot, store)
	local := func(p string) (string, bool) {
		if p != prefix && !strings.HasPrefix(p, prefix+"/") {
			return "", false
		}
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, prefix))), true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/files/browse", func(w http.ResponseWriter, r *http.Request) {
		calls++
		p := r.URL.Query().Get("path")
		lp, ok := local(p)
		if !ok {
			http.NotFound(w, r)
			return
		}
		entries, err := os.ReadDir(lp)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var out []sandbox.FileEntry
		for _, e := range entries {
			info, _ := e.Info()
			mode := "-rw-r--r--"
			if e.IsDir() {
				mode = "drwxr-xr-x"
			}
			out = append(out, sandbox.FileEntry{Path: path.Join(p, e.Name()), Size: info.Size(), Mode: mode, NLink: 1})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, r *http.Request) {
		calls++
		lp, ok := local(r.URL.Query().Get("path"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		st, err := os.Stat(lp)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if st.IsDir() {
			http.Error(w, "Cannot download a directory.\n", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, lp)
	})
	mux.HandleFunc("/files/read", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("/files/read was called for %s; artifacts go through /files/download (medios#582)", r.URL.Query().Get("path"))
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return sandbox.Node{ID: "N-1", Host: u.Hostname(), Port: port}, &sandbox.Resolver{}, &calls
}

// binaryArtifact has every byte class the wire can mangle: quotes,
// backslashes, control bytes, a valid multi-byte rune, and bytes that are
// not valid UTF-8.
var binaryArtifact = []byte("quote\" back\\ nl\n tab\t ctl\x01\x1f \xc3\xa9 \xff\xfe\x80 end")

func nodeAttempt(t *testing.T) (string, string, *Manifest) {
	t.Helper()
	root, dir, m := attempt(t)
	if err := os.WriteFile(filepath.Join(dir, "ckpt", "last.pt"), binaryArtifact, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	return root, dir, m
}

func TestNodeReader_VerifyHashesBinaryThroughDownload(t *testing.T) {
	root, _, m := nodeAttempt(t)
	node, res, calls := fakeFilesNode(t, root, "results")
	r := NodeReader{Resolver: res, Node: node, Store: "results", Base: "cue-gen-20261010/train-tabicl/a1"}
	got, err := VerifyFrom(context.Background(), r, ManifestName)
	if err != nil || !got.Complete() || !got.Hashed {
		t.Fatalf("verify over the node: %v %+v", err, got)
	}
	if got.Manifest.Artifacts[1].SHA256 != m.Artifacts[1].SHA256 {
		t.Fatal("manifest not read through the node")
	}
	if *calls == 0 {
		t.Fatal("nothing went through the node")
	}
	if len(got.Undeclared) != 0 {
		t.Fatalf("undeclared = %v", got.Undeclared)
	}
}

func TestNodeReader_StorePathAndNotFound(t *testing.T) {
	root, _, _ := nodeAttempt(t)
	node, res, _ := fakeFilesNode(t, root, "results")
	if got := node.StorePathFor("results", "ns/job/a1/manifest.json"); got != "/stores/results/ns/job/a1/manifest.json" {
		t.Fatalf("StorePathFor = %s", got)
	}
	if got := node.StorePathFor("results", ""); got != "/stores/results" {
		t.Fatalf("StorePathFor root = %s", got)
	}
	r := NodeReader{Resolver: res, Node: node, Store: "other-store", Base: "cue-gen-20261010/train-tabicl/a1"}
	if _, err := VerifyFrom(context.Background(), r, ManifestName); err == nil || !strings.Contains(err.Error(), "/stores/other-store/") {
		t.Fatalf("a store the node does not attach must error with the node path: %v", err)
	}
	r.Store = "results"
	if _, err := r.Stat(context.Background(), "nope.txt"); err == nil || !strings.Contains(err.Error(), "file does not exist") {
		t.Fatalf("missing file must wrap fs.ErrNotExist: %v", err)
	}
}

func TestNodeReader_VerifyAndCheckReportMissingAndMismatch(t *testing.T) {
	root, dir, _ := nodeAttempt(t)
	node, res, _ := fakeFilesNode(t, root, "results")
	r := NodeReader{Resolver: res, Node: node, Store: "results", Base: "cue-gen-20261010/train-tabicl/a1"}
	ctx := context.Background()

	// Same length, other bytes: Check passes, Verify catches it.
	os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(`{"loss":0.99}`), 0o644)
	chk, err := Check(ctx, r, ManifestName)
	if err != nil || !chk.Complete() || chk.Hashed || chk.String() != "present" {
		t.Fatalf("check: %v %+v", err, chk)
	}
	ver, _ := VerifyFrom(ctx, r, ManifestName)
	if ver.Outcome != HashMismatch || ver.Artifact != "metrics" {
		t.Fatalf("verify: %+v", ver)
	}

	// Wrong length: Check catches it without hashing.
	os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(`{"loss":0.123}`), 0o644)
	chk, _ = Check(ctx, r, ManifestName)
	if chk.Outcome != SizeMismatch || chk.String() != "size mismatch metrics" {
		t.Fatalf("check size: %+v", chk)
	}

	// Gone: both report missing.
	os.Remove(filepath.Join(dir, "ckpt", "last.pt"))
	os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(`{"loss":0.12}`), 0o644)
	chk, _ = Check(ctx, r, ManifestName)
	ver, _ = VerifyFrom(ctx, r, ManifestName)
	if chk.Outcome != Missing || ver.Outcome != Missing || chk.Artifact != "checkpoint" {
		t.Fatalf("missing: check %+v verify %+v", chk, ver)
	}

	// A stray temp file shows up as undeclared through the node too.
	os.WriteFile(filepath.Join(dir, ManifestName+".9.tmp"), []byte("{"), 0o644)
	chk, _ = Check(ctx, r, ManifestName)
	if len(chk.Undeclared) != 1 || chk.Undeclared[0] != ManifestName+".9.tmp" {
		t.Fatalf("undeclared: %v", chk.Undeclared)
	}
}

func TestNodeReader_ListOverTheStore(t *testing.T) {
	root, _, _ := nodeAttempt(t)
	os.MkdirAll(filepath.Join(root, "cue-gen-20261010", "train-tabicl", "a2"), 0o755)
	os.MkdirAll(filepath.Join(root, "bad name", "j", "a"), 0o755)
	node, res, _ := fakeFilesNode(t, root, "results")
	r := NodeReader{Resolver: res, Node: node, Store: "results"}
	list, err := ListFrom(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("entries = %+v", list)
	}
	if list[0].Malformed == "" || list[0].Dir != "/stores/results/bad name" {
		t.Fatalf("malformed = %+v", list[0])
	}
	if !list[1].Committed || list[1].ManifestPath != "/stores/results/cue-gen-20261010/train-tabicl/a1/manifest.json" {
		t.Fatalf("a1 = %+v", list[1])
	}
	if list[2].Committed || list[2].Attempt != "a2" {
		t.Fatalf("a2 = %+v", list[2])
	}
}

// Check on the local reader is the same cheap half, and a hashed pass
// renders differently from it.
func TestCheck_LocalReader(t *testing.T) {
	_, dir, _ := nodeAttempt(t)
	res, err := Check(context.Background(), LocalReader{Root: dir}, ManifestName)
	if err != nil || !res.Complete() || res.Hashed || res.String() != "present" {
		t.Fatalf("%v %+v", err, res)
	}
	ver, err := VerifyFrom(context.Background(), LocalReader{Root: dir}, ManifestName)
	if err != nil || !ver.Complete() || !ver.Hashed || ver.String() != "complete" {
		t.Fatalf("%v %+v", err, ver)
	}
}

// Both readers refuse a path that leaves the store before any file or
// request is touched: the refusal, not a not-found, and no call to the
// node.
func TestReaders_RefuseAPathThatEscapes(t *testing.T) {
	root, dir, _ := nodeAttempt(t)
	node, res, calls := fakeFilesNode(t, root, "results")
	readers := map[string]Reader{
		"local": LocalReader{Root: dir},
		"node":  NodeReader{Resolver: res, Node: node, Store: "results", Base: "cue-gen-20261010/train-tabicl/a1"},
	}
	escapes := []string{"../../../../etc/passwd", "ckpt/../../x", "/etc/passwd", "..", "a/../.."}
	ctx := context.Background()
	for name, r := range readers {
		for _, p := range escapes {
			want := "climbs out of the attempt directory"
			if strings.HasPrefix(p, "/") {
				want = "is absolute"
			}
			if _, err := r.Open(ctx, p); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s Open(%q): %v", name, p, err)
			}
			if _, err := r.Stat(ctx, p); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s Stat(%q): %v", name, p, err)
			}
			if _, err := r.List(ctx, p); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s List(%q): %v", name, p, err)
			}
		}
	}
	if *calls != 0 {
		t.Fatalf("the node was asked %d times for an escaping path", *calls)
	}
	if _, err := VerifyFrom(ctx, readers["node"], "../../a2/"+ManifestName); err == nil || !strings.Contains(err.Error(), "climbs out") {
		t.Fatalf("VerifyFrom with a climbing manifest path: %v", err)
	}
}
