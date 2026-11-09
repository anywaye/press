package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"archive/tar"
"bytes"
"compress/gzip"
"io"
"path/filepath"
	"net/http"
	"strings"

	"github.com/anywaye/press/internal/auth"
	"github.com/anywaye/press/internal/store"
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
	verifier    auth.Verifier
	identityURL string
	store       store.Client
	receipts    Receipts
	queues      Queue
	maxBytes    int64
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

func NewService(v auth.Verifier, identityURL string, s store.Client, receipts Receipts, queues Queue, maxBytes int64) *Service {
	return &Service{verifier: v, identityURL: strings.TrimSuffix(identityURL, "/"), store: s, receipts: receipts, queues: queues, maxBytes: maxBytes}
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
		return Request{}, nil, fmt.Errorf("ingest: body is not the documented multipart form: %w", err)
	}
	req := Request{
		Kind:    r.FormValue("kind"),
		Project: strings.TrimSpace(r.FormValue("project")),
		Version: strings.TrimSpace(r.FormValue("version")),
	}
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
	// verify token via identity's /v1/me (works with session tokens)
	var tenant string
	var err error
	if s.identityURL != "" {
		tenant, err = verifyTokenViaMe(ctx, s.identityURL, token)
	} else if s.verifier != nil {
		claims, verr := s.verifier.Verify(ctx, token)
		if verr != nil {
			err = verr
		} else {
			tenant = claims.AccountID
		}
	} else {
		err = fmt.Errorf("no verifier configured")
	}
	if err != nil {
		return Receipt{}, http.StatusUnauthorized, fmt.Errorf("ingest: %w", err)
	}

	if store.Kind(req.Kind) == store.KindPantry {
		if _, err := store.PantryArtifactPrefix(tenant, req.Project, req.Version); err != nil {
			return Receipt{}, http.StatusBadRequest, fmt.Errorf("ingest: %w", err)
		}
	}

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
		return Receipt{}, http.StatusInternalServerError, fmt.Errorf("ingest: %w", err)
	}

	if err := s.store.Put(ctx, key, data); err != nil {
		return Receipt{}, http.StatusBadGateway, fmt.Errorf("ingest: cannot write the tarball: %w", err)
	}

	if err := extractAndStore(ctx, s.store, tenant, data); err != nil {
		return Receipt{}, http.StatusBadGateway, fmt.Errorf("ingest: cannot extract content: %w", err)
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
		Caller:    tenant,
	}
	if err := s.receipts.Insert(ctx, receipt); err != nil {
		return Receipt{}, http.StatusInternalServerError, fmt.Errorf("ingest: object written to %s but the receipt did not record: %w", key, err)
	}
	if s.queues != nil {
		if err := s.queues.Enqueue(ctx, receipt); err != nil {
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

func newReceiptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}


func extractAndStore(ctx context.Context, st store.Client, tenant string, data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		// not a gzipped tarball; store as-is, no extraction
		return nil
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the tarball: %w", err)
		}
		for _, part := range strings.Split(filepath.ToSlash(hdr.Name), "/") {
			if part == ".." {
				return fmt.Errorf("path escape: %s", hdr.Name)
			}
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "/")
		if name == "" {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read member %s: %w", hdr.Name, err)
		}
		key := fmt.Sprintf("tenants/%s/content/%s", tenant, name)
		if err := st.Put(ctx, key, body); err != nil {
			return fmt.Errorf("write %s: %w", key, err)
		}
	}
}


func verifyTokenViaMe(ctx context.Context, identityURL, token string) (string, error) {
	if identityURL == "" {
		return "", fmt.Errorf("identity URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, identityURL+"/v1/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("identity unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("invalid token")
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return "", fmt.Errorf("invalid identity response")
	}
	return out.ID, nil
}
