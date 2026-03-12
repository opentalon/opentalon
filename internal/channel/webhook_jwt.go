package channel

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWTValidator validates RS256 JWTs from Microsoft Bot Framework.
// It fetches JWKS from the OIDC discovery endpoint and caches them for 24 hours.
type JWTValidator struct {
	oidcURL  string
	audience string
	issuer   string

	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey
	lastFetch time.Time

	httpClient *http.Client
}

// NewJWTValidator creates a new JWTValidator.
func NewJWTValidator(oidcURL, audience, issuer string) *JWTValidator {
	return &JWTValidator{
		oidcURL:    oidcURL,
		audience:   audience,
		issuer:     issuer,
		keys:       make(map[string]crypto.PublicKey),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ValidateRequest extracts the Bearer token from an HTTP request and validates it.
func (v *JWTValidator) ValidateRequest(r *http.Request) error {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return fmt.Errorf("missing or invalid Authorization header")
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	return v.ValidateToken(r.Context(), token)
}

// ValidateToken validates a raw JWT string.
func (v *JWTValidator) ValidateToken(ctx context.Context, token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid JWT format")
	}

	// Decode header
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode JWT header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fmt.Errorf("parse JWT header: %w", err)
	}
	if header.Alg != "RS256" {
		return fmt.Errorf("unsupported algorithm: %s", header.Alg)
	}

	// Decode claims
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode JWT claims: %w", err)
	}
	var claims struct {
		Aud interface{} `json:"aud"` // string or []string
		Iss string      `json:"iss"`
		Exp int64       `json:"exp"`
	}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return fmt.Errorf("parse JWT claims: %w", err)
	}

	// Validate expiry
	if time.Now().Unix() > claims.Exp {
		return fmt.Errorf("JWT expired")
	}

	// Validate issuer
	if claims.Iss != v.issuer {
		return fmt.Errorf("JWT issuer mismatch: got %q, want %q", claims.Iss, v.issuer)
	}

	// Validate audience
	if err := v.validateAudience(claims.Aud); err != nil {
		return err
	}

	// Get signing key
	key, err := v.getKey(ctx, header.Kid)
	if err != nil {
		return fmt.Errorf("get signing key: %w", err)
	}

	// Verify signature
	signingInput := parts[0] + "." + parts[1]
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode JWT signature: %w", err)
	}

	return verifyRS256([]byte(signingInput), sigBytes, key)
}

func (v *JWTValidator) validateAudience(aud interface{}) error {
	switch a := aud.(type) {
	case string:
		if a != v.audience {
			return fmt.Errorf("JWT audience mismatch: got %q, want %q", a, v.audience)
		}
	case []interface{}:
		for _, item := range a {
			if s, ok := item.(string); ok && s == v.audience {
				return nil
			}
		}
		return fmt.Errorf("JWT audience does not contain %q", v.audience)
	default:
		return fmt.Errorf("JWT audience has unexpected type")
	}
	return nil
}

// getKey returns the cached public key for the given kid,
// fetching from OIDC endpoint if needed or on cache miss.
func (v *JWTValidator) getKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	stale := time.Since(v.lastFetch) > 24*time.Hour
	v.mu.RUnlock()

	if ok && !stale {
		return key, nil
	}

	// Fetch fresh keys
	if err := v.fetchKeys(ctx); err != nil {
		return nil, err
	}

	v.mu.RLock()
	key, ok = v.keys[kid]
	v.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("key ID %q not found in JWKS", kid)
	}
	return key, nil
}

// fetchKeys retrieves the JWKS from the OIDC discovery endpoint.
func (v *JWTValidator) fetchKeys(ctx context.Context) error {
	// Fetch OIDC discovery doc
	req, err := http.NewRequestWithContext(ctx, "GET", v.oidcURL, nil)
	if err != nil {
		return fmt.Errorf("build OIDC request: %w", err)
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch OIDC config: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read OIDC config body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fetch OIDC config: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var oidcConfig struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &oidcConfig); err != nil {
		return fmt.Errorf("parse OIDC config: %w", err)
	}
	if oidcConfig.JWKSURI == "" {
		return fmt.Errorf("parse OIDC config: missing jwks_uri")
	}

	// Fetch JWKS
	req, err = http.NewRequestWithContext(ctx, "GET", oidcConfig.JWKSURI, nil)
	if err != nil {
		return fmt.Errorf("build JWKS request: %w", err)
	}
	resp, err = v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read JWKS body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fetch JWKS: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}

	keys := make(map[string]crypto.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}

	v.mu.Lock()
	v.keys = keys
	v.lastFetch = time.Now()
	v.mu.Unlock()

	return nil
}

func parseRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func verifyRS256(signingInput, signature []byte, key crypto.PublicKey) error {
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("key is not RSA")
	}
	h := sha256.Sum256(signingInput)
	return rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, h[:], signature)
}
