package main

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/channel"
	chanpkg "github.com/opentalon/opentalon/pkg/channel"
)

// recordChannel is a minimal pkg.Channel that records outbound sends so the
// channelNotifier routing/stamping tests can assert what reached it.
type recordChannel struct {
	id   string
	sent []chanpkg.OutboundMessage
}

func (c *recordChannel) ID() string   { return c.id }
func (c *recordChannel) Kind() string { return c.id }
func (c *recordChannel) Capabilities() chanpkg.Capabilities {
	return chanpkg.Capabilities{ID: c.id}
}
func (c *recordChannel) Start(_ context.Context, _ chan<- chanpkg.InboundMessage) error { return nil }
func (c *recordChannel) Send(_ context.Context, msg chanpkg.OutboundMessage) error {
	c.sent = append(c.sent, msg)
	return nil
}
func (c *recordChannel) Stop() error { return nil }

// newNotifierFixture builds a channelNotifier over a real registry with one
// registered stub channel ("ws"). channelNotifier is a small struct, so the
// tests construct it directly.
func newNotifierFixture(t *testing.T) (*channelNotifier, *recordChannel) {
	t.Helper()
	reg := channel.NewRegistry(func(_ context.Context, _ string, _ chanpkg.InboundMessage) (chanpkg.OutboundMessage, error) {
		return chanpkg.OutboundMessage{}, nil
	})
	t.Cleanup(reg.StopAll)
	ch := &recordChannel{id: "ws"}
	if err := reg.Register(ch); err != nil {
		t.Fatalf("registering stub channel: %v", err)
	}
	return &channelNotifier{reg: reg}, ch
}

func sentFrame(t *testing.T, ch *recordChannel) chanpkg.OutboundMessage {
	t.Helper()
	if len(ch.sent) != 1 {
		t.Fatalf("expected exactly one delivered frame, got %d", len(ch.sent))
	}
	return ch.sent[0]
}

// TestChannelNotifier_ProfileKey_RoutesAndStampsOwner: a profile-scoped key
// "<entity>:<channel>:<conversation>" routes to the channel/conversation from
// the right AND stamps the leading entity as the conversation owner.
func TestChannelNotifier_ProfileKey_RoutesAndStampsOwner(t *testing.T) {
	n, ch := newNotifierFixture(t)
	if err := n.SendToSession(context.Background(), "e1:ws:c1", chanpkg.OutboundMessage{Content: "hi"}); err != nil {
		t.Fatalf("SendToSession: %v", err)
	}
	got := sentFrame(t, ch)
	if got.ConversationID != "c1" {
		t.Errorf("ConversationID = %q, want c1", got.ConversationID)
	}
	if owner := got.Metadata[chanpkg.OwnerEntityMetadataKey]; owner != "e1" {
		t.Errorf("Metadata[%s] = %q, want e1", chanpkg.OwnerEntityMetadataKey, owner)
	}
}

// TestChannelNotifier_AnonymousKey_NoStamp: a 2-part anonymous key carries no
// entity, so no owner may be stamped (the empty-owner fallback on the channel
// side delivers the frame anyway).
func TestChannelNotifier_AnonymousKey_NoStamp(t *testing.T) {
	n, ch := newNotifierFixture(t)
	if err := n.SendToSession(context.Background(), "ws:c1", chanpkg.OutboundMessage{Content: "hi"}); err != nil {
		t.Fatalf("SendToSession: %v", err)
	}
	got := sentFrame(t, ch)
	if got.ConversationID != "c1" {
		t.Errorf("ConversationID = %q, want c1", got.ConversationID)
	}
	if _, ok := got.Metadata[chanpkg.OwnerEntityMetadataKey]; ok {
		t.Errorf("anonymous frame must not carry the owner stamp, got %+v", got.Metadata)
	}
}

// TestChannelNotifier_AnonymousThreadKey_RoutesWithoutStamp: a 3-part
// anonymous threaded key "<channel>:<conversation>:<thread>" makes the
// from-right-2 lookup fail (it lands on the conversation id), so routing goes
// through the registry-match fallback — and its parts[0] is a channel id, not
// an owner entity, so it must NOT be stamped as one.
func TestChannelNotifier_AnonymousThreadKey_RoutesWithoutStamp(t *testing.T) {
	n, ch := newNotifierFixture(t)
	if err := n.SendToSession(context.Background(), "ws:c1:t1", chanpkg.OutboundMessage{Content: "hi"}); err != nil {
		t.Fatalf("SendToSession: %v", err)
	}
	got := sentFrame(t, ch)
	if got.ConversationID != "c1" {
		t.Errorf("ConversationID = %q, want c1", got.ConversationID)
	}
	if got.ThreadID != "t1" {
		t.Errorf("ThreadID = %q, want t1", got.ThreadID)
	}
	if _, ok := got.Metadata[chanpkg.OwnerEntityMetadataKey]; ok {
		t.Errorf("anonymous threaded frame must not carry the owner stamp, got %+v", got.Metadata)
	}
}
