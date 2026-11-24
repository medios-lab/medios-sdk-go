package outputs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// AttemptDir is the attempt's directory under the store's mount.
func AttemptDir(root, namespace, job, attempt string) string {
	return filepath.Join(root, namespace, job, attempt)
}

// Publish commits the manifest for the attempt directory dir. The artifacts
// must already be in place: Publish hashes each one from disk and fsyncs
// it, fills in or checks sha256 and bytes, stamps published_at, fsyncs the
// attempt directory, and writes manifest.json last (with mode 0644) by
// linking a temporary file into place, so a manifest already committed by
// another writer is never overwritten and a committed manifest is never
// durable before the files it names. The directory's last three components
// must be the manifest's namespace, job and attempt, which binds the receipt
// to its location.
//
// On success m is updated with the computed hashes, sizes and the publish
// time. On refusal m is left exactly as the caller passed it. Every refusal
// is an error with a sentence naming the field; nothing is defaulted or
// guessed.
func Publish(dir string, m *Manifest) error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if dir == "" {
		return errors.New("attempt directory is required")
	}
	committed := filepath.Join(dir, ManifestName)
	if _, err := os.Lstat(committed); err == nil {
		return fmt.Errorf("%s already exists: attempt %s/%s/%s is committed and a second writer never overwrites it",
			committed, m.Namespace, m.Job, m.Attempt)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", committed, err)
	}

	// Work on a copy so a refusal leaves the caller's manifest untouched.
	out := *m
	out.Artifacts = append([]Artifact(nil), m.Artifacts...)
	out.PublishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := out.validate(); err != nil {
		return err
	}
	if err := checkLocation(dir, &out); err != nil {
		return err
	}
	for i := range out.Artifacts {
		a := &out.Artifacts[i]
		sum, n, err := digest(filepath.Join(dir, filepath.FromSlash(a.Path)), true)
		if err != nil {
			return fmt.Errorf("artifact %q: %w", a.Name, err)
		}
		if a.SHA256 != "" && a.SHA256 != sum {
			return fmt.Errorf("artifact %q: declared sha256 %s but the file hashes to %s", a.Name, a.SHA256, sum)
		}
		if a.Bytes != nil && *a.Bytes != n {
			return fmt.Errorf("artifact %q: declared %d bytes but the file is %d", a.Name, *a.Bytes, n)
		}
		a.SHA256, a.Bytes = sum, &n
	}
	body, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, ManifestName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary manifest in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	// The artifacts' data and the directory entries that name them are
	// durable before the commit exists, and the commit's entry is durable
	// before Publish returns: every distinct parent directory of a declared
	// artifact, deepest first, then the attempt directory.
	for _, parent := range artifactParents(dir, out.Artifacts) {
		if err := fsyncDir(parent); err != nil {
			return err
		}
	}
	if err := fsyncDir(dir); err != nil {
		return err
	}
	// link, not rename: rename replaces an existing target, link refuses one.
	// That is the second-writer rule, and it needs a filesystem with hard
	// links (see the package doc).
	if err := os.Link(tmpName, committed); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: attempt %s/%s/%s was committed by another writer first",
				committed, m.Namespace, m.Job, m.Attempt)
		}
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("commit %s: the store's filesystem does not support hard links (%v); "+
				"Publish needs them to refuse a second writer, so declare the store on a filesystem that has them",
				committed, err)
		}
		return fmt.Errorf("commit %s: %w", committed, err)
	}
	if err := fsyncDir(dir); err != nil {
		return err
	}
	*m = out
	return nil
}

// artifactParents is every directory under dir that holds a declared
// artifact, each once, deepest first, the attempt directory itself
// excluded.
func artifactParents(dir string, artifacts []Artifact) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range artifacts {
		rel := path.Dir(a.Path)
		for rel != "." && rel != "" {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			rel = path.Dir(rel)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], string(filepath.Separator)), strings.Count(out[j], string(filepath.Separator))
		if di != dj {
			return di > dj
		}
		return out[i] < out[j]
	})
	return out
}

// fsyncDir is a variable so a test can record which directories were
// synced and in what order.
var fsyncDir = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// checkLocation refuses a manifest whose identity is not where it is being
// written, so a manifest can never be found under one attempt and claim
// another.
func checkLocation(dir string, m *Manifest) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	attempt := filepath.Base(abs)
	job := filepath.Base(filepath.Dir(abs))
	namespace := filepath.Base(filepath.Dir(filepath.Dir(abs)))
	if namespace != m.Namespace || job != m.Job || attempt != m.Attempt {
		return fmt.Errorf("directory %s ends in %s/%s/%s but the manifest is for %s/%s/%s",
			dir, namespace, job, attempt, m.Namespace, m.Job, m.Attempt)
	}
	return nil
}

// digest hashes the file and, when sync is set, fsyncs it through the same
// descriptor so the bytes that were hashed are the bytes on disk.
func digest(path string, sync bool) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", 0, fmt.Errorf("file %s is missing; artifacts are written before the manifest", path)
		}
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", path, err)
	}
	if sync {
		if err := f.Sync(); err != nil {
			return "", 0, fmt.Errorf("sync %s: %w", path, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
