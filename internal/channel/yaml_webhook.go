package channel

import (
	"io"
	"log/slog"
	"net/http"
	"os"
)

// startWebhookInbound registers the HTTP webhook handler and starts the
// shared webhook server. Returns immediately (server runs in background).
func (ch *YAMLChannel) startWebhookInbound(wh *WebhookInboundSpec) error {
	// Create JWT validator if configured
	if wh.ValidateJWT {
		audience := substituteTemplate(wh.Audience, ch.buildContexts())
		ch.jwtValidator = NewJWTValidator(wh.OIDCEndpoint, audience, wh.Issuer)
	}

	path := wh.Path
	if path == "" {
		path = "/api/messages"
	}
	if !wh.ValidateJWT {
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

		// Read body with 1MB cap
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			slog.Warn("yaml-channel read webhook body failed", "channel", ch.spec.ID, "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if os.Getenv("LOG_LEVEL") == "debug" {
			slog.Debug("yaml-channel webhook received", "channel", ch.spec.ID, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "body", string(body))
		}

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
