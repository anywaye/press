package auth

import (
	"context"
	"errors"
	"testing"
)


type stubVerifier struct {
	got   string
	claim Claims
	err   error
}

func (s *stubVerifier) Verify(_ context.Context, token string) (Claims, error) {
	s.got = token
	return s.claim, s.err
}

type stubIntrospector struct {
	got string
	in  Introspection
	err error
}

func (s *stubIntrospector) Introspect(_ context.Context, token string) (Introspection, error) {
	s.got = token
	return s.in, s.err
}

func TestChainSendsAThreePartTokenToTheKeySet(t *testing.T) {
	jwks := &stubVerifier{claim: Claims{AccountID: "acct", Subject: "u1"}}
	intro := &stubIntrospector{}
	c := ChainVerifier{JWT: jwks, Introspector: intro}
	got, err := c.Verify(context.Background(), "aGVhZGVy.cGF5bG9hZA.c2ln")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.AccountID != "acct" {
		t.Fatalf("claims = %+v", got)
	}
	if intro.got != "" {
		t.Fatal("a JWS reached introspection — the dispatch is wrong")
	}
}

func TestChainSendsAnOpaqueTokenToIdentity(t *testing.T) {
	jwks := &stubVerifier{err: errors.New("must not be called")}
	intro := &stubIntrospector{in: Introspection{Active: true, AccountID: "acct-opaque", Subject: "u2"}}
	c := ChainVerifier{JWT: jwks, Introspector: intro}
	got, err := c.Verify(context.Background(), "5cRVhnknaNDZAXBM36U4o5E6aqWwQ9irZmSCEvuMpto")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.AccountID != "acct-opaque" {
		t.Fatalf("claims = %+v", got)
	}
	if jwks.got != "" {
		t.Fatal("an opaque token reached the key set — the dispatch is wrong")
	}
}

func TestChainRefusesAForgedJWSSHapeWithoutIntrospecting(t *testing.T) {
	jwks := &stubVerifier{err: errors.New("token does not verify")}
	intro := &stubIntrospector{in: Introspection{Active: true, AccountID: "acct"}}
	c := ChainVerifier{JWT: jwks, Introspector: intro}
	if _, err := c.Verify(context.Background(), "forged.head.body"); err == nil {
		t.Fatal("a JWS-shaped token that failed the key set was accepted")
	}
	if intro.got != "" {
		t.Fatal("a forged JWS fell through to introspection")
	}
}

func TestChainRefusesAnInactiveOpaqueToken(t *testing.T) {
	c := ChainVerifier{JWT: &stubVerifier{}, Introspector: &stubIntrospector{in: Introspection{Active: false}}}
	if _, err := c.Verify(context.Background(), "opaque-token"); err == nil {
		t.Fatal("an inactive token was accepted")
	}
}

func TestChainRefusesAnOpaqueTokenWithNoAccount(t *testing.T) {
	c := ChainVerifier{JWT: &stubVerifier{}, Introspector: &stubIntrospector{in: Introspection{Active: true}}}
	if _, err := c.Verify(context.Background(), "opaque-token"); err == nil {
		t.Fatal("a token with no account was accepted")
	}
}
