package outputs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attempt lays out a store root with one attempt directory holding two
// artifacts and returns the directory and a manifest describing them.
func attempt(t *testing.T) (string, string, *Manifest) {
	t.Helper()
	root := t.TempDir()
	dir := AttemptDir(root, "cue-gen-20261010", "train-tabicl", "a1")
	if err := os.MkdirAll(filepath.Join(dir, "ckpt"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "metrics.json"), `{"loss":0.12}`)
	write(t, filepath.Join(dir, "ckpt", "last.pt"), "weights")
	return root, dir, manifest()
}

func int32p(n int32) *int32 { return &n }
func int64p(n int64) *int64 { return &n }

func manifest() *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersion,
		Namespace:     "cue-gen-20261010", Job: "train-tabicl", Attempt: "a1",
		FrameworkID: "fw-1", TaskID: "cue.train-tabicl.a1", NodeID: "S0",
		Submitter: "team:research",
		Work:      WorkOutcome{ExitStatus: int32p(0)},
		Finish:    FinishOutcome{State: FinishRan, ExitCode: int32p(0)},
		StartedAt: "2026-10-10T14:00:00Z", FinishedAt: "2026-10-10T14:20:00Z",
		Artifacts: []Artifact{
			{Name: "metrics", Path: "metrics.json", MediaType: "application/json"},
			{Name: "checkpoint", Path: "ckpt/last.pt", MediaType: "application/octet-stream"},
		},
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestPublish_CommitsAManifestTheVerifierAccepts(t *testing.T) {
	root, dir, m := attempt(t)
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	if m.Artifacts[0].SHA256 != sum(`{"loss":0.12}`) || m.Artifacts[0].Bytes == nil || *m.Artifacts[0].Bytes != 13 {
		t.Fatalf("metrics not hashed: %+v", m.Artifacts[0])
	}
	if m.PublishedAt == "" {
		t.Fatal("published_at not stamped")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o644 {
		t.Fatalf("manifest.json mode = %#o, want 0644", got)
	}
	for _, want := range []string{
		`"schema_version": 1`, `"namespace": "cue-gen-20261010"`, `"submitter": "team:research"`,
		`"work": {`, `"exit_status": 0`, `"state": "RAN"`, `"exit_code": 0`, `"path": "ckpt/last.pt"`,
		`"sha256": "` + sum("weights") + `"`, `"bytes": 7`, `"media_type": "application/octet-stream"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("manifest lacks %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), root) {
		t.Fatalf("a host path leaked into the manifest:\n%s", raw)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("temporary file left behind: %v", entries)
	}

	res, err := Verify(filepath.Join(dir, ManifestName))
	if err != nil || !res.Complete() || res.String() != "complete" || len(res.Undeclared) != 0 {
		t.Fatalf("verify: %v %+v", err, res)
	}
	list, err := List(root)
	if err != nil || len(list) != 1 || !list[0].Committed || list[0].Job != "train-tabicl" {
		t.Fatalf("list: %v %+v", err, list)
	}
}

// Every parent directory of a declared artifact is fsynced, deepest first,
// then the attempt directory, before the manifest is linked; and the
// attempt directory again after.
func TestPublish_SyncsEveryArtifactParentBeforeTheCommit(t *testing.T) {
	_, dir, m := attempt(t)
	os.MkdirAll(filepath.Join(dir, "ckpt", "deep", "er"), 0o755)
	write(t, filepath.Join(dir, "ckpt", "deep", "er", "x.bin"), "x")
	m.Artifacts = append(m.Artifacts, Artifact{Name: "x", Path: "ckpt/deep/er/x.bin", MediaType: "application/octet-stream"})

	var synced []string
	linkedAt := -1
	orig := fsyncDir
	fsyncDir = func(p string) error {
		if _, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil && linkedAt < 0 {
			linkedAt = len(synced)
		}
		synced = append(synced, p)
		return orig(p)
	}
	defer func() { fsyncDir = orig }()

	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "ckpt", "deep", "er"),
		filepath.Join(dir, "ckpt", "deep"),
		filepath.Join(dir, "ckpt"),
		dir,
		dir,
	}
	if strings.Join(synced, "\n") != strings.Join(want, "\n") {
		t.Fatalf("synced:\n%s\nwant:\n%s", strings.Join(synced, "\n"), strings.Join(want, "\n"))
	}
	if linkedAt != 4 {
		t.Fatalf("the manifest was linked before the parents were synced (linked at sync %d)", linkedAt)
	}
}

func TestPublish_SecondWriterForTheAttemptFails(t *testing.T) {
	_, dir, m := attempt(t)
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, ManifestName))
	again := manifest()
	again.TaskID = "cue.train-tabicl.a1-retry"
	err := Publish(dir, again)
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("err = %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, ManifestName))
	if string(first) != string(second) {
		t.Fatal("the committed manifest was overwritten")
	}
}

func TestPublish_RefusesAMissingArtifact(t *testing.T) {
	_, dir, m := attempt(t)
	m.Artifacts = append(m.Artifacts, Artifact{Name: "log", Path: "train.log", MediaType: "text/plain"})
	err := Publish(dir, m)
	if err == nil || !strings.Contains(err.Error(), `artifact "log": file`) || !strings.Contains(err.Error(), "written before the manifest") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil {
		t.Fatal("a manifest was committed with a missing artifact")
	}
}

func TestPublish_RefusesADeclaredHashOrSizeThatDisagrees(t *testing.T) {
	_, dir, m := attempt(t)
	m.Artifacts[0].SHA256 = sum("other")
	if err := Publish(dir, m); err == nil || !strings.Contains(err.Error(), "declared sha256") {
		t.Fatalf("err = %v", err)
	}
	_, dir, m = attempt(t)
	m.Artifacts[1].Bytes = int64p(99)
	if err := Publish(dir, m); err == nil || !strings.Contains(err.Error(), "declared 99 bytes but the file is 7") {
		t.Fatalf("err = %v", err)
	}
	// Zero is a declared size, not an absence: it disagrees with a 7-byte file.
	_, dir, m = attempt(t)
	m.Artifacts[1].Bytes = int64p(0)
	if err := Publish(dir, m); err == nil || !strings.Contains(err.Error(), "declared 0 bytes but the file is 7") {
		t.Fatalf("declared zero: err = %v", err)
	}
	// And an empty artifact publishes with bytes 0 recorded.
	_, dir, m = attempt(t)
	write(t, filepath.Join(dir, "empty.txt"), "")
	m.Artifacts = append(m.Artifacts, Artifact{Name: "empty", Path: "empty.txt", MediaType: "text/plain"})
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	if m.Artifacts[2].Bytes == nil || *m.Artifacts[2].Bytes != 0 {
		t.Fatalf("empty artifact: %+v", m.Artifacts[2])
	}
}

// A refused publish leaves the caller's manifest exactly as it was.
func TestPublish_RefusalLeavesTheManifestUntouched(t *testing.T) {
	_, dir, m := attempt(t)
	m.Artifacts = append(m.Artifacts, Artifact{Name: "log", Path: "train.log", MediaType: "text/plain"})
	before, _ := json.Marshal(m)
	if err := Publish(dir, m); err == nil {
		t.Fatal("expected a refusal")
	}
	after, _ := json.Marshal(m)
	if string(before) != string(after) {
		t.Fatalf("manifest changed by a refused publish:\n%s\n%s", before, after)
	}
	if m.PublishedAt != "" || m.Artifacts[0].SHA256 != "" {
		t.Fatalf("partly filled in: %+v", m)
	}
}

func TestPublish_RefusesADirectoryThatIsNotTheManifestsAttempt(t *testing.T) {
	root, _, m := attempt(t)
	other := AttemptDir(root, "cue-gen-20261010", "train-tabicl", "a2")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Publish(other, m)
	if err == nil || !strings.Contains(err.Error(), "ends in cue-gen-20261010/train-tabicl/a2 but the manifest is for cue-gen-20261010/train-tabicl/a1") {
		t.Fatalf("err = %v", err)
	}
}

func TestPublish_RefusesEveryMalformedField(t *testing.T) {
	cases := []struct {
		name string
		edit func(m *Manifest)
		want string
	}{
		{"schema version", func(m *Manifest) { m.SchemaVersion = 2 }, "schema_version 2 is not supported"},
		{"empty namespace", func(m *Manifest) { m.Namespace = "" }, "namespace is required"},
		{"namespace with slash", func(m *Manifest) { m.Namespace = "a/b" }, `namespace "a/b" is malformed`},
		{"attempt dot-dot", func(m *Manifest) { m.Attempt = ".." }, `attempt ".." is malformed`},
		{"empty framework", func(m *Manifest) { m.FrameworkID = "" }, "framework_id is required"},
		{"empty task", func(m *Manifest) { m.TaskID = "" }, "task_id is required"},
		{"empty node", func(m *Manifest) { m.NodeID = "" }, "node_id is required"},
		{"empty submitter", func(m *Manifest) { m.Submitter = "" }, "submitter is required"},
		{"no work exit status", func(m *Manifest) { m.Work.ExitStatus = nil }, "work.exit_status is required"},
		{"no finish state", func(m *Manifest) { m.Finish.State = "" }, "finish.state is required"},
		{"unknown finish state", func(m *Manifest) { m.Finish.State = "done" }, `finish.state "done" is unknown`},
		{"lower-case finish state", func(m *Manifest) { m.Finish.State = "ran" }, `finish.state "ran" is unknown`},
		{"ran without exit or signal", func(m *Manifest) { m.Finish.ExitCode = nil }, "RAN carries exactly one of exit_code and signal"},
		{"ran with both", func(m *Manifest) { m.Finish.Signal = int32p(9) }, "RAN carries exactly one of exit_code and signal"},
		{"did_not_start without reason", func(m *Manifest) { m.Finish = FinishOutcome{State: FinishDidNotStart} }, "DID_NOT_START carries no reason"},
		{"lost without reason", func(m *Manifest) { m.Finish = FinishOutcome{State: FinishLost} }, "LOST carries no reason"},
		{"lost with exit code", func(m *Manifest) {
			m.Finish = FinishOutcome{State: FinishLost, Reason: "read error", ExitCode: int32p(0)}
		}, "LOST carries an exit_code or signal"},
		{"bad started_at", func(m *Manifest) { m.StartedAt = "yesterday" }, `started_at "yesterday" is not RFC 3339`},
		{"no artifacts", func(m *Manifest) { m.Artifacts = nil }, "artifacts is empty"},
		{"unnamed artifact", func(m *Manifest) { m.Artifacts[0].Name = "" }, "artifacts[0].name is required"},
		{"duplicate name", func(m *Manifest) { m.Artifacts[1].Name = "metrics" }, `artifact name "metrics" is used twice`},
		{"absolute path", func(m *Manifest) { m.Artifacts[0].Path = "/etc/passwd" }, "is absolute"},
		{"climbing path", func(m *Manifest) { m.Artifacts[0].Path = "../a1/metrics.json" }, "climbs out"},
		{"manifest as artifact", func(m *Manifest) { m.Artifacts[0].Path = ManifestName }, "is the manifest's own name"},
		{"dot segment", func(m *Manifest) { m.Artifacts[0].Path = "./metrics.json" }, "empty or dot segment"},
		{"backslash", func(m *Manifest) { m.Artifacts[0].Path = `ckpt\last.pt` }, "uses a backslash"},
		{"duplicate path", func(m *Manifest) { m.Artifacts[1].Path = "metrics.json" }, `artifact path "metrics.json" is used twice`},
		{"no media type", func(m *Manifest) { m.Artifacts[0].MediaType = "" }, "media_type is required"},
	}
	for _, c := range cases {
		_, dir, m := attempt(t)
		c.edit(m)
		err := Publish(dir, m)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
		if _, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil {
			t.Errorf("%s: a manifest was committed anyway", c.name)
		}
	}
	if err := Publish("", manifest()); err == nil || !strings.Contains(err.Error(), "attempt directory is required") {
		t.Errorf("empty dir: %v", err)
	}
	if err := Publish(t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "manifest is nil") {
		t.Errorf("nil manifest: %v", err)
	}
}

func TestVerify_ReportsTheFirstFailureByKind(t *testing.T) {
	_, dir, m := attempt(t)
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, ManifestName)

	write(t, filepath.Join(dir, "metrics.json"), `{"loss":0.99}`) // same length, other bytes
	res, err := Verify(mp)
	if err != nil || res.Outcome != HashMismatch || res.Artifact != "metrics" || res.String() != "hash mismatch metrics" {
		t.Fatalf("hash: %v %+v", err, res)
	}

	write(t, filepath.Join(dir, "metrics.json"), `{"loss":0.123}`)
	res, _ = Verify(mp)
	if res.Outcome != SizeMismatch || res.String() != "size mismatch metrics" {
		t.Fatalf("size: %+v", res)
	}

	write(t, filepath.Join(dir, "metrics.json"), `{"loss":0.12}`)
	os.Remove(filepath.Join(dir, "ckpt", "last.pt"))
	res, _ = Verify(mp)
	if res.Outcome != Missing || res.Artifact != "checkpoint" || res.String() != "missing checkpoint" {
		t.Fatalf("missing: %+v", res)
	}
}

func TestVerify_InvalidManifests(t *testing.T) {
	_, dir, m := attempt(t)
	mp := filepath.Join(dir, ManifestName)

	write(t, mp, "{not json")
	res, err := Verify(mp)
	if err != nil || res.Outcome != Invalid || !strings.Contains(res.String(), "does not parse") {
		t.Fatalf("parse: %v %+v", err, res)
	}

	m.Artifacts[0].SHA256, m.Artifacts[0].Bytes = sum(`{"loss":0.12}`), int64p(13)
	m.Artifacts[1].SHA256, m.Artifacts[1].Bytes = sum("weights"), int64p(7)
	m.PublishedAt = "2026-10-10T14:21:00Z"
	m.Attempt = "a9" // committed under a1 but claims a9
	raw, _ := json.Marshal(m)
	write(t, mp, string(raw))
	res, _ = Verify(mp)
	if res.Outcome != Invalid || !strings.Contains(res.Detail, "but the manifest is for cue-gen-20261010/train-tabicl/a9") {
		t.Fatalf("location: %+v", res)
	}

	m.Attempt = "a1"
	m.Artifacts[0].SHA256 = ""
	raw, _ = json.Marshal(m)
	write(t, mp, string(raw))
	res, _ = Verify(mp)
	if res.Outcome != Invalid || !strings.Contains(res.Detail, `artifact "metrics" has no sha256`) {
		t.Fatalf("no hash: %+v", res)
	}

	// A manifest JSON with no work object is refused, not read as exit 0.
	write(t, mp, `{"schema_version":1,"namespace":"cue-gen-20261010","job":"train-tabicl","attempt":"a1",`+
		`"framework_id":"fw","task_id":"t","node_id":"n","submitter":"team:research","finish":{"state":"RAN","exit_code":0},`+
		`"artifacts":[{"name":"metrics","path":"metrics.json","sha256":"`+sum(`{"loss":0.12}`)+`","bytes":13,"media_type":"application/json"}]}`)
	res, _ = Verify(mp)
	if res.Outcome != Invalid || !strings.Contains(res.Detail, "work.exit_status is required") {
		t.Fatalf("no work: %+v", res)
	}

	if _, err := Verify(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("an unreadable path must be an error, not a result")
	}
}

// Verify reports files the manifest does not name without changing the
// outcome: a stray temp file from an interrupted publish among them.
func TestVerify_ReportsUndeclaredFiles(t *testing.T) {
	_, dir, m := attempt(t)
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ManifestName+".123.tmp"), "{")
	os.MkdirAll(filepath.Join(dir, "ckpt", "scratch"), 0o755)
	write(t, filepath.Join(dir, "ckpt", "scratch", "partial.pt"), "x")
	res, err := Verify(filepath.Join(dir, ManifestName))
	if err != nil || !res.Complete() {
		t.Fatalf("%v %+v", err, res)
	}
	want := []string{"ckpt/scratch/partial.pt", ManifestName + ".123.tmp"}
	if strings.Join(res.Undeclared, ",") != strings.Join(want, ",") {
		t.Fatalf("undeclared = %v, want %v", res.Undeclared, want)
	}
}

func TestList_FindsCommittedAndUncommittedAttemptsOnly(t *testing.T) {
	root, dir, m := attempt(t)
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	uncommitted := AttemptDir(root, "cue-gen-20261010", "train-tabicl", "a2")
	os.MkdirAll(uncommitted, 0o755)
	os.MkdirAll(filepath.Join(root, "cue-gen-20261010", "train-tabicl", "a1", "deeper", "x"), 0o755)
	os.MkdirAll(filepath.Join(root, "bad name", "j", "a"), 0o755)
	write(t, filepath.Join(root, "stray.txt"), "x")

	os.MkdirAll(filepath.Join(root, "cue-gen-20261010", "train-tabicl", "..x"), 0o755) // valid: dots allowed
	os.MkdirAll(filepath.Join(root, "cue-gen-20261010", "bad job", "a1"), 0o755)

	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("entries = %+v", list)
	}
	// Sorted by path: "bad name" < "cue-gen-..."; inside, "bad job" < "train-tabicl".
	if list[0].Malformed == "" || list[0].Dir != filepath.Join(root, "bad name") || !strings.Contains(list[0].Malformed, `namespace "bad name" is malformed`) {
		t.Fatalf("malformed namespace = %+v", list[0])
	}
	if list[1].Malformed == "" || list[1].Namespace != "cue-gen-20261010" || !strings.Contains(list[1].Malformed, `job "bad job" is malformed`) {
		t.Fatalf("malformed job = %+v", list[1])
	}
	if list[2].Committed || list[2].Attempt != "..x" {
		t.Fatalf("..x = %+v", list[2])
	}
	if !list[3].Committed || list[3].Attempt != "a1" || list[3].ManifestPath != filepath.Join(dir, ManifestName) {
		t.Fatalf("a1 = %+v", list[3])
	}
	if list[4].Committed || list[4].Attempt != "a2" || list[4].ManifestPath != "" {
		t.Fatalf("a2 = %+v", list[4])
	}

	if _, err := List(""); err == nil || !strings.Contains(err.Error(), "root is required") {
		t.Errorf("empty root: %v", err)
	}
	if _, err := List(filepath.Join(root, "stray.txt")); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("file root: %v", err)
	}
	if _, err := List(filepath.Join(root, "nowhere")); err == nil {
		t.Error("absent root must error")
	}
}
