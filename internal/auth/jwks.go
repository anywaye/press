// Package auth is press's trust boundary: a caller is who identity says they
// are, and nothing else. The producer side is identity's JWKS endpoint; this
// package is the verifying half, and it verifies exactly one thing — that the
// bearer token was minted by identity, is unexpired, and names an account —
// because that is everything press's authorization needs: the tenant IS the
// account.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Claims are the fields press reads off a verified token. Everything else the
// token carries is none of press's business.
type Claims struct {
	// AccountID is the tenant. It is the ONLY source of a tenant in press:
	// no request body, no header, no query parameter can name one.
	AccountID string
	// Subject is the identity's user id, recorded on the receipt so an
	// ingest is auditable to a human without a join.
	Subject string
}

// Verifier checks a bearer token. It is an interface so the ingest path's
// tests can exercise every branch (valid, expired, wrong issuer, missing
// account) without a live identity, and so the identity outage mode — refuse
// everything — is a property of the verifier and not of each caller.
type Verifier interface {
	Verify(ctx context.Context, token string) (Claims, error)
}

// JWKSVerifier verifies RS256 tokens against identity's published key set.
// The key set is fetched and cached, with the refresh interval stated rather
// than poked: identity rotates keys, and a verifier that never refreshes
// turns a rotation into an outage.
type JWKSVerifier struct {
	url    string
	issuer string
	// audience is the one audience this verifier admits. Identity's
	// session-to-token exchange mints tokens per audience on purpose — a
	// token minted for billing names billing — and a verifier that skipped
	// the claim would accept a token minted for any other service, which is
	// a delegation that widens silently.
	audience string
	fetch    func(ctx context.Context, url string) (*jose.JSONWebKeySet, error)
	mu       sync.Mutex
	cached   *jose.JSONWebKeySet
	fetched  time.Time
	ttl      time.Duration
	client   *http.Client
}

// NewJWKSVerifier builds the production verifier. The issuer is expected and
// required: a token minted by some other issuer is refused even if the key
// happens to validate, because "the signature checks out" is not "identity
// said this".
func NewJWKSVerifier(jwksURL, issuer string) (*JWKSVerifier, error) {
	return NewJWKSVerifierFor(jwksURL, issuer, "")
}

// NewJWKSVerifierFor builds a verifier that admits one audience. press's
// audience is `press` — the name identity's exchange pattern allows and the
// one a caller names when exchanging a session for a token to publish with.
func NewJWKSVerifierFor(jwksURL, issuer, audience string) (*JWKSVerifier, error) {
	if jwksURL == "" || issuer == "" {
		return nil, fmt.Errorf("auth: jwks url and issuer are both required")
	}
	return &JWKSVerifier{
		url:      jwksURL,
		issuer:   issuer,
		audience: audience,
		ttl:      5 * time.Minute,
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Verify parses and verifies the token.
func (v *JWKSVerifier) Verify(ctx context.Context, token string) (Claims, error) {
	tok, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return Claims{}, fmt.Errorf("auth: token is not an RS256 JWS: %w", err)
	}

	keys, err := v.keys(ctx)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: cannot read identity's key set: %w", err)
	}

	// The kid selects the key; a token whose kid is not in the set is
	// refused rather than tried against every key.
	header := tok.Signatures[0].Header
	var key *jose.JSONWebKey
	for i := range keys.Keys {
		if keys.Keys[i].KeyID == header.KeyID {
			key = &keys.Keys[i]
			break
		}
	}
	if key == nil {
		return Claims{}, fmt.Errorf("auth: identity's key set has no key %q", header.KeyID)
	}

	payload, err := tok.Verify(key)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: token does not verify: %w", err)
	}

	var raw struct {
		Issuer    string `json:"iss"`
		Audience  string `json:"aud"`
		Subject   string `json:"sub"`
		AccountID string `json:"account_id"`
		Expiry    int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Claims{}, fmt.Errorf("auth: token payload is not the expected shape: %w", err)
	}
	if raw.Issuer != v.issuer {
		return Claims{}, fmt.Errorf("auth: token issuer is %q, expected %q", raw.Issuer, v.issuer)
	}
	if v.audience != "" && raw.Audience != v.audience {
		// A token minted for another service is not a token for this one.
		// The exchange mints one audience per token precisely so this check
		// has something to check.
		return Claims{}, fmt.Errorf("auth: token audience is %q, expected %q", raw.Audience, v.audience)
	}
	if raw.Expiry <= time.Now().Unix() {
		return Claims{}, fmt.Errorf("auth: token expired")
	}
	// A token without an account names no tenant, so it authors nothing.
	if raw.AccountID == "" {
		return Claims{}, fmt.Errorf("auth: token carries no account_id")
	}
	return Claims{AccountID: raw.AccountID, Subject: raw.Subject}, nil
}

func (v *JWKSVerifier) keys(ctx context.Context) (*jose.JSONWebKeySet, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cached != nil && time.Since(v.fetched) < v.ttl {
		return v.cached, nil
	}
	keys, err := v.fetchKeySet(ctx)
	if err != nil {
		return nil, err
	}
	v.cached, v.fetched = keys, time.Now()
	return keys, nil
}

func (v *JWKSVerifier) fetchKeySet(ctx context.Context) (*jose.JSONWebKeySet, error) {
	if v.fetch != nil {
		return v.fetch(ctx, v.url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("key set endpoint answered %d", resp.StatusCode)
	}
	var keys jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, fmt.Errorf("key set is not a JWKS: %w", err)
	}
	return &keys, nil
}

// Bearer extracts the token from an Authorization header. The constant-time
// comparison note does not apply — the token's authenticity is the JWKS
// verification's job, and this is a prefix check.
func Bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
