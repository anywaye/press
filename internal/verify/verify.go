package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/anywaye/press/internal/errorreporting"
	"strings"
	"time"
)

// IdentityVerifier resolves a bearer token to the account it names. The
// production implementation is a chain: JWTs are verified against identity's
// key set (with an audience check), opaque tokens through identity's
// introspection route. Either way the answer is an account id or an error,
// so the gate can treat "forged", "wrong audience", "wrong account" and
// "unreachable identity" identically — 401, with no oracle.
type IdentityVerifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// Introspection is the part of identity's answer this gate reads. `Active` is
// the token's own validity; `AccountID` is the org the caller belongs to.
type Introspection struct {
	Active    bool
	AccountID string
}

// HTTPIntrospector calls identity's introspection route. The URL is identity's
// issuer (https://auth.anywaye.com), and the route is the one identity mounts
// for exactly this question: "what does this token claim".
type HTTPIntrospector struct {
	identityURL string
	client      *http.Client
}

// NewHTTPIntrospector builds the production introspector.
func NewHTTPIntrospector(identityURL string) *HTTPIntrospector {
	return &HTTPIntrospector{
		identityURL: strings.TrimSuffix(identityURL, "/"),
		client:      &http.Client{Timeout: 5 * time.Second},
	}
}

type introspectionRequest struct {
	Token string `json:"token"`
}

type introspectionResponse struct {
	Active    bool   `json:"active"`
	AccountID string `json:"account_id"`
}

func (h *HTTPIntrospector) Introspect(ctx context.Context, token string) (Introspection, error) {
	body, err := json.Marshal(introspectionRequest{Token: token})
	if err != nil {
		return Introspection{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.identityURL+"/v1/introspections", bytes.NewReader(body))
	if err != nil {
		return Introspection{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.client.Do(req)
	if err != nil {
		errorreporting.CaptureError(fmt.Errorf("press: identity unreachable: %w", err), nil)
		return Introspection{}, fmt.Errorf("verify: identity unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Introspection{Active: false}, nil
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		errorreporting.CaptureError(fmt.Errorf("press: identity answered %d: %s", resp.StatusCode, bytes.TrimSpace(msg)), nil)
		return Introspection{}, fmt.Errorf("verify: identity answered %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out introspectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Introspection{}, fmt.Errorf("verify: identity's answer is not the introspection shape: %w", err)
	}
	return Introspection{Active: out.Active, AccountID: out.AccountID}, nil
}

// Gate decides whether a request passes.
type Gate struct {
	verifier        IdentityVerifier
	allowedAccounts map[string]bool
}

// NewGate builds the gate. allowedAccounts is the set of account ids permitted
// to read this tenant's notes — in v1 the internal tenant's own org(s), one
// entry per operator account; multi-tenant gating arrives with press's
// tenant model, and this set is where it plugs in.
func NewGate(v IdentityVerifier, allowedAccounts []string) *Gate {
	allowed := make(map[string]bool, len(allowedAccounts))
	for _, a := range allowedAccounts {
		allowed[a] = true
	}
	return &Gate{verifier: v, allowedAccounts: allowed}
}

// Admit answers the nginx auth_request subrequest. 204 for a caller the
// gate admits, 401 for one it does not. The body is always empty: nginx
// reads the status, and a body here is a body served to nobody.
func (g *Gate) Admit(w http.ResponseWriter, r *http.Request) {
	token, ok := parseBearer(r.Header.Get("Authorization"))
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	accountID, err := g.verifier.Verify(r.Context(), token)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !g.allowedAccounts[accountID] {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
