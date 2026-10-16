package builds

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cafaye/press/internal/ingest"
	"github.com/cafaye/press/internal/store"
)

// The worker turns a receipt into artifacts: claim a queued build, fetch the
// tarball, lay it into the engine's content tree, build, upload under the
// build's own prefix. Every step is a collaborator behind an interface so the
// whole lifecycle is testable without R2, without node, and without a git
// remote — and so a failure at any step is a failed build with the reason
// recorded, never a build stuck in running.

// Extractor lays a tarball out under a directory. press's own implementation
// refuses paths that escape the destination, because a tarball is untrusted
// input the same way a request is.
type Extractor interface {
	Extract(ctx context.Context, r io.Reader, destDir string) error
}

// Engine provides the docs engine at a pinned ref, checked out to a
// directory. press's implementation clones cafaye/docs; a build pinned to a
// ref is reproducible, and a ref not pinned is a build that changes under a
// site.
type Engine interface {
	Checkout(ctx context.Context, ref string) (dir string, err error)
}

// Runner runs the two builds inside a checked-out engine. press's
// implementation shells out: `node bin/sync-content --source <contentDir>`
// then the two astro builds, in the engine's directory, with the tenant's
// content beside it.
type Runner interface {
	Build(ctx context.Context, engineDir, contentDir string) error
}

// Uploader copies build output (a directory tree) to the store under a
// prefix.
type Uploader interface {
	Upload(ctx context.Context, prefix, dir string) error
}

// StoreReader reads a stored tarball back.
type StoreReader interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// Worker runs one build to completion.
type Worker struct {
	store     StoreReader
	extractor Extractor
	engine    Engine
	runner    Runner
	uploader  Uploader
	builds    Records
	// scratch is the temp-space seam so a test can point the worker at a
	// directory it cleans itself.
	scratch func(dir, pattern string) (string, error)
}

// NewWorker wires the worker.
func NewWorker(s StoreReader, e Extractor, eng Engine, r Runner, u Uploader, b Records) *Worker {
	return &Worker{store: s, extractor: e, engine: eng, runner: r, uploader: u, builds: b,
		scratch: os.MkdirTemp}
}

// ReceiptSource is the receipt's locatable parts: where the tarball is and
// what it is. press's ingest service records exactly these.
type ReceiptSource struct {
	ID        string
	Tenant    string
	Kind      store.Kind
	ObjectKey string
}

// Run drives b to a terminal state. It returns the terminal state and an
// error; a build that fails is recorded as failed and the error is the
// reason, so a caller never has to guess what an unfinished build meant.
func (w *Worker) Run(ctx context.Context, b Build, receipt ReceiptSource, engineRef string) (State, error) {
	if b.State != StateQueued {
		return b.State, fmt.Errorf("builds: build %s is %s, not queued", b.ID, b.State)
	}
	if err := w.builds.Transition(ctx, b.ID, StateRunning); err != nil {
		return b.State, err
	}
	b.State = StateRunning
	return w.RunClaimed(ctx, b, receipt, engineRef)
}

// RunClaimed runs a build the claim loop already moved to running. It exists
// because the claim takes the row in the same statement that moves it —
// routing a running build through Run would re-attempt the queued → running
// move the claim just made. Everything after the move is shared with Run, so
// a claimed build gets the same terminal-state guarantee: succeeded, or
// failed with the reason recorded.
func (w *Worker) RunClaimed(ctx context.Context, b Build, receipt ReceiptSource, engineRef string) (State, error) {
	if b.State != StateRunning {
		return b.State, fmt.Errorf("builds: build %s is %s, not running", b.ID, b.State)
	}
	return w.finish(ctx, b, receipt, engineRef)
}

