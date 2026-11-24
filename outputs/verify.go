package outputs

import (
	"context"
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
)

// Entry is one directory found under a store: an attempt, with or without
// a committed manifest, or a directory whose name is not a valid component
// and so cannot be part of the layout. An uncommitted attempt is listed so
// a framework reconciling after a restart can see a publish that never
// finished; a malformed name is listed so a reconciler never concludes
// "no outputs here" from a short list.
type Entry struct {
	Namespace string
	Job       string
	Attempt   string
	// Dir is the directory as the reader names it (Reader.Path).
	// ManifestPath is set when manifest.json is present; Committed says
	// so.
	Dir          string
	ManifestPath string
	Committed    bool
	// Malformed is set, with the reason, on a directory whose name is not a
	// valid component. Its fields above hold what was read up to it and it
	// is not descended into.
	Malformed string
}

// List walks the store mounted at root; see ListFrom.
func List(root string) ([]Entry, error) {
	if root == "" {
		return nil, errors.New("root is required: the store's mount")
	}
	if st, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("root %s: %w", root, err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("root %s is not a directory", root)
	}
	return ListFrom(context.Background(), LocalReader{Root: root})
}

// ListFrom walks the reader's root exactly three levels deep,
// namespace/job/attempt, and returns every attempt directory, sorted by
// path, plus every directory at any of the three levels whose name is not
// a valid component. Files are not entries.
func ListFrom(ctx context.Context, r Reader) ([]Entry, error) {
	var out []Entry
	namespaces, bad, err := subdirs(ctx, r, "", "namespace")
	if err != nil {
		return nil, err
	}
	for _, b := range bad {
		out = append(out, Entry{Dir: r.Path(b.name), Malformed: b.why})
	}
	for _, ns := range namespaces {
		jobs, bad, err := subdirs(ctx, r, ns, "job")
		if err != nil {
			return nil, err
		}
		for _, b := range bad {
			out = append(out, Entry{Namespace: ns, Dir: r.Path(path.Join(ns, b.name)), Malformed: b.why})
		}
		for _, job := range jobs {
			attempts, bad, err := subdirs(ctx, r, path.Join(ns, job), "attempt")
			if err != nil {
				return nil, err
			}
			for _, b := range bad {
				out = append(out, Entry{Namespace: ns, Job: job, Dir: r.Path(path.Join(ns, job, b.name)), Malformed: b.why})
			}
			for _, attempt := range attempts {
				rel := path.Join(ns, job, attempt)
				e := Entry{Namespace: ns, Job: job, Attempt: attempt, Dir: r.Path(rel)}
				mp := path.Join(rel, ManifestName)
				if _, err := r.Stat(ctx, mp); err == nil {
					e.ManifestPath, e.Committed = r.Path(mp), true
				} else if !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("stat %s: %w", r.Path(mp), err)
				}
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

type malformedDir struct{ name, why string }

// subdirs splits a directory's subdirectories into valid components and
// malformed names, each with the reason.
func subdirs(ctx context.Context, r Reader, rel, field string) ([]string, []malformedDir, error) {
	entries, err := r.List(ctx, rel)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", r.Path(rel), err)
	}
	var names []string
	var bad []malformedDir
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		if err := ValidateComponent(field, e.Name); err != nil {
			bad = append(bad, malformedDir{e.Name, err.Error()})
			continue
		}
		names = append(names, e.Name)
	}
	return names, bad, nil
}

// Outcome is what Verify or Check found.
type Outcome string

const (
	// Complete: the manifest is well formed, sits where it says, and every
	// artifact it names is present with the recorded size, and, from
	// Verify, the recorded hash. It is a claim about the manifest's
	// artifacts, not about the directory: files the manifest does not name
	// are reported in Result.Undeclared and do not change the outcome.
	Complete Outcome = "complete"
	// Missing: an artifact the manifest names is not on disk.
	Missing Outcome = "missing"
	// HashMismatch: an artifact's bytes do not hash to the recorded sha256.
	HashMismatch Outcome = "hash mismatch"
	// SizeMismatch: an artifact's length is not the recorded bytes.
	SizeMismatch Outcome = "size mismatch"
	// Invalid: the manifest cannot be read, fails validation, or is not
	// where its identity says.
	Invalid Outcome = "invalid"
)

// Result is Verify's or Check's answer. Artifact names the first artifact
// that failed, Detail says how, and Manifest is the decoded receipt when it
// parsed. Hashed says whether the artifacts' bytes were hashed (Verify) or
// only their presence and size confirmed (Check). Undeclared lists files
// under the attempt directory that the manifest does not name, relative
// paths with forward slashes, in sorted order: a half-written artifact, a
// stray manifest.json.*.tmp from an interrupted publish, output the writer
// forgot to declare. The outcome does not depend on them.
type Result struct {
	Outcome    Outcome
	Artifact   string
	Detail     string
	Manifest   *Manifest
	Hashed     bool
	Undeclared []string
}

// Complete reports whether every check passed.
func (r Result) Complete() bool { return r.Outcome == Complete }

// String renders the result as "complete" from Verify or "present" from
// Check, so a log line tells a hashed pass from a presence-and-size pass;
// otherwise "missing <name>", "hash mismatch <name>", "size mismatch
// <name>" or "invalid: <detail>".
func (r Result) String() string {
	switch r.Outcome {
	case Complete:
		if !r.Hashed {
			return "present"
		}
		return string(Complete)
	case Invalid:
		return fmt.Sprintf("%s: %s", Invalid, r.Detail)
	default:
		return fmt.Sprintf("%s %s", r.Outcome, r.Artifact)
	}
}

// Verify checks a committed manifest on the local filesystem; see
// VerifyFrom.
func Verify(manifestPath string) (Result, error) {
	dir, name := filepath.Split(manifestPath)
	return VerifyFrom(context.Background(), LocalReader{Root: filepath.Clean(dir)}, name)
}

// VerifyFrom reads the manifest at manifestRel under the reader's root,
// which is the attempt directory, and checks every artifact it names:
// present, the recorded length, the recorded sha256, hashing the bytes
// through the reader. The first failure is the result; artifacts are
// checked in manifest order. Files the manifest does not name are listed
// in Undeclared and do not affect the outcome. An unreadable or malformed
// manifest is Invalid rather than an error, so a caller listing a store
// gets one Result per attempt and never a panic on a bad file; only a
// manifest that cannot be opened at all returns an error.
func VerifyFrom(ctx context.Context, r Reader, manifestRel string) (Result, error) {
	return check(ctx, r, manifestRel, true)
}

// Check is the cheap half of Verify: it confirms the manifest is
// committed, well formed and where its identity says, and that every
// declared artifact exists with its declared byte count, without reading
// the artifacts' bytes. Its Complete says present and sized, not intact;
// Result.Hashed is false. Use it to triage a store before paying for
// VerifyFrom.
func Check(ctx context.Context, r Reader, manifestRel string) (Result, error) {
	return check(ctx, r, manifestRel, false)
}

func check(ctx context.Context, r Reader, manifestRel string, hash bool) (Result, error) {
	mf, err := r.Open(ctx, manifestRel)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", r.Path(manifestRel), err)
	}
	raw, err := io.ReadAll(mf)
	mf.Close()
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", r.Path(manifestRel), err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Result{Outcome: Invalid, Hashed: hash, Detail: fmt.Sprintf("%s does not parse: %v", r.Path(manifestRel), err)}, nil
	}
	if err := m.validate(); err != nil {
		return Result{Outcome: Invalid, Hashed: hash, Detail: err.Error(), Manifest: &m}, nil
	}
	dirRel := path.Dir(manifestRel)
	if dirRel == "." {
		dirRel = ""
	}
	if err := checkLocation(r.Path(dirRel), &m); err != nil {
		return Result{Outcome: Invalid, Hashed: hash, Detail: err.Error(), Manifest: &m}, nil
	}
	undeclared, err := undeclaredFiles(ctx, r, dirRel, &m)
	if err != nil {
		return Result{}, err
	}
	res := Result{Manifest: &m, Hashed: hash, Undeclared: undeclared}
	for _, a := range m.Artifacts {
		if a.SHA256 == "" || a.Bytes == nil {
			res.Outcome, res.Artifact, res.Detail = Invalid, a.Name, fmt.Sprintf("artifact %q has no sha256 or bytes", a.Name)
			return res, nil
		}
		rel := path.Join(dirRel, a.Path)
		size, err := r.Stat(ctx, rel)
		if errors.Is(err, fs.ErrNotExist) {
			res.Outcome, res.Artifact, res.Detail = Missing, a.Name, r.Path(rel)
			return res, nil
		}
		if err != nil {
			return Result{}, fmt.Errorf("stat %s: %w", r.Path(rel), err)
		}
		if size != *a.Bytes {
			res.Outcome, res.Artifact, res.Detail = SizeMismatch, a.Name, fmt.Sprintf("recorded %d bytes, found %d", *a.Bytes, size)
			return res, nil
		}
		if !hash {
			continue
		}
		sum, n, err := digestFrom(ctx, r, rel)
		if err != nil {
			return Result{}, err
		}
		if n != *a.Bytes {
			res.Outcome, res.Artifact, res.Detail = SizeMismatch, a.Name, fmt.Sprintf("recorded %d bytes, read %d", *a.Bytes, n)
			return res, nil
		}
		if sum != a.SHA256 {
			res.Outcome, res.Artifact, res.Detail = HashMismatch, a.Name, fmt.Sprintf("recorded %s, found %s", a.SHA256, sum)
			return res, nil
		}
	}
	res.Outcome = Complete
	return res, nil
}

// digestFrom hashes a file through the reader.
func digestFrom(ctx context.Context, r Reader, rel string) (string, int64, error) {
	f, err := r.Open(ctx, rel)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", r.Path(rel), err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", r.Path(rel), err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// undeclaredFiles walks the attempt directory through the reader and
// returns every file the manifest does not name, the manifest itself
// excepted, as paths relative to the attempt directory.
func undeclaredFiles(ctx context.Context, r Reader, dirRel string, m *Manifest) ([]string, error) {
	declared := map[string]bool{ManifestName: true}
	for _, a := range m.Artifacts {
		declared[a.Path] = true
	}
	var out []string
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := r.List(ctx, path.Join(dirRel, rel))
		if err != nil {
			return fmt.Errorf("read %s: %w", r.Path(path.Join(dirRel, rel)), err)
		}
		for _, e := range entries {
			p := path.Join(rel, e.Name)
			if e.IsDir {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			if !declared[p] {
				out = append(out, p)
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
