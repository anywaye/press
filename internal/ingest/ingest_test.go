package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cafaye/press/internal/auth"
	"github.com/cafaye/press/internal/store"
)

// These are behavior tests: each one names a thing press must do (or must
// refuse to do) and drives the whole path — verifier to store to receipts —
// with fakes. No framework is exercised; the service's collaborators are the
// interfaces the service itself declares.

// fakeVerifier answers Verify from a table, so a test can be the tenant.
type fakeVerifier struct {
	claims auth.Claims
	err    error
}

func (f fakeVerifier) Verify(context.Context, string) (auth.Claims, error) {
	return f.claims, f.err
}

// fakeStore records every Put and can fail the next one.
type fakeStore struct {
	puts  map[string][]byte
	fail  error
	calls int
}

func newFakeStore() *fakeStore { return &fakeStore{puts: map[string][]byte{}} }

func (f *fakeStore) Put(_ context.Context, key string, body []byte) error {
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.puts[key] = body
	return nil
}
func (f *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	if b, ok := f.puts[key]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeStore) Head(_ context.Context, key string) (int64, error) {
	if b, ok := f.puts[key]; ok {
		return int64(len(b)), nil
	}
	return 0, store.ErrNotFound
}

// fakeReceipts records receipts and can refuse a specific id.
type fakeReceipts struct {
	rows    []Receipt
	err     error
	failAll bool
}

func (f *fakeReceipts) Insert(_ context.Context, r Receipt) error {
	if f.failAll {
		return f.err
	}
	for _, row := range f.rows {
		if row.ID == r.ID {
			return ErrReceiptExists
		}
	}
	f.rows = append(f.rows, r)
	return nil
}

// newService builds an ingest service with a stubbed verifier for tenant.
func newService(t *testing.T, tenant string, s store.Client, receipts Receipts, maxBytes int64) *Service {
	t.Helper()
	return NewService(fakeVerifier{claims: auth.Claims{AccountID: tenant, Subject: "user-1"}}, s, receipts, maxBytes)
}

// multipartBody builds the form press documents: content plus kind, project,
// version fields.
func multipartBody(t *testing.T, kind, project, version string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if kind != "" {
		_ = w.WriteField("kind", kind)
	}
	if project != "" {
		_ = w.WriteField("project", project)
	}
	if version != "" {
		_ = w.WriteField("version", version)
	}
	fw, err := w.CreateFormFile("content", "docs.tar.gz")
	if err != nil {
		t.Fatalf("build form: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func TestIngestWritesUnderTheCallersOwnTenantPrefixAndNothingElse(t *testing.T) {
	s := newFakeStore()
	receipts := &fakeReceipts{}
	svc := newService(t, "beta-org", s, receipts, 1<<20)

	req := Request{Kind: string(store.KindDocs)}
	receipt, status, err := svc.Ingest(context.Background(), "token", req, strings.NewReader("tarball-bytes"))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if status != 201 {
		t.Fatalf("status = %d, want 201", status)
	}

	wantPrefix := "tenants/beta-org/ingest/"
	if !strings.HasPrefix(receipt.ObjectKey, wantPrefix) {
		t.Fatalf("object key %q is not under the caller's tenant prefix %q", receipt.ObjectKey, wantPrefix)
	}
	if len(s.puts) != 1 {
		t.Fatalf("store saw %d puts, want exactly 1", len(s.puts))
	}
	if got := string(s.puts[receipt.ObjectKey]); got != "tarball-bytes" {
		t.Fatalf("stored %q, want the exact bytes sent", got)
	}
	if receipt.Tenant != "beta-org" {
		t.Fatalf("receipt tenant = %q, want the token's account", receipt.Tenant)
	}
	if len(receipts.rows) != 1 || receipts.rows[0].ID != receipt.ID {
		t.Fatalf("receipt row not recorded")
	}
}

func TestIngestRefusesAProjectNameThatCouldEscapeItsPrefix(t *testing.T) {
	// The form has no tenant field, so the escape route is a project name
	// with traversal in it — a pantry ingest whose project is "../other".
	// press must refuse it before the key is built, and write nothing.
	for _, bad := range []string{"../other", "a/b", "..", "CANON"} {
		s := newFakeStore()
		svc := newService(t, "beta-org", s, &fakeReceipts{}, 1<<20)
		req := Request{Kind: string(store.KindPantry), Project: bad, Version: "v1"}
		_, _, err := svc.Ingest(context.Background(), "token", req, strings.NewReader("bytes"))
		if err == nil {
			t.Fatalf("project %q was accepted", bad)
		}
		if s.calls != 0 {
			t.Fatalf("project %q wrote an object before being refused", bad)
		}
	}
}

func TestIngestRefusesAnOverCapTarballBeforeWritingAnything(t *testing.T) {
	s := newFakeStore()
	receipts := &fakeReceipts{}
	svc := newService(t, "beta-org", s, receipts, 8) // eight bytes

	_, status, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindDocs)}, strings.NewReader("nine bytes!"))
	if err == nil {
		t.Fatal("an over-cap tarball was accepted")
	}
	if status != 413 {
		t.Fatalf("status = %d, want 413", status)
	}
	if s.calls != 0 {
		t.Fatal("an over-cap request wrote an object — the cap must be enforced while reading")
	}
	if len(receipts.rows) != 0 {
		t.Fatal("an over-cap request recorded a receipt")
	}
}

