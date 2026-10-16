package builds

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/cafaye/press/internal/store"
)

// Behavior tests for the worker: each one names a thing the worker must do,
// or must refuse, and drives it with fakes. No node, no R2, no network.

// fakeStoreReader answers Get from a map and counts calls.
type fakeStoreReader struct {
	objects map[string][]byte
}

func (f fakeStoreReader) Get(_ context.Context, key string) ([]byte, error) {
	if b, ok := f.objects[key]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

// fakeEngine records the ref asked for and returns a temp dir it owns.
type fakeEngine struct {
	dir  string
	err  error
	refs []string
}

func (f *fakeEngine) Checkout(_ context.Context, ref string) (string, error) {
	f.refs = append(f.refs, ref)
	if f.err != nil {
		return "", f.err
	}
	return f.dir, nil
}

// fakeRunner records what it was asked to build, and can fail. It snapshots
// the content tree it was handed, because the worker cleans its scratch
// before the test can look at the filesystem.
type fakeRunner struct {
	err      error
	calls    int
	lastEng  string
	lastCnt  string
	snapshot []string
	walkErr  error
}

func (f *fakeRunner) Build(_ context.Context, engineDir, contentDir string) error {
	f.calls++
	f.lastEng, f.lastCnt = engineDir, contentDir
	f.snapshot, f.walkErr = walkRel(contentDir)
	if f.err != nil {
		return f.err
	}
	// What the real runner leaves behind: a dist tree under the engine dir.
	for _, kind := range []string{"docs", "notes"} {
		p := filepath.Join(engineDir, "dist", kind)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p, "index.html"), []byte("<html>built</html>"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fakeUploader records the prefix and directory it was told to upload, and
// can fail. The prefix is the assertion that matters.
type fakeUploader struct {
	err    error
	prefix string
	dir    string
	calls  int
}

func (f *fakeUploader) Upload(_ context.Context, prefix, dir string) error {
	f.calls++
	f.prefix, f.dir = prefix, dir
	return f.err
}

// recordsFake persists transitions for real, so the lifecycle is exercised
// through the same code the Postgres store runs.
type workerRecordsFake struct {
	rows map[string]Build
}

func newWorkerRecordsFake() *workerRecordsFake {
	return &workerRecordsFake{rows: map[string]Build{}}
}

func (r *workerRecordsFake) Insert(_ context.Context, b Build) error {
	r.rows[b.ID] = b
	return nil
}

func (r *workerRecordsFake) Get(_ context.Context, id string) (Build, error) {
	b, ok := r.rows[id]
	if !ok {
		return Build{}, errors.New("builds: no such build")
	}
	return b, nil
}

func (r *workerRecordsFake) Transition(_ context.Context, id string, next State) error {
	b, ok := r.rows[id]
	if !ok {
		return errors.New("builds: no such build")
	}
	if err := b.Transition(next); err != nil {
		return err
	}
	r.rows[id] = b
	return nil
}

// tarball builds a gzipped tarball with one file, so the extractor has real
// bytes to chew.
func tarball(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// walkRel lists every file under dir, relative to it, so a test can assert
// the layout the worker materialized.
func walkRel(dir string) ([]string, error) {
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// newWorkerTest wires a worker over fakes and returns the parts the test
// asserts against.
func newWorkerTest(t *testing.T) (*Worker, *workerRecordsFake, *fakeRunner, *fakeUploader, string) {
	t.Helper()
	dir := t.TempDir()
	recs := newWorkerRecordsFake()
	runner := &fakeRunner{}
	up := &fakeUploader{}
	sr := fakeStoreReader{objects: map[string][]byte{}}
	w := NewWorker(sr, TarExtractor{}, &fakeEngine{dir: dir}, runner, up, recs)
	w.scratch = func(dir, pattern string) (string, error) { return os.MkdirTemp(dir, pattern) }
	return w, recs, runner, up, dir
}

func TestRunBuildsTheKindAndUploadsToTheBuildOwnPrefix(t *testing.T) {
	w, recs, runner, up, _ := newWorkerTest(t)
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	key := "tenants/acme/ingest/r1.tar.gz"
	w.store.(fakeStoreReader).objects[key] = tarball(t, "index.md", "hello")

	receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: key}
	state, err := w.Run(context.Background(), b, receipt, "press-engine-v1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if state != StateSucceeded {
		t.Fatalf("state = %q, want succeeded", state)
	}
	if runner.calls != 1 {
		t.Fatalf("runner ran %d times, want 1", runner.calls)
	}
	// The upload target is the build record's prefix — derived from the
	// build's tenant and kind, and nothing else.
	if want := "tenants/acme/dist/docs/"; up.prefix != want {
		t.Fatalf("uploaded to %q, want %q", up.prefix, want)
	}
	if stored := recs.rows["b1"]; stored.State != StateSucceeded {
		t.Fatalf("stored state = %q", stored.State)
	}
}

func TestRunBuildExtractsTheNotesKindIntoTheNotesTree(t *testing.T) {
	// The private kind's content must land under notes/ in the engine's
	// content dir, or the build would produce an empty private site.
	w, recs, runner, _, _ := newWorkerTest(t)
	b, _ := New("b1", "acme", store.KindNotes, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	key := "tenants/acme/ingest/r1.tar.gz"
	w.store.(fakeStoreReader).objects[key] = tarball(t, "runbook.md", "private")

	receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindNotes, ObjectKey: key}
	if _, err := w.Run(context.Background(), b, receipt, "ref"); err != nil {
		t.Fatalf("run: %v", err)
	}
	found := false
	for _, p := range runner.snapshot {
		if p == "notes/runbook.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the tarball did not land in the notes tree; snapshot: %v", runner.snapshot)
	}
}

func TestRunBuildRefusesAReceiptFromAnotherTenant(t *testing.T) {
	// The one cross-tenant shape this code can be handed. The tarball IS in
	// the store under the receipt's own key, so the tenant check is the only
	// thing that can stop the build: with it removed, this run fetches acme's
	// content, builds it and uploads it under beta's prefix, and the
	// zero-counts assertion below is what fails. Without the seeded object the
	// test would pass on a fetch failure instead — green for a reason it does
	// not name.
	w, recs, runner, up, _ := newWorkerTest(t)
	b, _ := New("b1", "beta", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	key := "tenants/acme/ingest/r1.tar.gz"
	w.store.(fakeStoreReader).objects[key] = tarball(t, "index.md", "acme's content")

	receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: key}
	state, err := w.Run(context.Background(), b, receipt, "ref")
	if runner.calls != 0 || up.calls != 0 {
		t.Fatal("a cross-tenant build ran or uploaded something")
	}
	if err == nil || state != StateFailed {
		t.Fatalf("state = %q err = %v; want failed", state, err)
	}
	if stored := recs.rows["b1"]; stored.State != StateFailed {
		t.Fatalf("stored state = %q, want failed", stored.State)
	}
}

func TestRunBuildRefusesAReceiptThatNamesNoTenant(t *testing.T) {
	// The no-identity arm of the claim path: a receipt row with no tenant is
	// not any account's, and the worker must not build it for whichever build
	// happens to be beside it. The tarball IS in the store, so only the tenant
	// check can refuse it — with the check removed the build succeeds and
	// uploads, and the zero-counts assertion below is what fails.
	w, recs, runner, up, _ := newWorkerTest(t)
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	key := "tenants/acme/ingest/r1.tar.gz"
	w.store.(fakeStoreReader).objects[key] = tarball(t, "index.md", "content whose receipt names no tenant")

	receipt := ReceiptSource{ID: "r1", Tenant: "", Kind: store.KindDocs, ObjectKey: key}
	state, err := w.Run(context.Background(), b, receipt, "ref")
	if runner.calls != 0 || up.calls != 0 {
		t.Fatal("a receipt naming no tenant was built or uploaded")
	}
	if err == nil || state != StateFailed {
		t.Fatalf("state = %q err = %v; want failed", state, err)
	}
	if stored := recs.rows["b1"]; stored.State != StateFailed {
		t.Fatalf("stored state = %q, want failed", stored.State)
	}
}

func TestRunBuildFailsWithTheReasonAndNeverSticksInRunning(t *testing.T) {
	// Whatever breaks, the record lands on failed. A build stuck in running
	// is a build whose tenant silently never gets its artifacts.
	for name, breakIt := range map[string]func(*newWorkerTestParts){
		"runner fails": func(p *newWorkerTestParts) { p.runner.err = errors.New("npm ci failed") },
		"store fetch fails": func(p *newWorkerTestParts) {
			p.reader.objects = map[string][]byte{} // the key is absent
		},
		"upload fails": func(p *newWorkerTestParts) { p.up.err = errors.New("r2 503") },
	} {
		t.Run(name, func(t *testing.T) {
			w, recs, _, _, _ := newWorkerTest(t)
			p := &newWorkerTestParts{runner: w.runner.(*fakeRunner), up: w.uploader.(*fakeUploader), reader: w.store.(fakeStoreReader)}
			breakIt(p)
			b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
			_ = recs.Insert(context.Background(), b)
			receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: "tenants/acme/ingest/r1.tar.gz"}
			state, err := w.Run(context.Background(), b, receipt, "ref")
			if err == nil || state != StateFailed {
				t.Fatalf("state = %q err = %v; want failed with a reason", state, err)
			}
			if stored := recs.rows["b1"]; stored.State != StateFailed {
				t.Fatalf("stored state = %q, want failed", stored.State)
			}
		})
	}
}

type newWorkerTestParts struct {
	runner *fakeRunner
	up     *fakeUploader
	reader fakeStoreReader
}

func TestTarExtractorRefusesATarballThatEscapesTheDestination(t *testing.T) {
	// A tarball is untrusted input the same way a request is: a member
	// named ../../etc/passwd is refused, and nothing is written outside
	// the destination.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.md", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()
	_ = gz.Close()

	dest := filepath.Join(t.TempDir(), "content", "docs")
	err := TarExtractor{}.Extract(context.Background(), bytes.NewReader(buf.Bytes()), dest)
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("err = %v, want the path-escape refusal", err)
	}
	parent := filepath.Dir(dest)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() == "escape.md" {
			t.Fatal("the escaping member was written")
		}
	}
}

