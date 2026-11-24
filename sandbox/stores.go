package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
)

// Stores on the node's files API.
//
// The node attaches every declared store read-only at StoresRoot/<name>
// (medios#585), on the same files routes the sandbox is read from. A reader on another machine lists and hashes an attempt's outputs
// through the node without a mount of its own: Browse for presence and
// size, Download for the bytes. Until medios#585 lands on a node, nothing
// here has been exercised against one; the fake node in the tests is the
// contract these calls are written to.
//
// Bytes go through /files/download, which streams the file as
// application/octet-stream, and not through /files/read, whose JSON string
// carries file bytes verbatim and corrupts a binary file silently
// (medios#582).

// StoresRoot is the virtual directory the node attaches stores under.
const StoresRoot = "/stores"

// StorePathFor returns the node-side path of a file inside a declared
// store, the value sent as ?path= to the files routes. relative is a
// forward-slash path inside the store; empty names the store's root.
func (n Node) StorePathFor(store, relative string) string {
	return path.Join(StoresRoot, store, relative)
}

// FileEntry is one entry of a /files/browse listing, as the node models a
// FileInfo (src/common/http.cpp): Path is the node-side path, Mode the
// ls-style string whose first character is the type ('d' for a
// directory, '-' for a regular file).
type FileEntry struct {
	Path  string  `json:"path"`
	Size  int64   `json:"size"`
	Mode  string  `json:"mode"`
	NLink int     `json:"nlink"`
	MTime float64 `json:"mtime"`
}

// Name is the entry's last path component.
func (e FileEntry) Name() string { return path.Base(e.Path) }

// IsDir reports whether the entry is a directory.
func (e FileEntry) IsDir() bool { return len(e.Mode) > 0 && e.Mode[0] == 'd' }

// Browse lists a directory on the node through /files/browse. Errors as
// Read: ErrNotFound when the node has no such path, ErrNodeUnreachable on
// a transport failure, which also drops the node's cached location.
func (r *Resolver) Browse(ctx context.Context, n Node, dirPath string) ([]FileEntry, error) {
	resp, err := r.filesGet(ctx, n, "/files/browse", dirPath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []FileEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sandbox: decode browse response: %w", err)
	}
	return out, nil
}

// Download streams a file's bytes from the node through /files/download.
// The body is the file as it is on disk, with no cap on length. The
// caller closes it; the Resolver's timeout bounds the whole transfer, so
// a Resolver that reads large artifacts sets Timeout for them. Errors as
// Browse; a directory is refused by the node with 400, reported as an
// error naming the path.
func (r *Resolver) Download(ctx context.Context, n Node, filePath string) (io.ReadCloser, error) {
	resp, err := r.filesGet(ctx, n, "/files/download", filePath)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// filesGet issues one GET on a files route with ?path= and maps the
// status as Read does. The returned body carries the bounded context's
// cancel, released on Close.
func (r *Resolver) filesGet(ctx context.Context, n Node, route, filePath string) (*http.Response, error) {
	ctx, cancel := r.bounded(ctx)
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(n.Host, strconv.Itoa(n.Port)), Path: route}
	q := u.Query()
	q.Set("path", filePath)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sandbox: new request: %w", err)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		cancel()
		r.Forget(n.ID)
		return nil, fmt.Errorf("%w: %s: %v", ErrNodeUnreachable, u.Host, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	case http.StatusNotFound:
		resp.Body.Close()
		cancel()
		return nil, ErrNotFound
	default:
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("sandbox: node returned %d for %s %s", resp.StatusCode, route, filePath)
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
