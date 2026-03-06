package actor

import (
	"context"
	"testing"
)

func TestWithSessionID_SessionID(t *testing.T) {
	ctx := context.Background()
	if SessionID(ctx) != "" {
		t.Errorf("SessionID(background) = %q; want \"\"", SessionID(ctx))
	}
	ctx = WithSessionID(ctx, "sess-1")
	if got := SessionID(ctx); got != "sess-1" {
		t.Errorf("SessionID(WithSessionID(_, \"sess-1\")) = %q; want sess-1", got)
	}
	ctx = WithSessionID(ctx, "")
	if SessionID(ctx) != "sess-1" {
		t.Errorf("WithSessionID(_, \"\") should not overwrite; SessionID = %q", SessionID(ctx))
	}
}

func TestWithActor_Actor(t *testing.T) {
	ctx := context.Background()
	if Actor(ctx) != "" {
		t.Errorf("Actor(background) = %q; want \"\"", Actor(ctx))
	}
	ctx = WithActor(ctx, "channel:user")
	if got := Actor(ctx); got != "channel:user" {
		t.Errorf("Actor(WithActor(_, \"channel:user\")) = %q; want channel:user", got)
	}
}
