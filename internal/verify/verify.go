// Package verify is the private site's door: a request to /notes/* is
// admitted when identity resolves its bearer token and the token's account
// is allowed to read this tenant's notes. It is one endpoint behind traefik's
// ForwardAuth middleware, and it exists as a separate binary from the ingest
// service because it is on the request path of every private page and the
// ingest path is on none of them — the two want different failure modes.
package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// IdentityIntrospector resolves a token to claims. The production
// implementation is identity's POST /v1/introspections; an interface so the
// behavior tests run without a live identity.
type IdentityIntrospector interface {
	Introspect(ctx context.Context, token string) (Introspection, error)
}

// Introspection is the part of identity's answer this gate reads. `Active` is
// the token's own validity; `AccountID` is the org the caller belongs to.
type Introspection struct {
	Active    bool
	AccountID string
}

// HTTPIntrospector calls identity's introspection route. The URL is identity's
// issuer (https://auth.cafaye.com), and the route is the one identity mounts
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
	resp, err := h.client.Do(req)
	if err != nil {
		return Introspection{}, fmt.Errorf("verify: identity unreachable: %w", err)
	}
	defer resp.Body.Close()
	// A 401 from identity is identity's answer, not an outage: the token did
	// not resolve. A 5xx is an outage and must NOT be treated as admission —
	// the gate fails closed.
	if resp.StatusCode == http.StatusUnauthorized {
		return Introspection{Active: false}, nil
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
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
	introspector    IdentityIntrospector
	allowedAccounts map[string]bool
}

// NewGate builds the gate. allowedAccounts is the set of account ids permitted
// to read this tenant's notes — in v1 the internal tenant's own org(s), one
// entry per operator account; multi-tenant gating arrives with press's
// tenant model, and this set is where it plugs in.
func NewGate(i IdentityIntrospector, allowedAccounts []string) *Gate {
	allowed := make(map[string]bool, len(allowedAccounts))
	for _, a := range allowedAccounts {
		allowed[a] = true
	}
	return &Gate{introspector: i, allowedAccounts: allowed}
}

// Admit answers the traefik ForwardAuth subrequest. 204 for a caller the
// gate admits, 401 for one it does not. The body is always empty: traefik
// reads the status, and a body here is a body served to nobody.
func (g *Gate) Admit(w http.ResponseWriter, r *http.Request) {
	token, ok := parseBearer(r.Header.Get("Authorization"))
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	claims, err := g.introspector.Introspect(r.Context(), token)
	if err != nil {
		// Fail closed: an unreachable identity is a closed door, never an
		// open one. The notes site being briefly unreadable during an
		// identity outage is the correct trade.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !claims.Active || !g.allowedAccounts[claims.AccountID] {
		// A valid token for the wrong org gets the same 401 as a forged
		// one: the answer must not distinguish "bad token" from "not your
		// tenant", or the endpoint becomes an account-existence oracle.
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
