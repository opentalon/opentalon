package channel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

func githubSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func standardWebhooksSignature(key []byte, id, ts, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func serveSigned(t *testing.T, ch *YAMLChannel, wh *WebhookInboundSpec, body string, headers map[string]string) int {
	t.Helper()
	handler := ch.buildWebhookHandler(wh)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec.Code
}

func githubChannel(secret string) (*YAMLChannel, *WebhookInboundSpec) {
	ch := newTestChannel([]string{"message"}, make(chan pkg.InboundMessage, 1))
	ch.webhookSignatureKey = resolveSignatureKey("github", secret)
	return ch, &WebhookInboundSpec{SignatureScheme: "github", ResponseCode: 200}
}

var testSigningKey = []byte("0123456789abcdef0123456789abcdef")
var testSigningToken = "whsec_" + base64.StdEncoding.EncodeToString(testSigningKey)

func standardWebhooksChannel(token string) (*YAMLChannel, *WebhookInboundSpec) {
	ch := newTestChannel([]string{"message"}, make(chan pkg.InboundMessage, 1))
	ch.webhookSignatureKey = resolveSignatureKey("standard_webhooks", token)
	return ch, &WebhookInboundSpec{SignatureScheme: "standard_webhooks", ResponseCode: 200}
}

func standardHeaders(id, ts, sig string) map[string]string {
	return map[string]string{"webhook-id": id, "webhook-timestamp": ts, "webhook-signature": sig}
}

func nowTS() string { return strconv.FormatInt(time.Now().Unix(), 10) }

func TestWebhookSignatureGitHubValid(t *testing.T) {
	ch, wh := githubChannel("gh-secret")
	body := `{"action":"opened"}`
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("gh-secret", body)})
	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
}

func TestWebhookSignatureGitHubRejects(t *testing.T) {
	body := `{"action":"opened"}`
	valid := githubSignature("gh-secret", body)
	cases := map[string]string{
		"missing header":     "",
		"wrong secret":       githubSignature("other", body),
		"tampered body":      githubSignature("gh-secret", body+" "),
		"no prefix":          strings.TrimPrefix(valid, "sha256="),
		"sha1 prefix":        "sha1=" + strings.TrimPrefix(valid, "sha256="),
		"non-hex":            "sha256=zz" + valid[9:],
		"truncated":          valid[:len(valid)-2],
		"uppercase prefix":   "SHA256=" + strings.TrimPrefix(valid, "sha256="),
		"prefix only":        "sha256=",
		"static secret sent": "gh-secret",
	}
	for name, sig := range cases {
		ch, wh := githubChannel("gh-secret")
		headers := map[string]string{}
		if sig != "" {
			headers["X-Hub-Signature-256"] = sig
		}
		if code := serveSigned(t, ch, wh, body, headers); code != http.StatusUnauthorized {
			t.Errorf("%s: got status %d, want 401", name, code)
		}
	}
}

func TestWebhookSignatureGitHubEmptySecretFailsClosed(t *testing.T) {
	ch, wh := githubChannel("")
	body := `{}`
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("", body)})
	if code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401: an empty secret must never verify", code)
	}
}

func TestWebhookSignatureStandardWebhooksValid(t *testing.T) {
	ch, wh := standardWebhooksChannel(testSigningToken)
	body := `{"object_kind":"note"}`
	ts := nowTS()
	sig := standardWebhooksSignature(testSigningKey, "msg_1", ts, body)
	if code := serveSigned(t, ch, wh, body, standardHeaders("msg_1", ts, sig)); code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
}

func TestWebhookSignatureStandardWebhooksTokenWithoutPrefix(t *testing.T) {
	ch, wh := standardWebhooksChannel(base64.StdEncoding.EncodeToString(testSigningKey))
	body := `{}`
	ts := nowTS()
	sig := standardWebhooksSignature(testSigningKey, "msg_1", ts, body)
	if code := serveSigned(t, ch, wh, body, standardHeaders("msg_1", ts, sig)); code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
}

