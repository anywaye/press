package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FSClient is a filesystem-backed store for local development. It implements
// Client so press can run without R2. Not for production.
type FSClient struct {
	root string
}

func NewFSClient(root string) (*FSClient, error) {
	if root == "" {
		return nil, fmt.Errorf("store: fs root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("store: cannot create fs root %s: %w", root, err)
	}
	return &FSClient{root: root}, nil
}

func (f *FSClient) path(key string) string {
	return filepath.Join(f.root, filepath.FromSlash(key))
}

func (f *FSClient) Put(_ context.Context, key string, body []byte) error {
	p := f.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o644)
}

func (f *FSClient) Get(_ context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(f.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

func (f *FSClient) Head(_ context.Context, key string) (int64, error) {
	info, err := os.Stat(f.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return info.Size(), nil
}

func (f *FSClient) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	base := f.path(prefix)
	err := filepath.Walk(f.root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(f.root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, prefix) {
			keys = append(keys, rel)
		}
		return nil
	})
	_ = base
	return keys, err
}
