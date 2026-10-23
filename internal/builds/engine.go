package builds

import (
	"context"
	"fmt"
	"os"
)

// GitEngine checks the docs engine out at a pinned ref with git. A build
// pinned to a ref is reproducible, and a ref not pinned is a build that
// changes under a site — so the ref is checked out detached, exactly as
// named, and the directory is the engine's: the runner's commands run inside
// it.
//
// v1 clones whole and checks out. A full clone is the honest cost of "any
// ref": a shallow `--branch` clone cannot check out a SHA, and the pinned ref
// this process passes is usually a tag or a SHA, not a branch. Depth is the
// follow-up packet's optimization, not this one's correctness risk.
type GitEngine struct {
	repo string
	// scratch and run are the seams, so tests assert the argv and the layout
	// without a git remote.
	scratch func(dir, pattern string) (string, error)
	run     func(ctx context.Context, dir, name string, args ...string) error
}

// NewGitEngine builds the production engine checkout against repo, the git
// URL (or local path) of anywaye/docs.
func NewGitEngine(repo string) *GitEngine {
	return &GitEngine{repo: repo, scratch: os.MkdirTemp, run: runCommand}
}

// Checkout clones the engine and checks out ref detached, returning the
// directory. An empty ref is refused: an unpinned checkout is the build that
// changes under a site.
func (e *GitEngine) Checkout(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("builds: refusing an unpinned engine checkout")
	}
	if e.repo == "" {
		return "", fmt.Errorf("builds: the engine repository is not configured")
	}
	dir, err := e.scratch("", "press-engine-")
	if err != nil {
		return "", fmt.Errorf("builds: scratch space for the engine: %w", err)
	}
	if err := e.run(ctx, dir, "git", "clone", e.repo, "."); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("builds: clone the engine: %w", err)
	}
	if err := e.run(ctx, dir, "git", "checkout", "--detach", ref); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("builds: check out the engine at %s: %w", ref, err)
	}
	return dir, nil
}
