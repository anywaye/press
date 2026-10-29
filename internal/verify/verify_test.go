package verify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)


type fakeVerifier struct {
	accounts map[string]string
	err     error
}

func (f *fakeVerifier) Verify(_ context.Context, token string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if acct, ok := f.accounts[token]; ok {
		return acct, nil
	}
	return "", errors.New("verify: token not active")
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
	g := NewGate(&fakeVerifier{accounts: map[string]string{
		"good": "acct-anywaye",
	}}, []string{"acct-anywaye"})
	if code := admit(t, g, "good"); code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", code)
	}
}

func TestGateRefusesEveryWrongCallerWithTheSame401(t *testing.T) {
	ident := &fakeVerifier{accounts: map[string]string{
		"good":      "acct-anywaye",
		"other-org": "acct-acme",
	}}
	g := NewGate(ident, []string{"acct-anywaye"})
	for _, token := range []string{"other-org", "revoked", "unresolvable", ""} {
		if code := admit(t, g, token); code != http.StatusUnauthorized {
			t.Fatalf("token %q: code = %d, want 401", token, code)
		}
	}
}

func TestGateFailsClosedWhenIdentityIsUnreachable(t *testing.T) {
	g := NewGate(&fakeVerifier{err: errors.New("dial tcp: connection refused")}, []string{"acct-anywaye"})
	if code := admit(t, g, "any-token"); code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", code)
	}
}

func TestGateWithNoAllowedAccountsRefusesEveryone(t *testing.T) {
	g := NewGate(&fakeVerifier{accounts: map[string]string{
		"good": "acct-anywaye",
	}}, nil)
	if code := admit(t, g, "good"); code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", code)
	}
}

func TestGateWithMultipleAllowedAccounts(t *testing.T) {
	g := NewGate(&fakeVerifier{accounts: map[string]string{
		"a": "acct-one",
		"b": "acct-two",
	}}, []string{"acct-one", "acct-two"})
	if code := admit(t, g, "a"); code != http.StatusNoContent {
		t.Fatalf("account a: code = %d", code)
	}
	if code := admit(t, g, "b"); code != http.StatusNoContent {
		t.Fatalf("account b: code = %d", code)
	}
}

func TestHTTPIntrospectorPresentsTheTokenAsItsOwnCaller(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true,"account_id":"acct"}`))
	}))
	defer srv.Close()

	h := NewHTTPIntrospector(srv.URL)
	in, err := h.Introspect(context.Background(), "opaque-session-token")
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if gotAuth != "Bearer opaque-session-token" {
		t.Fatalf("Authorization = %q, want the token presented as its own caller", gotAuth)
	}
	if !in.Active || in.AccountID != "acct" {
		t.Fatalf("introspection = %+v", in)
	}
}
