package channel

import (
	"strings"
	"sync"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// defaultDebounceWindow is how long the debouncer waits for additional
// messages before dispatching. 0 = disabled (opt-in via SetDebounceWindow).
const defaultDebounceWindow = 0

// sessionDebouncer collects rapid-fire messages for the same session and
// merges them into a single InboundMessage before dispatching. This avoids
// burning an LLM round (~98k tokens) for each line when the user sends
// "yes\nalso add barcode\nset price to 50" as three separate WebSocket frames.
type sessionDebouncer struct {
	mu       sync.Mutex
	window   time.Duration
	buffers  map[string]*debounceBuf
	dispatch func(sessionKey string, merged pkg.InboundMessage)
	stopped  bool
}

type debounceBuf struct {
	messages []pkg.InboundMessage
	timer    *time.Timer
}

func newSessionDebouncer(window time.Duration, dispatch func(string, pkg.InboundMessage)) *sessionDebouncer {
	return &sessionDebouncer{
		window:   window,
		buffers:  make(map[string]*debounceBuf),
		dispatch: dispatch,
	}
}

// stop cancels all pending timers and prevents future dispatches.
// Buffered messages are discarded. Call when the dispatch loop exits.
func (d *sessionDebouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.stopped = true
	for key, buf := range d.buffers {
		if buf.timer != nil {
			buf.timer.Stop()
		}
		delete(d.buffers, key)
	}
}

// submit adds a message to the debounce buffer. If no more messages arrive
// within the window, the buffer is flushed as a single merged message.
// Returns true if the message was debounced, false if it should be dispatched
// immediately (e.g. confirmation signals).
func (d *sessionDebouncer) submit(sessionKey string, msg pkg.InboundMessage) bool {
	// Skip debounce for confirmation signals and typing indicators —
	// these must be processed immediately.
	if msg.Metadata["confirmation"] != "" || msg.Metadata["_typing"] == "true" {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.stopped {
		return false
	}

	buf, exists := d.buffers[sessionKey]
	if !exists {
		buf = &debounceBuf{}
		d.buffers[sessionKey] = buf
	}

	buf.messages = append(buf.messages, msg)

	// Reset the timer on each new message.
	if buf.timer != nil {
		buf.timer.Stop()
	}
	buf.timer = time.AfterFunc(d.window, func() {
		d.flush(sessionKey)
	})

	return true
}

// flush merges all buffered messages for a session and dispatches them.
func (d *sessionDebouncer) flush(sessionKey string) {
	d.mu.Lock()
	buf, exists := d.buffers[sessionKey]
	if d.stopped || !exists || len(buf.messages) == 0 {
		d.mu.Unlock()
		return
	}
	messages := buf.messages
	delete(d.buffers, sessionKey)
	d.mu.Unlock()

	merged := mergeMessages(messages)
	d.dispatch(sessionKey, merged)
}

// mergeMessages combines multiple InboundMessages into one.
// Content is joined with newlines. Metadata is merged (last wins).
// Files are concatenated. Identity fields come from the last message.
func mergeMessages(messages []pkg.InboundMessage) pkg.InboundMessage {
	if len(messages) == 1 {
		return messages[0]
	}

	last := messages[len(messages)-1]

	// Join content.
	var parts []string
	for _, m := range messages {
		if c := strings.TrimSpace(m.Content); c != "" {
			parts = append(parts, c)
		}
	}

	// Merge metadata (last wins on conflicts).
	var meta map[string]string
	for _, m := range messages {
		for k, v := range m.Metadata {
			if meta == nil {
				meta = make(map[string]string)
			}
			meta[k] = v
		}
	}

	// Concatenate files.
	var files []pkg.FileAttachment
	for _, m := range messages {
		files = append(files, m.Files...)
	}

	return pkg.InboundMessage{
		ChannelID:      last.ChannelID,
		ConversationID: last.ConversationID,
		ThreadID:       last.ThreadID,
		SenderID:       last.SenderID,
		SenderName:     last.SenderName,
		Content:        strings.Join(parts, "\n"),
		Metadata:       meta,
		Files:          files,
		Timestamp:      last.Timestamp,
	}
}
