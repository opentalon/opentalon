package channel

import (
	"io"
	"log"
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
				log.Printf("yaml-channel: %s: JWT validation failed: %v", ch.spec.ID, err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Read body with 1MB cap
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			log.Printf("yaml-channel: %s: read webhook body: %v", ch.spec.ID, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		if os.Getenv("LOG_LEVEL") == "debug" {
			log.Printf("yaml-channel: %s: webhook %s %s from %s body=%s",
				ch.spec.ID, r.Method, r.URL.Path, r.RemoteAddr, body)
		}

		// Respond immediately (Teams requires fast ack)
		w.WriteHeader(responseCode)

		// Process in goroutine
		go ch.processInboundData(body)
	}
}
