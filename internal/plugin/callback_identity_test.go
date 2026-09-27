package plugin

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/pkg/plugin/contextargs"
)

// A callback that re-identifies the nested chain (the agents plugin running
// a workflow as its owner) replaces the profile on the context. It must not
// drop the run's labels while doing so: a leaf tool inside a system run has
// to keep learning that it is one, or a consumer that narrows such runs
// would silently see them as interactive turns.
func TestApplyCallbackIdentityPreservesRunLabels(t *testing.T) {
	outer := profile.WithProfile(context.Background(), &profile.Profile{
		EntityID: "caller", Group: "g-caller", Kind: profile.KindSystem, SystemSource: "csv_mapping",
	})
	in := map[string]string{
		contextargs.CallbackEntityID: "owner",
		contextargs.CallbackGroupID:  "g-owner",
		"text":                       "x",
	}

	ctx, args := applyCallbackIdentity(outer, in)

	p := profile.FromContext(ctx)
	if p == nil {
		t.Fatal("no profile on the callback ctx")
	}
	if p.EntityID != "owner" || p.Group != "g-owner" {
		t.Errorf("identity = (%q, %q), want the callback's (owner, g-owner)", p.EntityID, p.Group)
	}
	if p.Kind != profile.KindSystem || p.SystemSource != "csv_mapping" {
		t.Errorf("labels = (%q, %q), want the outer run's (system, csv_mapping)", p.Kind, p.SystemSource)
	}
	if got := actor.Actor(ctx); got != "owner" {
		t.Errorf("actor = %q, want owner", got)
	}
	if _, leaked := args[contextargs.CallbackEntityID]; leaked || args["text"] != "x" {
		t.Errorf("args = %v, want the reserved keys stripped and the rest untouched", args)
	}

	// No outer profile (a dispatcher-started chain): identity only, no
	// invented labels.
	ctx2, _ := applyCallbackIdentity(context.Background(), map[string]string{contextargs.CallbackEntityID: "owner"})
	if p2 := profile.FromContext(ctx2); p2 == nil || p2.Kind != "" || p2.SystemSource != "" {
		t.Errorf("profile without an outer run = %+v, want identity only", p2)
	}
}

// A callback that moves the nested chain to another session must not carry
// the outer turn's latest-user-message id along: paired with the other
// session's id, a plugin would take it for a new message from the user. A
// callback that names the same session keeps it.
func TestApplyCallbackIdentityDropsLastUserMessageIDOnSessionChange(t *testing.T) {
	outer := actor.WithLastUserMessageID(actor.WithSessionID(context.Background(), "s-outer"), "m-outer")

	ctx, _ := applyCallbackIdentity(outer, map[string]string{contextargs.CallbackSessionID: "s-other"})
	if got := actor.SessionID(ctx); got != "s-other" {
		t.Fatalf("session = %q, want s-other", got)
	}
	if got := actor.LastUserMessageID(ctx); got != "" {
		t.Errorf("last_user_message_id = %q after a session change, want it dropped", got)
	}

	same, _ := applyCallbackIdentity(outer, map[string]string{contextargs.CallbackSessionID: "s-outer"})
	if got := actor.LastUserMessageID(same); got != "m-outer" {
		t.Errorf("last_user_message_id = %q for the same session, want m-outer kept", got)
	}

	// Identity-only callbacks leave the session and its id alone.
	idOnly, _ := applyCallbackIdentity(outer, map[string]string{contextargs.CallbackEntityID: "owner"})
	if got := actor.LastUserMessageID(idOnly); got != "m-outer" {
		t.Errorf("last_user_message_id = %q after an identity-only callback, want m-outer kept", got)
	}
}
