package channel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
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

		// Parse JSON response and store fields (supports dotted paths like "result.id")
		if len(step.Store) > 0 {
			var result map[string]interface{}
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("init %s: parse response: %w", step.Name, err)
			}
			ch.selfMu.Lock()
			for selfKey, jsonField := range step.Store {
				val := getStringField(result, jsonField)
				if val != "" {
					ch.selfVars[selfKey] = val
				}
			}
			ch.selfMu.Unlock()
		}

		slog.Info("yaml-channel init step done", "step", step.Name)
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
			slog.Warn("yaml-channel disconnected, reconnect disabled", "channel", ch.spec.ID, "error", err)
			return
		}

		slog.Warn("yaml-channel disconnected, reconnecting", "channel", ch.spec.ID, "backoff", backoff, "error", err)

		select {
		case <-ch.ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Re-run specified init steps to get fresh connection URL
		if len(ch.spec.Connection.Reconnect.ReInit) > 0 {
			if err := ch.reRunInit(ch.spec.Connection.Reconnect.ReInit); err != nil {
				slog.Warn("yaml-channel re-init failed", "channel", ch.spec.ID, "error", err)
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

	slog.Info("yaml-channel connected to WebSocket", "channel", ch.spec.ID)

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
		slog.Warn("yaml-channel invalid JSON frame", "channel", ch.spec.ID, "error", err)
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
				slog.Warn("yaml-channel ack write error", "channel", ch.spec.ID, "error", err)
			}
		}
	}

	ch.processInboundFrame(frame)
}

// processInboundData processes raw JSON data received from any inbound source
// (WebSocket frame or HTTP webhook body).
func (ch *YAMLChannel) processInboundData(data []byte) {
	var frame map[string]interface{}
	if err := json.Unmarshal(data, &frame); err != nil {
		slog.Warn("yaml-channel invalid JSON", "channel", ch.spec.ID, "error", err)
		return
	}
	ch.processInboundFrame(frame)
}

