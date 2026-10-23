package builds

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anywaye/press/internal/store"
)

// DirUploader copies a built directory tree into the store under a prefix —
// the last step of a build, where the dist tree becomes the tenant's site.
// Every key stays under the prefix the build record fixed at creation: the
// walk joins, never replaces, so a file in the tree cannot escape into
// another tenant's prefix the way an absolute key could.
type DirUploader struct {
	store store.Client
}

// NewDirUploader wires the tree copy to a store client.
func NewDirUploader(s store.Client) *DirUploader { return &DirUploader{store: s} }

// Upload walks dir and puts every file under prefix, keeping the tree's
// relative layout. An empty dist is refused rather than uploaded: a build
// that produced nothing is a build that failed, and an empty upload would
// present as a succeeded site with no pages.
func (u *DirUploader) Upload(ctx context.Context, prefix, dir string) error {
	var files int
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
		// The prefix is a directory, with or without its slash: the key is
		// always under it, never beside it.
		key := strings.TrimSuffix(prefix, "/") + "/" + filepath.ToSlash(rel)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := u.store.Put(ctx, key, body); err != nil {
			return fmt.Errorf("builds: upload %s: %w", key, err)
		}
		files++
		return nil
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("builds: refusing to upload an empty dist at %s", dir)
	}
	return nil
}