func TestWebhookSignatureStandardWebhooksAnyOfMultipleSignatures(t *testing.T) {
	ch, wh := standardWebhooksChannel(testSigningToken)
	body := `{}`
	ts := nowTS()
	good := standardWebhooksSignature(testSigningKey, "msg_1", ts, body)
	stale := standardWebhooksSignature([]byte("rotated-out-key"), "msg_1", ts, body)
	sig := "v2,abc " + stale + " " + good
	if code := serveSigned(t, ch, wh, body, standardHeaders("msg_1", ts, sig)); code != http.StatusOK {
		t.Fatalf("got status %d, want 200 when one of several signatures matches", code)
	}
}

func TestWebhookSignatureStandardWebhooksRejects(t *testing.T) {
	body := `{"object_kind":"note"}`
	ts := nowTS()
	good := standardWebhooksSignature(testSigningKey, "msg_1", ts, body)
	old := strconv.FormatInt(time.Now().Add(-6*time.Minute).Unix(), 10)
	future := strconv.FormatInt(time.Now().Add(6*time.Minute).Unix(), 10)
	cases := map[string]map[string]string{
		"missing all headers":   {},
		"missing signature":     {"webhook-id": "msg_1", "webhook-timestamp": ts},
		"missing id":            {"webhook-timestamp": ts, "webhook-signature": good},
		"missing timestamp":     {"webhook-id": "msg_1", "webhook-signature": good},
		"wrong key":             standardHeaders("msg_1", ts, standardWebhooksSignature([]byte("nope"), "msg_1", ts, body)),
		"tampered body":         standardHeaders("msg_1", ts, standardWebhooksSignature(testSigningKey, "msg_1", ts, body+"x")),
		"id swapped":            standardHeaders("msg_2", ts, good),
		"stale timestamp":       standardHeaders("msg_1", old, standardWebhooksSignature(testSigningKey, "msg_1", old, body)),
		"future timestamp":      standardHeaders("msg_1", future, standardWebhooksSignature(testSigningKey, "msg_1", future, body)),
		"non-numeric timestamp": standardHeaders("msg_1", "now", standardWebhooksSignature(testSigningKey, "msg_1", "now", body)),
		"v2 version only":       standardHeaders("msg_1", ts, "v2,"+strings.TrimPrefix(good, "v1,")),
		"no version":            standardHeaders("msg_1", ts, strings.TrimPrefix(good, "v1,")),
		"empty v1":              standardHeaders("msg_1", ts, "v1,"),
		"raw token sent":        standardHeaders("msg_1", ts, testSigningToken),
	}
	for name, headers := range cases {
		ch, wh := standardWebhooksChannel(testSigningToken)
		if code := serveSigned(t, ch, wh, body, headers); code != http.StatusUnauthorized {
			t.Errorf("%s: got status %d, want 401", name, code)
		}
	}
}

func TestWebhookSignatureStandardWebhooksUndecodableTokenFailsClosed(t *testing.T) {
	for _, token := range []string{"whsec_!!!not-base64!!!", "", "whsec_"} {
		ch, wh := standardWebhooksChannel(token)
		body := `{}`
		ts := nowTS()
		sig := standardWebhooksSignature(nil, "msg_1", ts, body)
		if code := serveSigned(t, ch, wh, body, standardHeaders("msg_1", ts, sig)); code != http.StatusUnauthorized {
			t.Errorf("token %q: got status %d, want 401", token, code)
		}
	}
}

