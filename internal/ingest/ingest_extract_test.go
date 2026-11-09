package ingest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"

	"github.com/anywaye/press/internal/store"
)

func makeTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func TestExtractAndStore(t *testing.T) {
	data := makeTarball(t, map[string]string{
		"docs/hello.md":   "# Hello",
		"articles/x.md":  "# X",
	})

	s := newFakeStore()
	if err := extractAndStore(context.Background(), s, "boax", data); err != nil {
		t.Fatal(err)
	}

	// verify files were extracted
	if _, ok := s.puts["tenants/boax/content/docs/hello.md"]; !ok {
		t.Error("docs/hello.md not extracted")
	}
	if _, ok := s.puts["tenants/boax/content/articles/x.md"]; !ok {
		t.Error("articles/x.md not extracted")
	}
	if string(s.puts["tenants/boax/content/docs/hello.md"]) != "# Hello" {
		t.Errorf("content mismatch: got %q", string(s.puts["tenants/boax/content/docs/hello.md"]))
	}
}

func TestExtractAndStoreTraversal(t *testing.T) {
	// create a tarball with a traversal attempt
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "../escape.md", Mode: 0o644, Size: 5}
	tw.WriteHeader(hdr)
	tw.Write([]byte("evil"))
	tw.Close()
	gw.Close()

	s := newFakeStore()
	err := extractAndStore(context.Background(), s, "boax", buf.Bytes())
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "path escape") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExtractAndStoreNonTarball(t *testing.T) {
	// non-tarball data should be skipped (no error)
	s := newFakeStore()
	err := extractAndStore(context.Background(), s, "boax", []byte("not a tarball"))
	if err != nil {
		t.Fatalf("expected nil for non-tarball, got %v", err)
	}
	if len(s.puts) != 0 {
		t.Errorf("expected no puts, got %d", len(s.puts))
	}
}

var _ = store.ErrNotFound
