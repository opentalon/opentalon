package channel

import (
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const maxWebhookBodyBytes = 1 << 20

// startWebhookInbound registers the HTTP webhook handler and starts the
// shared webhook server. Returns immediately (server runs in background).
func (ch *YAMLChannel) startWebhookInbound(wh *WebhookInboundSpec) error {
	// Create JWT validator if configured
	if wh.ValidateJWT {
		audience := substituteTemplate(wh.Audience, ch.buildContexts())
		ch.jwtValidator = NewJWTValidator(wh.OIDCEndpoint, audience, wh.Issuer)
	}
	if wh.SecretHeader != "" {
		ch.webhookSecretValue = substituteTemplate(wh.SecretValue, ch.buildContexts())
		if ch.webhookSecretValue == "" {
			slog.Error("yaml-channel secret_header is set but secret_value resolved empty; every request will be rejected (fail-closed) — check the referenced env var is set",
				"channel", ch.spec.ID, "secret_header", wh.SecretHeader, "secret_value_template", wh.SecretValue)
		}
	}

	if wh.SignatureScheme != "" {
		ch.webhookSignatureKey = resolveSignatureKey(wh.SignatureScheme, substituteTemplate(wh.SignatureSecret, ch.buildContexts()))
		if ch.webhookSignatureKey == nil {
			slog.Error("yaml-channel signature_secret resolved empty or undecodable; every request will be rejected (fail-closed) — check the referenced env var is set and, for standard_webhooks, is a whsec_<base64> token",
				"channel", ch.spec.ID, "signature_scheme", wh.SignatureScheme, "signature_secret_template", wh.SignatureSecret)
		}
	}

	path := wh.Path
	if path == "" {
		path = "/api/messages"
	}
	if !wh.ValidateJWT && wh.SecretHeader == "" && wh.SignatureScheme == "" {
		slog.Warn("yaml-channel webhook endpoint has no authentication configured", "channel", ch.spec.ID, "path", path)
	}

	handler := ch.buildWebhookHandler(wh)
	return RegisterWebhookRoute(wh.Port, path, handler)
}

// buildWebhookHandler returns the http.HandlerFunc for inbound webhook requests.
func (ch *YAMLChannel) buildWebhookHandler(wh *WebhookInboundSpec) http.HandlerFunc {
	responseCode := wh.ResponseCode
	if responseCode == 0 {
		responseCode = 200
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// JWT validation
		if wh.ValidateJWT && ch.jwtValidator != nil {
			if err := ch.jwtValidator.ValidateRequest(r); err != nil {
				slog.Warn("yaml-channel JWT validation failed", "channel", ch.spec.ID, "error", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Shared-secret header validation (e.g. GitLab's X-Gitlab-Token)
		if wh.SecretHeader != "" {
			got := r.Header.Get(wh.SecretHeader)
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(ch.webhookSecretValue)) != 1 {
				slog.Warn("yaml-channel secret header validation failed", "channel", ch.spec.ID, "header", wh.SecretHeader)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Read body with 1MB cap
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
		if err != nil {
			slog.Warn("yaml-channel read webhook body failed", "channel", ch.spec.ID, "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body) > maxWebhookBodyBytes {
			slog.Warn("yaml-channel webhook body exceeds size limit", "channel", ch.spec.ID, "limit_bytes", maxWebhookBodyBytes, "content_length", r.ContentLength)
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}

		if wh.SignatureScheme != "" {
			if err := verifyWebhookSignature(wh.SignatureScheme, ch.webhookSignatureKey, r.Header, body, time.Now()); err != nil {
				slog.Warn("yaml-channel webhook signature validation failed", "channel", ch.spec.ID, "signature_scheme", wh.SignatureScheme, "error", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		slog.Debug("yaml-channel webhook received", "channel", ch.spec.ID, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "body", string(body))

		// Respond immediately (Teams requires fast ack)
		w.WriteHeader(responseCode)

		// Process in goroutine
		ch.wg.Add(1)
		go func(payload []byte) {
			defer ch.wg.Done()
			ch.processInboundData(payload)
		}(body)
	}
}
