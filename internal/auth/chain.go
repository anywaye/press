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
		return Claims{}, fmt.Errorf("auth: the token carries no account_id")
	}
	return Claims{AccountID: in.AccountID, Subject: in.Subject}, nil
}
