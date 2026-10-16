package builds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Behavior tests for the production collaborators behind the worker's
// interfaces: the subprocess runner, the git engine checkout, and the tree
// uploader. Each one asserts the exact external effect — the argv, the keys —
// with the process boundary faked, so no node, no git remote, and no R2.

// recordRun is the exec seam's fake: it records every invocation and can fail
// a named step.
type recordRun struct {
	calls [][3]string // dir, name, argv joined
	fail  string      // substring of the shell line that fails
}

func (r *recordRun) run(_ context.Context, dir, name string, args ...string) error {
	r.calls = append(r.calls, [3]string{dir, name, strings.Join(args, " ")})
	if r.fail != "" && strings.Contains(name+" "+strings.Join(args, " "), r.fail) {
		return errors.New("exit status 1")
	}
	return nil
}

func TestSubprocessRunnerRunsSyncThenTheTwoBuilds(t *testing.T) {
	// The v1 contract with the engine: sync the tenant content first, then
	// the public build, then the notes build — each in the engine directory.
	rec := &recordRun{}
	r := &SubprocessRunner{run: rec.run}

	content := filepath.Join(t.TempDir(), "content")
	if err := r.Build(context.Background(), "/engine", content); err != nil {
		t.Fatalf("build: %v", err)
	}

	want := [][3]string{
		{"/engine", "node", "bin/sync-content --source " + content},
		{"/engine", "npm", "run build:public"},
		{"/engine", "npm", "run build:notes"},
	}
	if len(rec.calls) != len(want) {
		t.Fatalf("ran %d steps, want %d: %v", len(rec.calls), len(want), rec.calls)
	}
	for i := range want {
		if rec.calls[i] != want[i] {
			t.Errorf("step %d = %v, want %v", i, rec.calls[i], want[i])
		}
	}
}

func TestSubprocessRunnerStopsAtTheFirstFailureAndNamesIt(t *testing.T) {
	// A build that synced but never compiled is a failed build, not half a
	// site: the failure names the step, and the steps after it do not run.
	rec := &recordRun{fail: "build:public"}
	r := &SubprocessRunner{run: rec.run}

	err := r.Build(context.Background(), "/engine", "/content")
	if err == nil {
		t.Fatal("a failed public build was reported as success")
	}
	if !strings.Contains(err.Error(), "build:public") {
		t.Fatalf("error = %v; want the failing step named", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("ran %d steps after the failure, want the run to stop at 2", len(rec.calls))
	}
}

func TestGitEngineChecksOutThePinnedRefDetached(t *testing.T) {
	// The engine arrives at exactly the ref named — detached, so the build
	// cannot drift — and an unpinned checkout is refused rather than guessed.
	rec := &recordRun{}
	e := &GitEngine{repo: "https://example.com/cafaye/docs", scratch: os.MkdirTemp, run: rec.run}

	dir, err := e.Checkout(context.Background(), "press-engine-v1")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	defer os.RemoveAll(dir)

	if len(rec.calls) != 2 {
		t.Fatalf("ran %d commands, want clone + checkout: %v", len(rec.calls), rec.calls)
	}
	if rec.calls[0][1] != "git" || !strings.Contains(rec.calls[0][2], "clone https://example.com/cafaye/docs") {
		t.Errorf("first command = %v, want the clone", rec.calls[0])
	}
	if rec.calls[1] != [3]string{dir, "git", "checkout --detach press-engine-v1"} {
		t.Errorf("second command = %v, want the detached checkout", rec.calls[1])
	}

	if _, err := e.Checkout(context.Background(), ""); err == nil {
		t.Fatal("an unpinned checkout was accepted")
	}
}

// fakePutStore records every Put for the uploader test.
type fakePutStore struct {
	puts map[string][]byte
	err  error
}

func (f *fakePutStore) Put(_ context.Context, key string, body []byte) error {
	if f.err != nil {
		return f.err
	}
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = body
	return nil
}

func (f *fakePutStore) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("not needed here")
}

func (f *fakePutStore) Head(_ context.Context, _ string) (int64, error) {
	return 0, errors.New("not needed here")
}

func TestDirUploaderPutsEveryFileUnderThePrefix(t *testing.T) {
	// The dist tree's layout is preserved under the build's prefix, and the
	// prefix is a directory whether or not it arrived with its slash.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.css"), []byte("css"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, prefix := range []string{"tenants/acme/dist/docs/", "tenants/acme/dist/docs"} {
		s := &fakePutStore{}
		if err := NewDirUploader(s).Upload(context.Background(), prefix, dir); err != nil {
			t.Fatalf("upload with prefix %q: %v", prefix, err)
		}
		if string(s.puts["tenants/acme/dist/docs/index.html"]) != "<html>" {
			t.Errorf("prefix %q: index.html missing or wrong: %v", prefix, keysOf(s.puts))
		}
		if string(s.puts["tenants/acme/dist/docs/assets/app.css"]) != "css" {
			t.Errorf("prefix %q: nested file missing or wrong: %v", prefix, keysOf(s.puts))
		}
		if len(s.puts) != 2 {
			t.Errorf("prefix %q: uploaded %d objects, want 2", prefix, len(s.puts))
		}
	}
}

func TestDirUploaderRefusesAnEmptyDist(t *testing.T) {
	// A build that produced nothing failed; uploading nothing under its
	// prefix would present as a succeeded site with no pages.
	s := &fakePutStore{}
	err := NewDirUploader(s).Upload(context.Background(), "tenants/acme/dist/docs/", t.TempDir())
	if err == nil {
		t.Fatal("an empty dist was uploaded without complaint")
	}
	if len(s.puts) != 0 {
		t.Fatal("the refused upload wrote objects anyway")
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