func TestWorkerPinTheEngineRefItBuiltAgainst(t *testing.T) {
	// A build is reproducible because it names the engine ref; the ref is
	// recorded on the run, so a behavior change in the engine is diagnosable
	// per build.
	w, recs, _, _, _ := newWorkerTest(t)
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	key := "tenants/acme/ingest/r1.tar.gz"
	w.store.(fakeStoreReader).objects[key] = tarball(t, "index.md", "x")
	receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: key}
	if _, err := w.Run(context.Background(), b, receipt, "press-engine-2026-10-15"); err != nil {
		t.Fatalf("run: %v", err)
	}
	refs := w.engine.(*fakeEngine).refs
	if len(refs) != 1 || refs[0] != "press-engine-2026-10-15" {
		t.Fatalf("engine refs asked for: %v", refs)
	}
}

func TestRunRefusesABuildThatIsNotQueued(t *testing.T) {
	w, recs, _, _, _ := newWorkerTest(t)
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	_ = recs.Transition(context.Background(), "b1", StateRunning)
	receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: "tenants/acme/ingest/r1.tar.gz"}
	if _, err := w.Run(context.Background(), b, receipt, "ref"); err == nil {
		t.Fatal("a build that is already running was run again")
	}
}

func TestTarExtractorAcceptsTheTarballRootEntry(t *testing.T) {
	// A tarball written with a leading dot (`tar -czf x .`) carries a root
	// member named "./" — the destination itself. The traversal check must
	// refuse what leaves the destination and admit what IS it.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Mode: 0o755, Typeflag: tar.TypeDir})
	_ = tw.WriteHeader(&tar.Header{Name: "./index.md", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	_ = gz.Close()

	dest := filepath.Join(t.TempDir(), "content", "docs")
	if err := (TarExtractor{}).Extract(context.Background(), bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "index.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want the file under the destination", got)
	}
}
