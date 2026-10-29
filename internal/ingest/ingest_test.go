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

	"github.com/anywaye/press/internal/auth"
	"github.com/anywaye/press/internal/store"
)


type fakeVerifier struct {
	claims auth.Claims
	err    error
}

func (f fakeVerifier) Verify(context.Context, string) (auth.Claims, error) {
	return f.claims, f.err
}

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

type fakeQueue struct {
	enqueued []Receipt
	err      error
}

func (f *fakeQueue) Enqueue(_ context.Context, r Receipt) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, r)
	return nil
}

func newService(t *testing.T, tenant string, s store.Client, receipts Receipts, maxBytes int64) *Service {
	t.Helper()
	return NewService(fakeVerifier{claims: auth.Claims{AccountID: tenant, Subject: "user-1"}}, s, receipts, &fakeQueue{}, maxBytes)
}

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
	svc := newService(t, "beta-org", s, receipts, 8)

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
	svc := NewService(fakeVerifier{err: errors.New("token expired")}, s, &fakeReceipts{}, &fakeQueue{}, 1<<20)
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

func TestIngestHandlerRefusesAFormThatNamesAnotherTenantAndWritesNothing(t *testing.T) {
	s := newFakeStore()
	receipts := &fakeReceipts{}
	svc := newService(t, "beta-org", s, receipts, 1<<20)

	buf, contentType := multipartBodyWithTenant(t, "multipart/form-data", []byte("x"))
	r := httptest.NewRequest("POST", "/v1/ingest", buf)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer session-token")
	w := httptest.NewRecorder()

	svc.Handler().ServeHTTP(w, r)

	if s.calls != 0 {
		t.Fatal("a form naming another tenant wrote an object")
	}
	if len(receipts.rows) != 0 {
		t.Fatal("a form naming another tenant recorded a receipt")
	}
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

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

func TestIngestEnqueuesTheBuildForItsReceipt(t *testing.T) {
	s := newFakeStore()
	receipts := &fakeReceipts{}
	q := &fakeQueue{}
	svc := NewService(
		fakeVerifier{claims: auth.Claims{AccountID: "beta-org", Subject: "user-1"}},
		s, receipts, q, 1<<20)

	receipt, _, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindDocs)}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(q.enqueued) != 1 {
		t.Fatalf("enqueued %d builds, want exactly 1", len(q.enqueued))
	}
	if q.enqueued[0].ID != receipt.ID || q.enqueued[0].Tenant != "beta-org" || q.enqueued[0].Kind != "docs" {
		t.Fatalf("enqueued %+v, want the receipt's id, tenant and kind", q.enqueued[0])
	}
}

func TestIngestReportsWhenTheBuildDoesNotEnqueue(t *testing.T) {
	s := newFakeStore()
	q := &fakeQueue{err: errors.New("db down")}
	svc := NewService(fakeVerifier{claims: auth.Claims{AccountID: "beta-org"}}, s, &fakeReceipts{}, q, 1<<20)
	_, status, err := svc.Ingest(context.Background(), "token", Request{Kind: string(store.KindDocs)}, strings.NewReader("x"))
	if err == nil || status != 500 {
		t.Fatalf("status = %d, err = %v; want 500 naming the enqueue", status, err)
	}
}
