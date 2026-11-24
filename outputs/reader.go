package outputs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/medios-lab/medios-sdk-go/sandbox"
)

// Reader is what List, Verify and Check need from a store: open a file for
// reading, its size, and a directory listing, all by forward-slash path
// relative to the reader's root. Two implementations: the local filesystem
// where the store is mounted, and the node's files API where it is not.
type Reader interface {
	// Open returns the file's bytes. A missing file is an error that
	// wraps fs.ErrNotExist.
	Open(ctx context.Context, relative string) (io.ReadCloser, error)
	// Stat returns the file's size in bytes; a missing file wraps
	// fs.ErrNotExist.
	Stat(ctx context.Context, relative string) (int64, error)
	// List returns the directory's entries, name and kind and size.
	List(ctx context.Context, relative string) ([]DirEntry, error)
	// Path names relative the way this reader's backing names it, for
	// messages and for Entry.Dir: an absolute local path, or the node-side
	// path under /stores.
	Path(relative string) string
}

// DirEntry is one entry of a Reader's listing.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// checkRelative refuses a relative path that could leave the reader's
// root before any file or request is touched: an absolute path, or any
// component that climbs. Empty names the root itself.
func checkRelative(relative string) error {
	if relative == "" {
		return nil
	}
	if strings.HasPrefix(relative, "/") {
		return fmt.Errorf("path %q is absolute; want a path relative to the attempt directory", relative)
	}
	for _, seg := range strings.Split(relative, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q climbs out of the attempt directory", relative)
		}
	}
	if cleaned := path.Clean(relative); cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("path %q climbs out of the attempt directory", relative)
	}
	return nil
}

// LocalReader reads a store mounted on this machine.
type LocalReader struct {
	// Root is the directory the relative paths are under: the store's
	// mount for List, the attempt directory for Verify and Check.
	Root string
}

func (l LocalReader) Open(_ context.Context, relative string) (io.ReadCloser, error) {
	if err := checkRelative(relative); err != nil {
		return nil, err
	}
	f, err := os.Open(l.Path(relative))
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l LocalReader) Stat(_ context.Context, relative string) (int64, error) {
	if err := checkRelative(relative); err != nil {
		return 0, err
	}
	st, err := os.Stat(l.Path(relative))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (l LocalReader) List(_ context.Context, relative string) ([]DirEntry, error) {
	if err := checkRelative(relative); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(l.Path(relative))
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		d := DirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				d.Size = info.Size()
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (l LocalReader) Path(relative string) string {
	return filepath.Join(l.Root, filepath.FromSlash(relative))
}

// NodeReader reads a store through the node that has it mounted, over the
// files API the node attaches every declared store to (sandbox.StoresRoot):
// Browse for presence and size, Download for bytes. Read is not used, see
// medios#582.
type NodeReader struct {
	Resolver *sandbox.Resolver
	Node     sandbox.Node
	// Store is the declared store's name and Base the forward-slash path
	// inside it that relative paths are under: empty for List over the
	// whole store, the attempt's namespace/job/attempt for Verify and
	// Check.
	Store string
	Base  string
}

func (n NodeReader) Path(relative string) string {
	return n.Node.StorePathFor(n.Store, path.Join(n.Base, relative))
}

func (n NodeReader) Open(ctx context.Context, relative string) (io.ReadCloser, error) {
	if err := checkRelative(relative); err != nil {
		return nil, err
	}
	p := n.Path(relative)
	body, err := n.Resolver.Download(ctx, n.Node, p)
	if err != nil {
		return nil, mapNotFound(err, p)
	}
	return body, nil
}

// Stat finds the file in its parent's listing: the files API has no stat
// route, and Browse reports each entry's size.
func (n NodeReader) Stat(ctx context.Context, relative string) (int64, error) {
	if err := checkRelative(relative); err != nil {
		return 0, err
	}
	p := n.Path(relative)
	entries, err := n.Resolver.Browse(ctx, n.Node, path.Dir(p))
	if err != nil {
		return 0, mapNotFound(err, p)
	}
	name := path.Base(p)
	for _, e := range entries {
		if e.Name() == name && !e.IsDir() {
			return e.Size, nil
		}
	}
	return 0, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
}

func (n NodeReader) List(ctx context.Context, relative string) ([]DirEntry, error) {
	if err := checkRelative(relative); err != nil {
		return nil, err
	}
	p := n.Path(relative)
	entries, err := n.Resolver.Browse(ctx, n.Node, p)
	if err != nil {
		return nil, mapNotFound(err, p)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: e.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func mapNotFound(err error, p string) error {
	if errors.Is(err, sandbox.ErrNotFound) {
		return fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	return err
}
