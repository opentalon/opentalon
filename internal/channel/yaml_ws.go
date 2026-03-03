package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// runInit executes the given init steps, storing results in selfVars.
func (ch *YAMLChannel) runInit(steps []InitStep) error {
	for _, step := range steps {
		contexts := ch.buildContexts()
		url := substituteTemplate(step.URL, contexts)

		var body io.Reader
		if step.Body != "" {
			body = strings.NewReader(substituteTemplate(step.Body, contexts))
		}

		req, err := http.NewRequestWithContext(ch.ctx, strings.ToUpper(step.Method), url, body)
		if err != nil {
			return fmt.Errorf("init %s: build request: %w", step.Name, err)
		}
		for k, v := range step.Headers {
			req.Header.Set(k, substituteTemplate(v, contexts))
		}

		resp, err := ch.client.Do(req)
		if err != nil {
			return fmt.Errorf("init %s: request: %w", step.Name, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("init %s: HTTP %d: %s", step.Name, resp.StatusCode, bytes.TrimSpace(respBody))
		}

		// Parse JSON response and store fields
		if len(step.Store) > 0 {
			var result map[string]interface{}
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("init %s: parse response: %w", step.Name, err)
			}
			for selfKey, jsonField := range step.Store {
				if val, ok := result[jsonField]; ok {
					ch.selfVars[selfKey] = fmt.Sprintf("%v", val)
				}
			}
		}

		log.Printf("yaml-channel: init %s done", step.Name)
	}
	return nil
}

// reRunInit re-runs only the named init steps (for reconnect).
func (ch *YAMLChannel) reRunInit(names []string) error {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	var steps []InitStep
	for _, s := range ch.spec.Init {
		if nameSet[s.Name] {
			steps = append(steps, s)
		}
	}
	return ch.runInit(steps)
}