// processInboundFrame processes a parsed inbound frame from any source.
func (ch *YAMLChannel) processInboundFrame(frame map[string]interface{}) {
	// Navigate to the event object
	event := navigatePath(frame, ch.spec.Inbound.EventPath)
	if event == nil {
		slog.Debug("yaml-channel frame dropped: event_path not found", "channel", ch.spec.ID, "event_path", ch.spec.Inbound.EventPath)
		return
	}

	// Check event type
	eventType, _ := event["type"].(string)
	if !ch.shouldProcess(event, eventType) {
		slog.Debug("yaml-channel frame dropped: event type not in event_types", "channel", ch.spec.ID, "event_type", eventType)
		return
	}

	// Apply process_when allowlist (if configured, at least one must match)
	if !ch.matchesProcessWhen(event) {
		slog.Debug("yaml-channel frame dropped: no process_when rule matched", "channel", ch.spec.ID, "event_type", eventType)
		return
	}

	// Apply skip rules
	if ch.shouldSkip(event) {
		slog.Debug("yaml-channel frame dropped: matched skip rule", "channel", ch.spec.ID, "event_type", eventType)
		return
	}

	// Extract fields via mapping
	eventCtx := flattenToStringMap(event)
	msg := ch.extractMessage(event, eventCtx)

	// Resolve media: detect non-text messages, download files or inject descriptions
	if len(ch.spec.Inbound.Media) > 0 {
		ch.resolveMedia(event, eventCtx, &msg)
	}

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

// matchesProcessWhen checks process_when allowlist rules. If no rules are
// configured, returns true (process everything). If rules are configured,
// at least one must match (OR logic).
func (ch *YAMLChannel) matchesProcessWhen(event map[string]interface{}) bool {
	rules := ch.spec.Inbound.ProcessWhen
	if len(rules) == 0 {
		return true // no allowlist = process all
	}

	contexts := ch.buildContexts()

	for _, rule := range rules {
		val := getStringField(event, rule.Field)

		if rule.Equals != "" {
			expected := substituteTemplate(rule.Equals, contexts)
			if val == expected {
				return true
			}
		}

		if rule.Contains != "" {
			needle := substituteTemplate(rule.Contains, contexts)
			if strings.Contains(val, needle) {
				return true
			}
		}

		if rule.NotEmpty != nil && *rule.NotEmpty && val != "" {
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

	if ch.spec.Inbound.Mapping.Files != "" {
		if raw, ok := event[ch.spec.Inbound.Mapping.Files]; ok {
			if arr, ok := raw.([]interface{}); ok {
				for _, item := range arr {
					if fileMap, ok := item.(map[string]interface{}); ok {
						if fa := decodeFileAttachment(fileMap); fa != nil {
							msg.Files = append(msg.Files, *fa)
						}
					}
				}
			}
		}
	}

	return msg
}

// maxFileAttachmentSize is the maximum allowed size for a decoded file attachment.
// Base64 encoding adds ~33% overhead, so the encoded string can be up to 4/3 this size.
const maxFileAttachmentSize = 20 * 1024 * 1024 // 20 MB decoded

// decodeFileAttachment parses a file object from an inbound JSON event.
// The object must have mime_type and data (base64-encoded) fields; name is optional.
func decodeFileAttachment(m map[string]interface{}) *pkg.FileAttachment {
	name, _ := m["name"].(string)
	mimeType, _ := m["mime_type"].(string)
	dataStr, _ := m["data"].(string)
	sizeF, _ := m["size"].(float64)

	if dataStr == "" {
		return nil
	}
	if mimeType == "" {
		slog.Warn("yaml-channel file attachment missing mime_type, skipping")
		return nil
	}
	// Enforce size limit before decoding to prevent large allocations.
	// Base64 encodes 3 bytes as 4 chars, so encoded length ≈ decoded * 4/3.
	if len(dataStr) > maxFileAttachmentSize*4/3 {
		slog.Warn("yaml-channel file attachment too large, skipping", "encoded_bytes", len(dataStr))
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		// Try URL-safe base64 (no padding)
		data, err = base64.RawURLEncoding.DecodeString(dataStr)
		if err != nil {
			slog.Warn("yaml-channel file attachment decode error", "error", err)
			return nil
		}
	}
	size := int64(sizeF)
	if size == 0 {
		size = int64(len(data))
	}
	return &pkg.FileAttachment{
		Name:     name,
		MimeType: mimeType,
		Data:     data,
		Size:     size,
	}
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
			if t.Regex {
				re, err := regexp.Compile(pattern)
				if err != nil {
					slog.Warn("yaml-channel invalid regex", "channel", ch.spec.ID, "pattern", pattern, "error", err)
				} else {
					content = re.ReplaceAllString(content, replacement)
				}
			} else {
				content = strings.ReplaceAll(content, pattern, replacement)
			}
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
				slog.Warn("yaml-channel hook error", "channel", ch.spec.ID, "error", err)
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
			resolved = stripEmptyJSONValues(resolved)
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

// doHTTPCallCaptureField executes a templated HTTP call and extracts a single
// top-level string field from the JSON response. Returns "" if the field is
// absent or the response isn't JSON. Used to capture message IDs (e.g. Slack's
// "ts") from send responses for streaming updates.
func (ch *YAMLChannel) doHTTPCallCaptureField(ctx context.Context, call HTTPCallSpec, contexts map[string]map[string]string, field string) (string, error) {
	url := substituteTemplate(call.URL, contexts)
	if url == "" {
		return "", fmt.Errorf("empty URL after template substitution")
	}

	var bodyReader io.Reader
	if call.Body != "" {
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
			resolved = stripEmptyJSONValues(resolved)
		} else {
			resolved = substituteTemplate(call.Body, contexts)
		}
		bodyReader = strings.NewReader(resolved)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(call.Method), url, bodyReader)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	for k, v := range call.Headers {
		req.Header.Set(k, substituteTemplate(v, contexts))
	}

	resp, err := ch.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}

	if field == "" {
		return "", nil
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", nil // not JSON, silently ignore
	}
	return getStringField(parsed, field), nil
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
// Supports dotted paths (e.g. "from.id") for nested lookups.
func getStringField(m map[string]interface{}, key string) string {
	// Try exact key first (handles keys that literally contain dots)
	if val, ok := m[key]; ok && val != nil {
		switch v := val.(type) {
		case string:
			return v
		case float64:
			if v == float64(int64(v)) {
				return fmt.Sprintf("%.0f", v)
			}
			return fmt.Sprintf("%g", v)
		default:
			return fmt.Sprintf("%v", v)
		}
	}
	// Navigate dotted path
	parts := strings.SplitN(key, ".", 2)
	if len(parts) == 2 {
		if nested, ok := m[parts[0]].(map[string]interface{}); ok {
			return getStringField(nested, parts[1])
		}
		// Array indexing: "photo.-1.file_id" or "photo.0.width"
		if arr, ok := m[parts[0]].([]interface{}); ok {
			subParts := strings.SplitN(parts[1], ".", 2)
			idx, err := strconv.Atoi(subParts[0])
			if err == nil {
				if idx < 0 {
					idx = len(arr) + idx
				}
				if idx >= 0 && idx < len(arr) {
					if len(subParts) == 2 {
						if nested, ok := arr[idx].(map[string]interface{}); ok {
							return getStringField(nested, subParts[1])
						}
					}
					return fmt.Sprintf("%v", arr[idx])
				}
			}
		}
	}
	return ""
}

// navigateToValue checks whether a dotted path leads to any non-nil value
// (including objects and arrays that getStringField would return "" for).
func navigateToValue(m map[string]interface{}, path string) (interface{}, bool) {
	if val, ok := m[path]; ok && val != nil {
		return val, true
	}
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 2 {
		if nested, ok := m[parts[0]].(map[string]interface{}); ok {
			return navigateToValue(nested, parts[1])
		}
		// Check arrays too (e.g. "photo" is []interface{})
		if arr, ok := m[parts[0]].([]interface{}); ok && len(arr) > 0 {
			return arr, true
		}
	}
	return nil, false
}

// resolveMedia processes media rules for an inbound event. It detects non-text
// message types, downloads binary data (if configured), and injects a text
// description so the LLM can respond naturally to unsupported media types.
// The first matching rule wins.
func (ch *YAMLChannel) resolveMedia(event map[string]interface{}, eventCtx map[string]string, msg *pkg.InboundMessage) {
	for _, rule := range ch.spec.Inbound.Media {
		if _, exists := navigateToValue(event, rule.When); !exists {
			continue
		}
		// Pre-resolve {{event.X.Y.Z}} template references against the raw event,
		// because flattenToStringMap only captures top-level keys.
		enrichEventCtx(event, eventCtx, rule)

		contexts := ch.buildContexts()
		contexts["event"] = eventCtx

		if rule.Resolve != nil {
			file, err := ch.resolveFile(rule.Resolve, contexts, event, eventCtx)
			if err != nil {
				slog.Warn("yaml-channel media resolve failed, falling back to description",
					"channel", ch.spec.ID, "when", rule.When, "error", err)
			} else if file != nil {
				msg.Files = append(msg.Files, *file)
			}
		}
		// Inject description as content if text is still empty
		if msg.Content == "" && rule.Description != "" {
			msg.Content = substituteTemplate(rule.Description, contexts)
		}
		return
	}
}

// enrichEventCtx pre-resolves nested event paths referenced in a media rule's
// templates so that {{event.photo.-1.file_id}} works in the template engine
// (which only does flat map lookups). It scans all template strings in the rule
// for {{event.X}} references and resolves them via getStringField on the raw event.
func enrichEventCtx(event map[string]interface{}, eventCtx map[string]string, rule MediaRule) {
	// Collect all template strings from the rule
	templates := []string{rule.Description}
	if rule.Resolve != nil {
		templates = append(templates, rule.Resolve.MimeType, rule.Resolve.Name)
		for _, step := range rule.Resolve.Steps {
			templates = append(templates, step.URL)
			for _, v := range step.Headers {
				templates = append(templates, v)
			}
		}
	}
	for _, tmpl := range templates {
		for _, match := range contextRe.FindAllStringSubmatch(tmpl, -1) {
			if len(match) == 3 && match[1] == "event" {
				key := match[2]
				if _, exists := eventCtx[key]; !exists {
					if val := getStringField(event, key); val != "" {
						eventCtx[key] = val
					}
				}
			}
		}
	}
}

// resolveFile executes the resolve steps to download binary media data.
// Intermediate steps (with Store) parse JSON responses; the final step (without
// Store) captures the raw body. Returns nil if no binary data was obtained.
func (ch *YAMLChannel) resolveFile(spec *MediaResolveSpec, contexts map[string]map[string]string, event map[string]interface{}, eventCtx map[string]string) (*pkg.FileAttachment, error) {
	resolveCtx := make(map[string]string)
	contexts["resolve"] = resolveCtx

	var data []byte
	for _, step := range spec.Steps {
		url := substituteTemplate(step.URL, contexts)
		if url == "" {
			return nil, fmt.Errorf("empty URL after template substitution")
		}

		method := strings.ToUpper(step.Method)
		if method == "" {
			method = "GET"
		}

		req, err := http.NewRequestWithContext(ch.ctx, method, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		for k, v := range step.Headers {
			req.Header.Set(k, substituteTemplate(v, contexts))
		}

		resp, err := ch.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request %s: %w", url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, bytes.TrimSpace(body))
		}

		if len(step.Store) > 0 {
			var result map[string]interface{}
			if err := json.Unmarshal(body, &result); err != nil {
				return nil, fmt.Errorf("parse response from %s: %w", url, err)
			}
			for selfKey, jsonField := range step.Store {
				if val := getStringField(result, jsonField); val != "" {
					resolveCtx[selfKey] = val
				}
			}
		} else {
			data = body
		}
	}

	if data == nil {
		return nil, fmt.Errorf("no binary data from resolve steps")
	}
	if len(data) > maxFileAttachmentSize {
		return nil, fmt.Errorf("file too large (%d bytes, max %d)", len(data), maxFileAttachmentSize)
	}

	mimeType := substituteTemplate(spec.MimeType, contexts)
	name := substituteTemplate(spec.Name, contexts)

	return &pkg.FileAttachment{
		Name:     name,
		MimeType: mimeType,
		Data:     data,
		Size:     int64(len(data)),
	}, nil
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
			if val == float64(int64(val)) {
				result[k] = fmt.Sprintf("%.0f", val)
			} else {
				result[k] = fmt.Sprintf("%g", val)
			}
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