func TestWebhookSignatureCombinedWithSecretHeaderRequiresBoth(t *testing.T) {
	body := `{}`
	sig := githubSignature("gh-secret", body)

	ch, wh := githubChannel("gh-secret")
	ch.webhookSecretValue = "static"
	wh.SecretHeader = "X-Token"
	if code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": sig}); code != http.StatusUnauthorized {
		t.Errorf("signature only: got status %d, want 401", code)
	}

	ch, wh = githubChannel("gh-secret")
	ch.webhookSecretValue = "static"
	wh.SecretHeader = "X-Token"
	if code := serveSigned(t, ch, wh, body, map[string]string{"X-Token": "static"}); code != http.StatusUnauthorized {
		t.Errorf("secret header only: got status %d, want 401", code)
	}

	ch, wh = githubChannel("gh-secret")
	ch.webhookSecretValue = "static"
	wh.SecretHeader = "X-Token"
	if code := serveSigned(t, ch, wh, body, map[string]string{"X-Token": "static", "X-Hub-Signature-256": sig}); code != http.StatusOK {
		t.Errorf("both: got status %d, want 200", code)
	}
}

func TestWebhookSignatureRejectedRequestNeverReachesInbox(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel([]string{"message"}, inbox)
	ch.webhookSignatureKey = resolveSignatureKey("github", "gh-secret")
	wh := &WebhookInboundSpec{SignatureScheme: "github", ResponseCode: 200}
	body := `{"type":"message","text":"hi","conversation":{"id":"c1"},"from":{"id":"u1"},"id":"m1"}`
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("wrong", body)})
	if code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", code)
	}
	ch.wg.Wait()
	select {
	case msg := <-inbox:
		t.Fatalf("rejected request reached inbox: %+v", msg)
	default:
	}
}

func TestWebhookSignatureBodyStillProcessedAfterVerification(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel([]string{"message"}, inbox)
	ch.webhookSignatureKey = resolveSignatureKey("github", "gh-secret")
	wh := &WebhookInboundSpec{SignatureScheme: "github", ResponseCode: 200}
	body := `{"type":"message","text":"hi","conversation":{"id":"c1"},"from":{"id":"u1"},"id":"m1"}`
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("gh-secret", body)})
	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
	ch.wg.Wait()
	select {
	case msg := <-inbox:
		if msg.Content != "hi" {
			t.Errorf("content %q, want %q", msg.Content, "hi")
		}
	default:
		t.Fatal("verified request did not reach inbox")
	}
}

func TestWebhookSignatureBodyAtLimitIsVerified(t *testing.T) {
	ch, wh := githubChannel("topsecret")
	body := strings.Repeat("a", maxWebhookBodyBytes)
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("topsecret", body)})
	if code != http.StatusOK {
		t.Errorf("body of exactly %d bytes: got status %d, want %d", maxWebhookBodyBytes, code, http.StatusOK)
	}
}

func TestWebhookSignatureBodyOverLimitReturns413(t *testing.T) {
	ch, wh := githubChannel("topsecret")
	body := strings.Repeat("a", maxWebhookBodyBytes+1)
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("topsecret", body)})
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("correctly signed body over the cap: got status %d, want %d", code, http.StatusRequestEntityTooLarge)
	}
}

func TestWebhookSignatureBodyOverLimitSignedOverTruncatedPrefixReturns413(t *testing.T) {
	ch, wh := githubChannel("topsecret")
	body := strings.Repeat("a", maxWebhookBodyBytes+1)
	code := serveSigned(t, ch, wh, body, map[string]string{"X-Hub-Signature-256": githubSignature("topsecret", body[:maxWebhookBodyBytes])})
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the cap signed over its truncated prefix: got status %d, want %d", code, http.StatusRequestEntityTooLarge)
	}
}

func TestWebhookBodyOverLimitWithoutAuthReturns413(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel([]string{"message"}, inbox)
	wh := &WebhookInboundSpec{ResponseCode: 200}
	code := serveSigned(t, ch, wh, strings.Repeat("a", maxWebhookBodyBytes+1), nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("unauthenticated body over the cap: got status %d, want %d", code, http.StatusRequestEntityTooLarge)
	}
	if len(inbox) != 0 {
		t.Errorf("oversized body must not reach the inbox, got %d messages", len(inbox))
	}
}