// wsLoop manages the WebSocket lifecycle including reconnection.
func (ch *YAMLChannel) wsLoop() {
	backoff := ch.spec.Connection.Reconnect.BackoffInitial
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := ch.spec.Connection.Reconnect.BackoffMax
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}

	for {
		err := ch.connectAndRead()
		if ch.ctx.Err() != nil {
			return // context cancelled, shutting down
		}

		if !ch.spec.Connection.Reconnect.Enabled {
			log.Printf("yaml-channel: %s disconnected (reconnect disabled): %v", ch.spec.ID, err)
			return
		}

		log.Printf("yaml-channel: %s disconnected, reconnecting in %v: %v", ch.spec.ID, backoff, err)

		select {
		case <-ch.ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Re-run specified init steps to get fresh connection URL
		if len(ch.spec.Connection.Reconnect.ReInit) > 0 {
			if err := ch.reRunInit(ch.spec.Connection.Reconnect.ReInit); err != nil {
				log.Printf("yaml-channel: %s re-init failed: %v", ch.spec.ID, err)
				// Continue with backoff
			}
		}

		// Exponential backoff
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectAndRead dials the WebSocket and runs the read loop until disconnect.
func (ch *YAMLChannel) connectAndRead() error {
	url := substituteTemplate(ch.spec.Connection.URL, ch.buildContexts())
	if url == "" {
		return fmt.Errorf("empty WebSocket URL")
	}

	conn, _, err := websocket.Dial(ch.ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()

	log.Printf("yaml-channel: %s connected to WebSocket", ch.spec.ID)

	for {
		_, data, err := conn.Read(ch.ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		ch.handleFrame(conn, data)
	}
}

// handleFrame processes a single WebSocket frame.
func (ch *YAMLChannel) handleFrame(conn *websocket.Conn, data []byte) {
	var frame map[string]interface{}
	if err := json.Unmarshal(data, &frame); err != nil {
		log.Printf("yaml-channel: %s: invalid JSON frame: %v", ch.spec.ID, err)
		return
	}

	// Build frame context for ack templates
	frameCtx := flattenToStringMap(frame)

	// Send ack if configured and condition met
	if ch.spec.Inbound.Ack.When != "" {
		if _, ok := frame[ch.spec.Inbound.Ack.When]; ok {
			contexts := ch.buildContexts()
			contexts["frame"] = frameCtx
			ackPayload := substituteTemplate(ch.spec.Inbound.Ack.Send, contexts)
			if err := conn.Write(ch.ctx, websocket.MessageText, []byte(ackPayload)); err != nil {
				log.Printf("yaml-channel: %s: ack write error: %v", ch.spec.ID, err)
			}
		}
	}

	// Navigate to the event object
	event := navigatePath(frame, ch.spec.Inbound.EventPath)
	if event == nil {
		return
	}

	// Check event type
	eventType, _ := event["type"].(string)
	if !ch.shouldProcess(event, eventType) {
		return
	}

	// Apply skip rules
	if ch.shouldSkip(event) {
		return
	}

	// Extract fields via mapping
	eventCtx := flattenToStringMap(event)
	msg := ch.extractMessage(event, eventCtx)

	// Apply transforms
	msg.Content = ch.applyTransforms(msg.Content, eventCtx)

	// Dedup
	if ch.dedup != nil && ch.spec.Inbound.Dedup.Key != "" {
		contexts := ch.buildContexts()
		contexts["event"] = eventCtx
		dedupKey := substituteTemplate(ch.spec.Inbound.Dedup.Key, contexts)
		if ch.dedup.IsDuplicate(dedupKey) {
			return
		}
	}

	// Run on_receive hooks (fire-and-forget)
	msgCtx := map[string]string{
		"conversation_id": msg.ConversationID,
		"thread_id":       msg.ThreadID,
		"content":         msg.Content,
		"sender_id":       msg.SenderID,
	}
	for k, v := range msg.Metadata {
		msgCtx["metadata."+k] = v
	}
	ch.runHooks(ch.spec.Hooks.OnReceive, msgCtx)

	// Send to inbox
	select {
	case ch.inbox <- msg:
	case <-ch.ctx.Done():
	}
}

// shouldProcess checks if the event type matches event_types or always_process_when.
func (ch *YAMLChannel) shouldProcess(event map[string]interface{}, eventType string) bool {
	// Check always_process_when first
	if apw := ch.spec.Inbound.AlwaysProcessWhen; apw != nil {
		val := getStringField(event, apw.Field)
		expected := substituteTemplate(apw.Equals, ch.buildContexts())
		if val == expected {
			return true
		}
	}

	if len(ch.spec.Inbound.EventTypes) == 0 {
		return true
	}

	for _, t := range ch.spec.Inbound.EventTypes {
		if eventType == t {
			return true
		}
	}
	return false
}

// shouldSkip evaluates skip rules against the event.
func (ch *YAMLChannel) shouldSkip(event map[string]interface{}) bool {
	contexts := ch.buildContexts()

	for _, rule := range ch.spec.Inbound.Skip {
		val := getStringField(event, rule.Field)

		if rule.Equals != "" {
			expected := substituteTemplate(rule.Equals, contexts)
			if val == expected {
				return true
			}
			continue
		}

		if rule.NotEmpty != nil && *rule.NotEmpty {
			if val != "" {
				// Check exceptions
				if len(rule.Except) > 0 {
					excepted := false
					for _, exc := range rule.Except {
						if val == exc {
							excepted = true
							break
						}
					}
					if excepted {
						continue // exception matched, don't skip
					}
				}
				return true
			}
		}
	}
	return false
}

// extractMessage builds an InboundMessage from the event using the mapping spec.
func (ch *YAMLChannel) extractMessage(event map[string]interface{}, eventCtx map[string]string) pkg.InboundMessage {
	msg := pkg.InboundMessage{
		ChannelID:      ch.spec.ID,
		ConversationID: ch.getMappedField(event, ch.spec.Inbound.Mapping.ConversationID),
		SenderID:       ch.getMappedField(event, ch.spec.Inbound.Mapping.SenderID),
		Content:        ch.getMappedField(event, ch.spec.Inbound.Mapping.Content),
		ThreadID:       ch.getMappedField(event, ch.spec.Inbound.Mapping.ThreadID),
		Timestamp:      time.Now(),
	}

	if len(ch.spec.Inbound.Mapping.Metadata) > 0 {
		msg.Metadata = make(map[string]string, len(ch.spec.Inbound.Mapping.Metadata))
		for metaKey, eventField := range ch.spec.Inbound.Mapping.Metadata {
			msg.Metadata[metaKey] = getStringField(event, eventField)
		}
	}

	return msg
}

// getMappedField resolves a MappingField against an event object.
func (ch *YAMLChannel) getMappedField(event map[string]interface{}, mf MappingField) string {
	if mf.Field == "" {
		return ""
	}
	val := getStringField(event, mf.Field)
	if val == "" && mf.Fallback != "" {
		val = getStringField(event, mf.Fallback)
	}
	return val
}

// applyTransforms runs text transforms on content.
func (ch *YAMLChannel) applyTransforms(content string, eventCtx map[string]string) string {
	contexts := ch.buildContexts()
	contexts["event"] = eventCtx

	for _, t := range ch.spec.Inbound.Transforms {
		switch t.Type {
		case "replace":
			pattern := substituteTemplate(t.Pattern, contexts)
			replacement := substituteTemplate(t.Replacement, contexts)
			content = strings.ReplaceAll(content, pattern, replacement)
		case "trim":
			content = strings.TrimSpace(content)
		}
	}
	return content
}

// runHooks runs HTTP call hooks in the background. The hookMsg context
// is merged with self/config/env for template resolution.
func (ch *YAMLChannel) runHooks(hooks []HTTPCallSpec, msgCtx map[string]string) {
	if len(hooks) == 0 {
		return
	}
	// Copy msgCtx for goroutine safety
	copied := make(map[string]string, len(msgCtx))
	for k, v := range msgCtx {
		copied[k] = v
	}
	go func() {
		for _, hook := range hooks {
			contexts := ch.buildContexts()
			// "event" and "msg" both resolve to the message context
			contexts["event"] = copied
			contexts["msg"] = copied

			// Check "when" condition
			if hook.When != "" {
				resolved := substituteTemplate(hook.When, contexts)
				if resolved == "" {
					continue
				}
			}

			if err := ch.doHTTPCall(ch.ctx, hook, contexts); err != nil {
				log.Printf("yaml-channel: %s hook error: %v", ch.spec.ID, err)
			}
		}
	}()
}

// doHTTPCall executes a single templated HTTP call.
func (ch *YAMLChannel) doHTTPCall(ctx context.Context, call HTTPCallSpec, contexts map[string]map[string]string) error {
	url := substituteTemplate(call.URL, contexts)
	if url == "" {
		return fmt.Errorf("empty URL after template substitution")
	}

	var body io.Reader
	if call.Body != "" {
		// JSON bodies need escaped values to avoid breaking the JSON structure.
		isJSON := false
		for _, v := range call.Headers {
			if strings.EqualFold(v, "application/json") {
				isJSON = true
				break
			}
		}
		var resolved string
		if isJSON {
			resolved = substituteTemplateJSON(call.Body, contexts)
		} else {
			resolved = substituteTemplate(call.Body, contexts)
		}
		body = strings.NewReader(resolved)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(call.Method), url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for k, v := range call.Headers {
		req.Header.Set(k, substituteTemplate(v, contexts))
	}

	resp, err := ch.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	return nil
}

// navigatePath walks a dot-separated path into a nested map.
// Returns nil if the path doesn't lead to a map.
func navigatePath(obj map[string]interface{}, path string) map[string]interface{} {
	if path == "" {
		return obj
	}
	parts := strings.Split(path, ".")
	current := obj
	for _, part := range parts {
		val, ok := current[part]
		if !ok {
			return nil
		}
		next, ok := val.(map[string]interface{})
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

// getStringField extracts a string value from a map by key.
func getStringField(m map[string]interface{}, key string) string {
	val, ok := m[key]
	if !ok || val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case float64:
		// JSON numbers — format without scientific notation
		return fmt.Sprintf("%g", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// flattenToStringMap converts a map[string]interface{} to map[string]string,
// converting values to their string representation.
func flattenToStringMap(m map[string]interface{}) map[string]string {
	result := make(map[string]string, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case string:
			result[k] = val
		case float64:
			result[k] = fmt.Sprintf("%g", val)
		case bool:
			result[k] = fmt.Sprintf("%t", val)
		case nil:
			result[k] = ""
		default:
			// For nested objects, marshal back to JSON
			b, _ := json.Marshal(val)
			result[k] = string(b)
		}
	}
	return result
}
