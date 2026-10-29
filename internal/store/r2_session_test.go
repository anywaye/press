package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)


type capturedRequest struct {
	authorization string
	signedHeaders string
	securityToken string
}

func TestSigningCarriesTheSessionTokenAsASignedHeader(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = capturedRequest{
			authorization: r.Header.Get("Authorization"),
			signedHeaders: signedHeaderList(r.Header.Get("Authorization")),
			securityToken: r.Header.Get("x-amz-security-token"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := NewR2WithToken(srv.URL, "bucket", "AKID", "SECRET", "SESSION-TOKEN-VALUE")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := c.Put(context.Background(), "tenants/acme/ingest/r1.tar.gz", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}

	if got.securityToken != "SESSION-TOKEN-VALUE" {
		t.Fatalf("x-amz-security-token = %q, want the session token on the wire", got.securityToken)
	}
	if !containsHeader(got.signedHeaders, "x-amz-security-token") {
		t.Fatalf("SignedHeaders = %q — the token must be in the signed set, not just sent", got.signedHeaders)
	}
}

func TestSigningOmitsTheSessionTokenWhenThereIsNone(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = capturedRequest{
			securityToken: r.Header.Get("x-amz-security-token"),
			signedHeaders: signedHeaderList(r.Header.Get("Authorization")),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewR2(srv.URL, "bucket", "AKID", "SECRET")
	if err := c.Put(context.Background(), "tenants/acme/ingest/r1.tar.gz", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got.securityToken != "" {
		t.Fatalf("x-amz-security-token = %q on a request with no token", got.securityToken)
	}
	if containsHeader(got.signedHeaders, "x-amz-security-token") {
		t.Fatalf("SignedHeaders = %q — no token, no signed token header", got.signedHeaders)
	}
	if !containsHeader(got.signedHeaders, "host") {
		t.Fatalf("SignedHeaders = %q — even without a token, host is signed", got.signedHeaders)
	}
}

func signedHeaderList(authorization string) string {
	for _, part := range splitSemicolonField(authorization) {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "SignedHeaders=") {
			return strings.TrimPrefix(part, "SignedHeaders=")
		}
	}
	return ""
}

func splitSemicolonField(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func containsHeader(list, want string) bool {
	for _, h := range strings.Split(list, ";") {
		if h == want {
			return true
		}
	}
	return false
}
