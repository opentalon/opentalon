package channel

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- JWT test helpers ---

func generateTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func makeTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]interface{}) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]interface{}{"alg": "RS256", "kid": kid})
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// startMockOIDCServer serves an OIDC discovery doc and a JWKS endpoint
// containing the given public key.
func startMockOIDCServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/openidconfiguration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jwks_uri": srv.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []interface{}{
				map[string]interface{}{
					"kty": "RSA",
					"kid": kid,
					"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
				},
			},
		})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func validClaims(aud, iss string) map[string]interface{} {
	return map[string]interface{}{
		"aud": aud,
		"iss": iss,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

// --- ValidateToken tests ---

func TestJWTValidator_MalformedToken(t *testing.T) {
	v := NewJWTValidator("http://unused", "aud", "iss")
	err := v.ValidateToken(context.Background(), "notajwt")
	if err == nil {
		t.Error("expected error for malformed token")
	}
}

func TestJWTValidator_WrongAlgorithm(t *testing.T) {
	// Build a header claiming HS256
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"k1"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"aud":"a","iss":"i","exp":9999999999}`))
	token := header + "." + claims + ".fakesig"

	v := NewJWTValidator("http://unused", "a", "i")
	err := v.ValidateToken(context.Background(), token)
	if err == nil || err.Error() != "unsupported algorithm: HS256" {
		t.Errorf("got %v, want 'unsupported algorithm: HS256'", err)
	}
}

func TestJWTValidator_ExpiredToken(t *testing.T) {
	key := generateTestRSAKey(t)
	claims := map[string]interface{}{
		"aud": "my-app",
		"iss": "https://login.botframework.com",
		"exp": time.Now().Add(-time.Hour).Unix(), // expired
	}
	token := makeTestJWT(t, key, "k1", claims)

	v := NewJWTValidator("http://unused", "my-app", "https://login.botframework.com")
	err := v.ValidateToken(context.Background(), token)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestJWTValidator_IssuerMismatch(t *testing.T) {
	key := generateTestRSAKey(t)
	claims := map[string]interface{}{
		"aud": "my-app",
		"iss": "https://wrong-issuer.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := makeTestJWT(t, key, "k1", claims)

	v := NewJWTValidator("http://unused", "my-app", "https://login.botframework.com")
	err := v.ValidateToken(context.Background(), token)
	if err == nil {
		t.Error("expected error for issuer mismatch")
	}
}

func TestJWTValidator_AudienceStringMismatch(t *testing.T) {
	key := generateTestRSAKey(t)
	claims := map[string]interface{}{
		"aud": "wrong-audience",
		"iss": "https://login.botframework.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := makeTestJWT(t, key, "k1", claims)

	v := NewJWTValidator("http://unused", "my-app", "https://login.botframework.com")
	err := v.ValidateToken(context.Background(), token)
	if err == nil {
		t.Error("expected error for audience mismatch")
	}
}

func TestJWTValidator_AudienceArrayMatch(t *testing.T) {
	key := generateTestRSAKey(t)
	srv := startMockOIDCServer(t, &key.PublicKey, "k1")

	claims := map[string]interface{}{
		"aud": []interface{}{"other-app", "my-app"},
		"iss": "https://login.botframework.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := makeTestJWT(t, key, "k1", claims)

	v := NewJWTValidator(srv.URL+"/openidconfiguration", "my-app", "https://login.botframework.com")
	if err := v.ValidateToken(context.Background(), token); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestJWTValidator_AudienceArrayMismatch(t *testing.T) {
	key := generateTestRSAKey(t)
	claims := map[string]interface{}{
		"aud": []interface{}{"other-app", "another-app"},
		"iss": "https://login.botframework.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := makeTestJWT(t, key, "k1", claims)

	v := NewJWTValidator("http://unused", "my-app", "https://login.botframework.com")
	err := v.ValidateToken(context.Background(), token)
	if err == nil {
		t.Error("expected error: audience array does not contain expected value")
	}
}

func TestJWTValidator_ValidSignature(t *testing.T) {
	key := generateTestRSAKey(t)
	srv := startMockOIDCServer(t, &key.PublicKey, "k1")

	token := makeTestJWT(t, key, "k1", validClaims("my-app", "https://login.botframework.com"))

	v := NewJWTValidator(srv.URL+"/openidconfiguration", "my-app", "https://login.botframework.com")
	if err := v.ValidateToken(context.Background(), token); err != nil {
		t.Errorf("unexpected error for valid token: %v", err)
	}
}

func TestJWTValidator_InvalidSignature(t *testing.T) {
	key := generateTestRSAKey(t)
	srv := startMockOIDCServer(t, &key.PublicKey, "k1")

	token := makeTestJWT(t, key, "k1", validClaims("my-app", "https://login.botframework.com"))

	// Tamper with the signature (last segment)
	parts := splitJWT(token)
	parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256)) // zeroed sig
	tampered := parts[0] + "." + parts[1] + "." + parts[2]

	v := NewJWTValidator(srv.URL+"/openidconfiguration", "my-app", "https://login.botframework.com")
	if err := v.ValidateToken(context.Background(), tampered); err == nil {
		t.Error("expected error for tampered signature")
	}
}

func TestJWTValidator_UnknownKid(t *testing.T) {
	key := generateTestRSAKey(t)
	srv := startMockOIDCServer(t, &key.PublicKey, "k1")

	// Sign with kid "k1" but JWKS only has "k1" — should work.
	// Now sign with "k2" which is not in the JWKS.
	token := makeTestJWT(t, key, "k2-unknown", validClaims("my-app", "https://login.botframework.com"))

	v := NewJWTValidator(srv.URL+"/openidconfiguration", "my-app", "https://login.botframework.com")
	if err := v.ValidateToken(context.Background(), token); err == nil {
		t.Error("expected error for unknown kid")
	}
}

func TestJWTValidator_KeyCaching(t *testing.T) {
	key := generateTestRSAKey(t)
	fetchCount := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/openidconfiguration", func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jwks_uri": srv.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []interface{}{
				map[string]interface{}{
					"kty": "RSA",
					"kid": "k1",
					"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
				},
			},
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	v := NewJWTValidator(srv.URL+"/openidconfiguration", "my-app", "https://login.botframework.com")

	token := makeTestJWT(t, key, "k1", validClaims("my-app", "https://login.botframework.com"))

	// First call fetches keys
	if err := v.ValidateToken(context.Background(), token); err != nil {
		t.Fatalf("first validation: %v", err)
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 OIDC fetch, got %d", fetchCount)
	}

	// Second call should use cache, not fetch again
	if err := v.ValidateToken(context.Background(), token); err != nil {
		t.Fatalf("second validation: %v", err)
	}
	if fetchCount != 1 {
		t.Errorf("expected still 1 OIDC fetch after cache hit, got %d", fetchCount)
	}
}

func TestJWTValidator_ValidateRequest_MissingHeader(t *testing.T) {
	v := NewJWTValidator("http://unused", "aud", "iss")
	req := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	err := v.ValidateRequest(req)
	if err == nil {
		t.Error("expected error for missing Authorization header")
	}
}

func TestJWTValidator_ValidateRequest_NonBearerScheme(t *testing.T) {
	v := NewJWTValidator("http://unused", "aud", "iss")
	req := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	err := v.ValidateRequest(req)
	if err == nil {
		t.Error("expected error for non-Bearer scheme")
	}
}

func TestJWTValidator_ValidateRequest_ValidToken(t *testing.T) {
	key := generateTestRSAKey(t)
	srv := startMockOIDCServer(t, &key.PublicKey, "k1")

	token := makeTestJWT(t, key, "k1", validClaims("my-app", "https://login.botframework.com"))

	v := NewJWTValidator(srv.URL+"/openidconfiguration", "my-app", "https://login.botframework.com")
	req := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if err := v.ValidateRequest(req); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// splitJWT splits a JWT into its three dot-separated parts.
func splitJWT(token string) []string {
	parts := make([]string, 3)
	i := 0
	for _, ch := range token {
		if ch == '.' {
			i++
			if i > 2 {
				break
			}
			continue
		}
		parts[i] += string(ch)
	}
	return parts
}