// finish runs the build's steps and records where it landed.
func (w *Worker) finish(ctx context.Context, b Build, receipt ReceiptSource, engineRef string) (State, error) {
	if err := w.run(ctx, b, receipt, engineRef); err != nil {
		_ = w.builds.Transition(ctx, b.ID, StateFailed)
		return StateFailed, fmt.Errorf("builds: build %s: %w", b.ID, err)
	}
	if err := w.builds.Transition(ctx, b.ID, StateSucceeded); err != nil {
		return b.State, err
	}
	return StateSucceeded, nil
}

func (w *Worker) run(ctx context.Context, b Build, receipt ReceiptSource, engineRef string) error {
	if receipt.Tenant != b.Tenant {
		// The one cross-tenant shape this code can be handed: a receipt
		// from another tenant. The build record's tenant is the caller's;
		// the receipt's must match it or this is a wiring bug and the
		// content never leaves this function.
		return fmt.Errorf("receipt tenant %q does not match build tenant %q", receipt.Tenant, b.Tenant)
	}

	tarball, err := w.store.Get(ctx, receipt.ObjectKey)
	if err != nil {
		return fmt.Errorf("fetch the tarball from %s: %w", receipt.ObjectKey, err)
	}

	work, err := w.scratch("", "press-build-")
	if err != nil {
		return fmt.Errorf("scratch space: %w", err)
	}
	defer os.RemoveAll(work)

	// The content tree the engine's --source mode reads: docs/ for the
	// public kind, notes/ for the private one.
	contentDir := filepath.Join(work, "content")
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return fmt.Errorf("content dir: %w", err)
	}
	kindDir := "docs"
	if b.Kind == store.KindNotes {
		kindDir = "notes"
	}
	if err := w.extractor.Extract(ctx, strings.NewReader(string(tarball)), filepath.Join(contentDir, kindDir)); err != nil {
		return fmt.Errorf("extract the tarball: %w", err)
	}

	engineDir, err := w.engine.Checkout(ctx, engineRef)
	if err != nil {
		return fmt.Errorf("engine checkout at %s: %w", engineRef, err)
	}

	if err := w.runner.Build(ctx, engineDir, contentDir); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// The upload target is the build record's prefix — computed at creation
	// from the build's tenant and kind, never from the receipt or the
	// request. A worker that accepted a prefix from anywhere else would be
	// the one place a cross-tenant write could start.
	distDir := filepath.Join(engineDir, "dist", kindDir)
	if err := w.uploader.Upload(ctx, b.ArtifactPrefix, distDir); err != nil {
		return fmt.Errorf("upload %s: %w", b.ArtifactPrefix, err)
	}
	return nil
}

// ErrPathEscape is the extractor's refusal: a tarball member whose path
// leaves the destination directory.
var ErrPathEscape = errors.New("build: tarball member escapes the destination")

// TarExtractor extracts a gzipped tarball, refusing traversal.
type TarExtractor struct{}

// Extract reads the tarball and writes every member under destDir.
func (TarExtractor) Extract(_ context.Context, r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzipped tarball: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the tarball: %w", err)
		}
		// A member with a `..` element is refused, not rewritten. The
		// tempting defense — Clean the path and carry on — silently
		// renames the archive's content, and an archive whose meaning
		// changed under extraction is worse than one that failed loudly.
		// The prefix check is the second line, not the first.
		for _, part := range strings.Split(filepath.ToSlash(hdr.Name), "/") {
			if part == ".." {
				return fmt.Errorf("%w: %s", ErrPathEscape, hdr.Name)
			}
		}
		target := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s", ErrPathEscape, hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // symlinks and devices are not documentation
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

// IngestReceipt returns the locatable parts of a receipt. press's ingest
// service is the producer; this is the seam where the record and the tarball
// are joined for a build.
func IngestReceipt(r ingest.Receipt) ReceiptSource {
	return ReceiptSource{
		ID:        r.ID,
		Tenant:    r.Tenant,
		Kind:      store.Kind(r.Kind),
		ObjectKey: r.ObjectKey,
	}
}