func TestIngestRefusesAnUnverifiedCaller(t *testing.T) {
	s := newFakeStore()
	svc := NewService(fakeVerifier{err: errors.New("token expired")}, s, &fakeReceipts{}, 1<<20)
	_, status, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindDocs)}, strings.NewReader("x"))
	if err == nil || status != 401 {
		t.Fatalf("status = %d, err = %v; want 401", status, err)
	}
	if s.calls != 0 {
		t.Fatal("an unverified caller wrote an object")
	}
}

func TestIngestRecordsTheDigestAndSizeOfTheExactBytesSent(t *testing.T) {
	s := newFakeStore()
	receipts := &fakeReceipts{}
	svc := newService(t, "beta-org", s, receipts, 1<<20)

	content := []byte("exact bytes, digests and all")
	receipt, _, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindNotes)}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	sum := sha256.Sum256(content)
	if receipt.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt digest %q is not the digest of the bytes sent", receipt.SHA256)
	}
	if receipt.Size != int64(len(content)) {
		t.Fatalf("receipt size %d, want %d", receipt.Size, len(content))
	}
	if row := receipts.rows[0]; row.SHA256 != receipt.SHA256 || row.Size != receipt.Size {
		t.Fatal("the recorded row does not match the returned receipt")
	}
}

func TestIngestGeneratesItsOwnReceiptId(t *testing.T) {
	// The receipt id is the object's identity and part of its key; a caller
	// that chose it could collide with another tenant's receipt id.
	s := newFakeStore()
	svc := newService(t, "beta-org", s, &fakeReceipts{}, 1<<20)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		r, _, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindDocs)}, strings.NewReader("x"))
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
		if len(r.ID) != 32 {
			t.Fatalf("receipt id %q is not the 128-bit id press mints", r.ID)
		}
		if seen[r.ID] {
			t.Fatal("two ingests got the same receipt id")
		}
		seen[r.ID] = true
	}
}

func TestIngestWhenTheReceiptTableRefusesLeavesAnOrphanItSaysSo(t *testing.T) {
	// The documented failure order: the object is written, then recorded. A
	// refusal at record time means the object exists with no receipt, and
	// the reconciliation run exists to sweep those. What press must not do
	// is answer 201 hiding a receipt that was never written.
	s := newFakeStore()
	svc := newService(t, "beta-org", s, &fakeReceipts{failAll: true, err: errors.New("db down")}, 1<<20)
	_, status, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindDocs)}, strings.NewReader("x"))
	if err == nil || status != 500 {
		t.Fatalf("status = %d, err = %v; want 500", status, err)
	}
	if len(s.puts) != 1 {
		t.Fatal("the object should exist for reconciliation to sweep")
	}
}

func TestParseRequestRejectsAFormThatNamesATenantField(t *testing.T) {
	// The one hostile form press must notice by shape: a caller sending the
	// tenant in the body. There is no such field, so the request is refused
	// rather than silently ignored — a client that thinks it sets the tenant
	// is a client misunderstanding the whole boundary, and silence would let
	// it ship.
	buf, contentType := multipartBody(t, string(store.KindDocs), "", "", []byte("x"))
	buf2, _ := multipartBodyWithTenant(t, contentType, []byte("x"))

	r1 := httptest.NewRequest("POST", "/v1/ingest", buf)
	r1.Header.Set("Content-Type", contentType)
	if _, _, err := ParseRequest(r1); err != nil {
		t.Fatalf("the documented form did not parse: %v", err)
	}

	r2 := httptest.NewRequest("POST", "/v1/ingest", buf2)
	r2.Header.Set("Content-Type", contentType)
	if _, _, err := ParseRequest(r2); err == nil {
		t.Fatal("a form carrying a tenant field was accepted")
	}
}

// multipartBodyWithTenant is the same form with an extra field no press
// endpoint documents.
func multipartBodyWithTenant(t *testing.T, contentType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("kind", string(store.KindDocs))
	_ = w.WriteField("tenant", "someone-else")
	fw, err := w.CreateFormFile("content", "docs.tar.gz")
	if err != nil {
		t.Fatalf("build form: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return &buf, w.FormDataContentType()
}
