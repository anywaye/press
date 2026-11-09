package content

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anywaye/press/internal/auth"
	"github.com/anywaye/press/internal/store"
)

func newTestIdentity(t *testing.T, validTokens map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if validTokens[token] {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "user-" + token})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
}

func newTestService(t *testing.T, identitySrv *httptest.Server, store *fakeStoreCS) *Service {
	t.Helper()
	// auth.Verifier is unused now (isAuthed uses /v1/me), but NewService requires it
	return NewService(store, auth.ChainVerifier{}, identitySrv.URL, nil)
}

// fakeStoreCS implements store.Client for content tests
type fakeStoreCS struct {
	data map[string][]byte
}

func newFakeStoreCS() *fakeStoreCS { return &fakeStoreCS{data: map[string][]byte{}} }

func (f *fakeStoreCS) Put(_ context.Context, key string, body []byte) error {
	f.data[key] = body
	return nil
}
func (f *fakeStoreCS) Get(_ context.Context, key string) ([]byte, error) {
	if b, ok := f.data[key]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeStoreCS) Head(_ context.Context, key string) (int64, error) {
	if b, ok := f.data[key]; ok {
		return int64(len(b)), nil
	}
	return 0, store.ErrNotFound
}
func (f *fakeStoreCS) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func TestContentManifestAnonymous(t *testing.T) {
	store := newFakeStoreCS()
	store.data["tenants/boax/content/docs/quickstart.md"] = []byte("---\ntitle: Quickstart\n---\n# Quickstart")
	store.data["tenants/boax/content/articles/hello.md"] = []byte("---\ntitle: Hello\ndate: 2026-01-01\n---\n# Hello")
	store.data["tenants/boax/content/notes/secret.md"] = []byte("# Secret")
	store.data["tenants/boax/content/drafts/docs/wip.md"] = []byte("# WIP")

	identitySrv := newTestIdentity(t, nil)
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/manifest", nil)
	req.Header.Set("X-Tenant-ID", "boax")
	rr := httptest.NewRecorder()
	svc.handleManifest(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Items []ManifestItem `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	// anonymous: only docs + articles
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2 (docs + articles)", len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.Section == "notes" || strings.HasPrefix(item.Section, "drafts/") {
			t.Errorf("anonymous manifest leaked private section: %s", item.Section)
		}
	}
}

func TestContentManifestAuthed(t *testing.T) {
	store := newFakeStoreCS()
	store.data["tenants/boax/content/docs/quickstart.md"] = []byte("---\ntitle: Quickstart\n---\n# Quickstart")
	store.data["tenants/boax/content/notes/secret.md"] = []byte("---\ntitle: Secret\n---\n# Secret")

	identitySrv := newTestIdentity(t, map[string]bool{"valid-token": true})
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/manifest", nil)
	req.Header.Set("X-Tenant-ID", "boax")
	req.Header.Set("Authorization", "Bearer valid-token")
	rr := httptest.NewRecorder()
	svc.handleManifest(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Items []ManifestItem `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	// authed: docs + notes
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2 (docs + notes)", len(resp.Items))
	}
}

func TestContentGetPublic(t *testing.T) {
	store := newFakeStoreCS()
	store.data["tenants/boax/content/docs/quickstart.md"] = []byte("# Quickstart")

	identitySrv := newTestIdentity(t, nil)
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/docs/quickstart.md", nil)
	req.Header.Set("X-Tenant-ID", "boax")
	req.SetPathValue("path", "docs/quickstart.md")
	rr := httptest.NewRecorder()
	svc.handleGet(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content-type = %q, want text/markdown", ct)
	}
	if !strings.Contains(rr.Body.String(), "Quickstart") {
		t.Errorf("body missing content")
	}
}

func TestContentGetPrivateNoAuth(t *testing.T) {
	store := newFakeStoreCS()
	store.data["tenants/boax/content/notes/secret.md"] = []byte("# Secret")

	identitySrv := newTestIdentity(t, nil)
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/notes/secret.md", nil)
	req.Header.Set("X-Tenant-ID", "boax")
	req.SetPathValue("path", "notes/secret.md")
	rr := httptest.NewRecorder()
	svc.handleGet(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestContentGetPrivateAuthed(t *testing.T) {
	store := newFakeStoreCS()
	store.data["tenants/boax/content/notes/secret.md"] = []byte("# Secret")

	identitySrv := newTestIdentity(t, map[string]bool{"valid-token": true})
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/notes/secret.md", nil)
	req.Header.Set("X-Tenant-ID", "boax")
	req.Header.Set("Authorization", "Bearer valid-token")
	req.SetPathValue("path", "notes/secret.md")
	rr := httptest.NewRecorder()
	svc.handleGet(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestContentMissingTenant(t *testing.T) {
	store := newFakeStoreCS()
	identitySrv := newTestIdentity(t, nil)
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/manifest", nil)
	rr := httptest.NewRecorder()
	svc.handleManifest(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestContentThemeDefault(t *testing.T) {
	store := newFakeStoreCS()
	identitySrv := newTestIdentity(t, nil)
	defer identitySrv.Close()

	svc := newTestService(t, identitySrv, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/content/theme", nil)
	req.Header.Set("X-Tenant-ID", "boax")
	rr := httptest.NewRecorder()
	svc.handleTheme(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// default theme is empty object
	if strings.TrimSpace(rr.Body.String()) != "{}" {
		t.Errorf("body = %q, want {}", rr.Body.String())
	}
}
