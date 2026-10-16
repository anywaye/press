// Package ingest is press's entry point: a tarball arrives, a caller is
// verified, an object lands under the caller's tenant prefix, and a receipt is
// written. Every decision in this package follows from one invariant:
//
//	the tenant comes from the verified token, never from the request.
//
// A request that names a different tenant is not a confused request to be
// tolerated; it is a caller whose body claims an identity the token does not.
// This package therefore has no field that accepts a tenant from a caller at
// all — the only inputs are the claims and the content's own names (kind,
// project, version), each validated tight enough that no path can escape its
// prefix.
package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cafaye/press/internal/auth"
	"github.com/cafaye/press/internal/store"
)

// Receipt is what a successful ingest returns and what the receipt table
// holds: the durable record that content arrived, from whom, and where.
type Receipt struct {
	ID        string `json:"id"`
	Tenant    string `json:"tenant"`
	Kind      string `json:"kind"`
	Project   string `json:"project,omitempty"`
	Version   string `json:"version,omitempty"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	State     string `json:"state"`
	ObjectKey string `json:"-"`
	Caller    string `json:"-"`
}

// ErrReceiptExists means the generated receipt id collided, which should
// never happen; the retry response is the honest one.
var ErrReceiptExists = errors.New("ingest: receipt id already exists")

// Service is the ingest path with its collaborators as interfaces. The
// interfaces exist for one reason: every behavior below is testable without
// Postgres, without R2, and without identity — a behavior test that needs the
// database is a behavior test that silently skips in CI.
type Service struct {
	verifier auth.Verifier
	store    store.Client
	receipts Receipts
	// queues enqueues the build for a receipt, in the same request that
	// records it. A receipt without a build is a promise nothing keeps:
	// the caller saw 201 and no one is building their content. Found by
	// publishing to a live press and watching the builds table stay empty.
	queues   Queue
	maxBytes int64
}

// Queue enqueues the build a receipt exists to feed. PressPosts's
// implementation writes the receipt and the build in one transaction (the
// repository's shape); the interface is here so the behavior tests can watch
// what an ingest enqueues without a database.
type Queue interface {
	Enqueue(ctx context.Context, receipt Receipt) error
}

// Receipts is the receipt table's behavior: remember an ingest, never
// overwrite one.
type Receipts interface {
	Insert(ctx context.Context, r Receipt) error
}

func NewService(v auth.Verifier, s store.Client, receipts Receipts, queues Queue, maxBytes int64) *Service {
	return &Service{verifier: v, store: s, receipts: receipts, queues: queues, maxBytes: maxBytes}
}

// Request is the parsed form press accepts. There is deliberately no Tenant
// field: the tenant is the token's account_id, and a form field that claimed
// one would be a second, lying source of it.
type Request struct {
	Kind    string
	Project string
	Version string
}

// ParseRequest reads the multipart form press documents. The whole body is
// the tarball; the form fields describe it.
func ParseRequest(r *http.Request) (Request, io.Reader, error) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		// A form that fails to parse is a caller sending something press
		// did not document; the error names what to read.
		return Request{}, nil, fmt.Errorf("ingest: body is not the documented multipart form: %w", err)
	}
	req := Request{
		Kind:    r.FormValue("kind"),
		Project: strings.TrimSpace(r.FormValue("project")),
		Version: strings.TrimSpace(r.FormValue("version")),
	}
	// A form that names a tenant is refused, not ignored. press's tenant is
	// the token's account_id and there is no field for it: a caller sending
	// one believes it chose its tenant, and quietly ignoring the field
	// would let that belief ship.
	if r.FormValue("tenant") != "" {
		return req, nil, fmt.Errorf("ingest: there is no tenant field; the tenant is the token's account")
	}
	if req.Kind == "" {
		return req, nil, fmt.Errorf("ingest: kind is required (docs, notes or pantry)")
	}
	kind := store.Kind(req.Kind)
	if !kind.Valid() {
		return req, nil, fmt.Errorf("ingest: kind %q is not a kind press ingests", req.Kind)
	}
	if kind == store.KindPantry && (req.Project == "" || req.Version == "") {
		return req, nil, fmt.Errorf("ingest: a pantry ingest needs project and version")
	}
	if kind != store.KindPantry && (req.Project != "" || req.Version != "") {
		return req, nil, fmt.Errorf("ingest: project and version belong to a pantry ingest only")
	}
	file, _, err := r.FormFile("content")
	if err != nil {
		return req, nil, fmt.Errorf("ingest: no content file in the form (field name is 'content'): %w", err)
	}
	return req, file, nil
}

// Ingest runs the whole path: verify, cap, digest, write, record. It returns
// the receipt and the status the caller should see.
func (s *Service) Ingest(ctx context.Context, token string, req Request, body io.Reader) (Receipt, int, error) {
	claims, err := s.verifier.Verify(ctx, token)
	if err != nil {
		return Receipt{}, http.StatusUnauthorized, fmt.Errorf("ingest: %w", err)
	}
	tenant := claims.AccountID

	// A pantry ingest's project and version become path segments in its
	// artifact prefix, so they are validated here — at the service seam —
	// and not only in the form parser: a receipt whose project can never
	// produce a valid artifact key must not be recorded at all. This is
	// also the guard that keeps a traversal-shaped project name out of the
	// build's key layout one hop later.
	if store.Kind(req.Kind) == store.KindPantry {
		if _, err := store.PantryArtifactPrefix(tenant, req.Project, req.Version); err != nil {
			return Receipt{}, http.StatusBadRequest, fmt.Errorf("ingest: %w", err)
		}
	}

	// The cap is enforced while reading, before anything is written, so an
	// over-cap request cannot leave a partial object in the tenant's
	// prefix. This is the one place press spends a reader to save a
	// reconciliation job.
	limited := io.LimitReader(body, s.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return Receipt{}, http.StatusBadRequest, fmt.Errorf("ingest: cannot read the content: %w", err)
	}
	if int64(len(data)) > s.maxBytes {
		return Receipt{}, http.StatusRequestEntityTooLarge,
			fmt.Errorf("ingest: tarball is larger than the %d byte cap", s.maxBytes)
	}
	if len(data) == 0 {
		return Receipt{}, http.StatusBadRequest, fmt.Errorf("ingest: content file is empty")
	}

	id, err := newReceiptID()
	if err != nil {
		return Receipt{}, http.StatusInternalServerError, fmt.Errorf("ingest: cannot mint a receipt id: %w", err)
	}

	key, err := store.IngestKey(tenant, id)
	if err != nil {
		// The tenant came from identity and identity's ids are already
		// segment-clean, so this is a press bug, not a caller error.
		return Receipt{}, http.StatusInternalServerError, fmt.Errorf("ingest: %w", err)
	}

	if err := s.store.Put(ctx, key, data); err != nil {
		return Receipt{}, http.StatusBadGateway, fmt.Errorf("ingest: cannot write the tarball: %w", err)
	}

	sum := sha256.Sum256(data)
	receipt := Receipt{
		ID:        id,
		Tenant:    tenant,
		Kind:      req.Kind,
		Project:   req.Project,
		Version:   req.Version,
		SHA256:    hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
		State:     "received",
		ObjectKey: key,
		Caller:    claims.Subject,
	}
	if err := s.receipts.Insert(ctx, receipt); err != nil {
		// The object exists and the receipt does not. That is a real
		// orphan, and press's reconciliation run exists to sweep
		// ingest/ objects with no receipt row. Returning 500 says so
		// instead of hiding a lost receipt behind a 201.
		return Receipt{}, http.StatusInternalServerError, fmt.Errorf("ingest: object written to %s but the receipt did not record: %w", key, err)
	}
	if s.queues != nil {
		if err := s.queues.Enqueue(ctx, receipt); err != nil {
			// The receipt is a row the caller can act on; the missing
			// build is the difference between "your content arrived" and
			// "your content is being built". A 500 with the reason keeps
			// the caller's retry able to produce a build.
			return Receipt{}, http.StatusInternalServerError, fmt.Errorf("ingest: receipt %s recorded but the build did not enqueue: %w", receipt.ID, err)
		}
	}
	return receipt, http.StatusCreated, nil
}

// Handler is the HTTP surface. It is a thin translation over Service so the
// logic stays testable without net/http.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := auth.Bearer(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "a bearer token from identity is required")
			return
		}
		req, body, err := ParseRequest(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		receipt, status, err := s.Ingest(r.Context(), token, req, body)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(receipt)
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// newReceiptID is a 128-bit server-side id, base32hex so it is URL-safe and
// sorts lexicographically. The receipt id is never taken from the request:
// a caller that chose its own id could aim its upload at another tenant's
// prefix.
func newReceiptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
