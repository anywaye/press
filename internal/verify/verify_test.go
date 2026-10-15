package verify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Behavior tests for the door: each names a request the gate must admit or
// refuse, driven through the real handler with a fake identity.

// fakeIntrospector answers from a table, and can fail the way an outage does.
type fakeIntrospector struct {
	claims map[string]Introspection
	err    error
}

func (f *fakeIntrospector) Introspect(_ context.Context, token string) (Introspection, error) {
	if f.err != nil {
		return Introspection{}, f.err
	}
	if c, ok := f.claims[token]; ok {
		return c, nil
	}
	return Introspection{Active: false}, nil
}

func admit(t *testing.T, g *Gate, token string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/verify", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	g.Admit(w, r)
	return w.Code
}

func TestGateAdmitsATokenForAnAllowedAccount(t *testing.T) {
	g := NewGate(&fakeIntrospector{claims: map[string]Introspection{
		"good": {Active: true, AccountID: "acct-cafaye"},
	}}, []string{"acct-cafaye"})
	if code := admit(t, g, "good"); code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", code)
	}
}

func TestGateRefusesEveryWrongCallerWithTheSame401(t *testing.T) {
	// The shapes that must all answer 401 — and answer it identically, so
	// the endpoint cannot be used to tell a forged token from a valid one
	// belonging to another org.
	ident := &fakeIntrospector{claims: map[string]Introspection{
		"good":         {Active: true, AccountID: "acct-cafaye"},
		"other-org":    {Active: true, AccountID: "acct-acme"},
		"revoked":      {Active: false, AccountID: "acct-cafaye"},
		"unresolvable": {},
	}}
	g := NewGate(ident, []string{"acct-cafaye"})
	for _, token := range []string{"other-org", "revoked", "unresolvable", ""} {
		if code := admit(t, g, token); code != http.StatusUnauthorized {
			t.Fatalf("token %q: code = %d, want 401", token, code)
		}
	}
}

func TestGateFailsClosedWhenIdentityIsUnreachable(t *testing.T) {
	// An outage is a closed door, never an open one: a notes site unreadable
	// for a minute beats a notes site readable by everyone for a minute.
	g := NewGate(&fakeIntrospector{err: errors.New("dial tcp: connection refused")}, []string{"acct-cafaye"})
	if code := admit(t, g, "any-token"); code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", code)
	}
}

func TestGateWithNoAllowedAccountsRefusesEveryone(t *testing.T) {
	// A misconfigured gate — no allowlist — is closed, not open. This is the
	// assertion that makes the allowlist's absence a deployment failure
	// instead of an open private site.
	g := NewGate(&fakeIntrospector{claims: map[string]Introspection{
		"good": {Active: true, AccountID: "acct-cafaye"},
	}}, nil)
	if code := admit(t, g, "good"); code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", code)
	}
}

func TestGateWithMultipleAllowedAccounts(t *testing.T) {
	g := NewGate(&fakeIntrospector{claims: map[string]Introspection{
		"a": {Active: true, AccountID: "acct-one"},
		"b": {Active: true, AccountID: "acct-two"},
	}}, []string{"acct-one", "acct-two"})
	if code := admit(t, g, "a"); code != http.StatusNoContent {
		t.Fatalf("account a: code = %d", code)
	}
	if code := admit(t, g, "b"); code != http.StatusNoContent {
		t.Fatalf("account b: code = %d", code)
	}
}
