package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The audience claim is what makes identity's exchange a delegation to ONE
// service: a token minted for billing is not a token for press. These tests
// sign real tokens with a real key and serve the real JWKS shape, so the
// verification path under test is the shipped one.

func TestJWKSVerifierAdmitsItsAudienceAndRefusesAnother(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), "test-key"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
		}})
	}))
	defer srv.Close()

	mint := func(audience string) string {
		tok, err := jwt.Signed(signer).Claims(map[string]any{
			"iss":        "https://auth.anywaye.com",
			"aud":        audience,
			"sub":        "user-1",
			"account_id": "acct-1",
			"exp":        time.Now().Add(time.Hour).Unix(),
		}).Serialize()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return tok
	}

	v, err := NewJWKSVerifierFor(srv.URL, "https://auth.anywaye.com", "press")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	claims, err := v.Verify(context.Background(), mint("press"))
	if err != nil {
		t.Fatalf("a token for press was refused: %v", err)
	}
	if claims.AccountID != "acct-1" {
		t.Fatalf("claims = %+v", claims)
	}

	if _, err := v.Verify(context.Background(), mint("billing")); err == nil {
		t.Fatal("a token minted for billing was accepted by press")
	}
}
