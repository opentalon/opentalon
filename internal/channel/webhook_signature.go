package channel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureSchemeGitHub           = "github"
	SignatureSchemeStandardWebhooks = "standard_webhooks"

	standardWebhooksTolerance = 5 * time.Minute
)

func resolveSignatureKey(scheme, secret string) []byte {
	if scheme != SignatureSchemeStandardWebhooks {
		if secret == "" {
			return nil
		}
		return []byte(secret)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 {
		return nil
	}
	return key
}

func verifyWebhookSignature(scheme string, key []byte, h http.Header, body []byte, now time.Time) error {
	if len(key) == 0 {
		return errors.New("signing key is empty")
	}
	switch scheme {
	case SignatureSchemeGitHub:
		return verifyGitHubSignature(key, h, body)
	case SignatureSchemeStandardWebhooks:
		return verifyStandardWebhooksSignature(key, h, body, now)
	}
	return fmt.Errorf("unknown signature scheme %q", scheme)
}

func verifyGitHubSignature(key []byte, h http.Header, body []byte) error {
	got := h.Get("X-Hub-Signature-256")
	hexSig, ok := strings.CutPrefix(got, "sha256=")
	if !ok {
		return errors.New("X-Hub-Signature-256 header missing or lacks sha256= prefix")
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil {
		return fmt.Errorf("X-Hub-Signature-256 is not hex: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return errors.New("X-Hub-Signature-256 does not match body")
	}
	return nil
}

func verifyStandardWebhooksSignature(key []byte, h http.Header, body []byte, now time.Time) error {
	id := h.Get("webhook-id")
	ts := h.Get("webhook-timestamp")
	sigs := h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return fmt.Errorf("missing webhook-id/webhook-timestamp/webhook-signature header (id=%t timestamp=%t signature=%t)", id != "", ts != "", sigs != "")
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("webhook-timestamp %q is not unix seconds", ts)
	}
	if skew := now.Sub(time.Unix(sec, 0)); skew > standardWebhooksTolerance || skew < -standardWebhooksTolerance {
		return fmt.Errorf("webhook-timestamp %d outside %s tolerance (skew %s)", sec, standardWebhooksTolerance, skew.Round(time.Second))
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, candidate := range strings.Fields(sigs) {
		b64, ok := strings.CutPrefix(candidate, "v1,")
		if !ok {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if hmac.Equal(sig, want) {
			return nil
		}
	}
	return fmt.Errorf("no v1 signature in webhook-signature matches body for webhook-id %q", id)
}
