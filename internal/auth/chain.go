// The chain verifier: identity issues two kinds of credential and press must
// accept both, because its callers hold whichever one they have.
//
//   - Agents and services hold RS256 JWTs (JWKS-verifiable).
//   - A human who ran `cafe login` holds an opaque session token — revocation
//     is a row delete, so identity keeps it server-side and it is NOT a JWS.
//
// press's first ingest accepted only the first kind, so the CLI's own login
// token was refused with "compact JWS format must have three parts" — a
// sentence that names the format and not the problem. The dispatcher reads
// the token's shape: three parts is a JWS and goes to the key set; anything
// else is opaque and goes to identity's introspection route, which is the
// same route the notes gate already lives on.
package auth

import (
	"context"
	"fmt"
	"strings"
)

// Introspector resolves an opaque token to claims. internal/verify's HTTP
// introspector is the production implementation; the interface lives here so
// the chain does not import the gate.
type Introspector interface {
	Introspect(ctx context.Context, token string) (Introspection, error)
}

// Introspection is the part of identity's answer the chain reads.
type Introspection struct {
	Active    bool
	AccountID string
	Subject   string
}

// ChainVerifier accepts a JWT via the key set and an opaque token via
// introspection.
type ChainVerifier struct {
	JWT          Verifier
	Introspector Introspector
}

// Verify dispatches on the token's shape. A JWS-shaped token that fails JWKS
// verification does NOT fall through to introspection: falling through would
// turn a forged JWT into an introspection call, and the answer to "is this
// signature good" must come from the key set alone.
func (c ChainVerifier) Verify(ctx context.Context, token string) (Claims, error) {
	if strings.Count(token, ".") == 2 {
		return c.JWT.Verify(ctx, token)
	}
	if c.Introspector == nil {
		return Claims{}, fmt.Errorf("auth: the token is opaque and no introspector is configured")
	}
	in, err := c.Introspector.Introspect(ctx, token)
	if err != nil {
		return Claims{}, err
	}
	if !in.Active {
		return Claims{}, fmt.Errorf("auth: identity does not consider the token active")
	}
	if in.AccountID == "" {
		// A session with no account names no tenant, so it authors nothing.
		return Claims{}, fmt.Errorf("auth: the token carries no account_id")
	}
	return Claims{AccountID: in.AccountID, Subject: in.Subject}, nil
}
