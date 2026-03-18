package channel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// pollingLoop periodically calls the configured endpoint and processes each
// element in the response array as an inbound event.
func (ch *YAMLChannel) pollingLoop() {
	poll := ch.spec.Inbound.Polling

	interval := poll.Interval
	if interval <= 0 {
		interval = time.Second
	}

	method := strings.ToUpper(poll.Method)
	if method == "" {
		method = "GET"
	}

	// Use a longer timeout for polling — APIs like Telegram use long polling
	// where the server holds the connection open (e.g. 30s), so the HTTP
	// client timeout must be longer than the server-side timeout.
	pollClient := &http.Client{Timeout: 60 * time.Second}

	// Poll immediately on start, then on tick
	ch.doPollOnce(method, pollClient)

	for {
		select {
		case <-ch.ctx.Done():
			return
		case <-time.After(interval):
			ch.doPollOnce(method, pollClient)
		}
	}
}

// pollOnce executes a single poll request and processes all events in the response.
func (ch *YAMLChannel) doPollOnce(method string, client *http.Client) {
	poll := ch.spec.Inbound.Polling
	contexts := ch.buildContexts()

	url := substituteTemplate(poll.URL, contexts)
	if url == "" {
		log.Printf("yaml-channel: %s: polling URL is empty after template substitution", ch.spec.ID)
		return
	}

	var bodyReader io.Reader
	if poll.Body != "" {
		bodyReader = strings.NewReader(substituteTemplate(poll.Body, contexts))
	}

	req, err := http.NewRequestWithContext(ch.ctx, method, url, bodyReader)
	if err != nil {
		log.Printf("yaml-channel: %s: poll build request: %v", ch.spec.ID, err)
		return
	}
	for k, v := range poll.Headers {
		req.Header.Set(k, substituteTemplate(v, contexts))
	}

	resp, err := client.Do(req)
	if err != nil {
		if ch.ctx.Err() != nil {
			return // shutting down
		}
		log.Printf("yaml-channel: %s: poll request: %v", ch.spec.ID, err)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("yaml-channel: %s: poll HTTP %d: %s", ch.spec.ID, resp.StatusCode, respBody)
		return
	}

	// Parse JSON response
	var raw map[string]interface{}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		log.Printf("yaml-channel: %s: poll parse response: %v", ch.spec.ID, err)
		return
	}

	// Navigate to result array
	events, err := ch.extractEvents(raw, poll.ResultPath)
	if err != nil {
		log.Printf("yaml-channel: %s: %v", ch.spec.ID, err)
		return
	}

	if len(events) == 0 {
		return
	}

	// Track max cursor value for offset-based APIs (e.g. Telegram update_id)
	var maxCursor int64 = -1
	cursorField := poll.CursorField

	// Process each event
	for _, event := range events {
		eventMap, ok := event.(map[string]interface{})
		if !ok {
			continue
		}

		// Track cursor before processing (cursor_field is on the wrapper, not the nested event)
		if cursorField != "" {
			if val := getStringField(eventMap, cursorField); val != "" {
				if n, err := strconv.ParseInt(val, 10, 64); err == nil && n > maxCursor {
					maxCursor = n
				}
			}
		}

		ch.processInboundFrame(eventMap)
	}

	// Update poll_offset for next request (cursor + 1)
	if cursorField != "" && maxCursor >= 0 {
		ch.selfVars["poll_offset"] = strconv.FormatInt(maxCursor+1, 10)
	}
}

// extractEvents navigates to the result_path in the response and returns the
// array of events. If result_path is empty, the response itself is treated as
// a single-element array.
func (ch *YAMLChannel) extractEvents(raw map[string]interface{}, resultPath string) ([]interface{}, error) {
	if resultPath == "" {
		return []interface{}{raw}, nil
	}

	// Navigate the dotted path
	parts := strings.Split(resultPath, ".")
	var current interface{} = raw
	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("poll result_path %q: expected object at %q", resultPath, part)
		}
		current = m[part]
	}

	arr, ok := current.([]interface{})
	if !ok {
		return nil, nil // not an array — no events (e.g. empty result)
	}
	return arr, nil
}
