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

// A workflow callback names the account it belongs to via the reserved group
// key. That group has to land on the actor ctx, because the injected
// contextargs.GroupID is resolved from actor.GroupID (see the orchestrator's
// defaultContextArgProviders), not from the profile. Without it a workflow
// run reaches plugins with no group, and the downstream service acts in
// whatever account the owner currently has switched in.
func TestApplyCallbackIdentitySetsActorGroup(t *testing.T) {
	outer := actor.WithGroupID(actor.WithActor(context.Background(), "caller"), "g-caller")
	in := map[string]string{
		contextargs.CallbackEntityID: "owner",
		contextargs.CallbackGroupID:  "g-owner",
		"text":                       "x",
	}

	ctx, args := applyCallbackIdentity(outer, in)

	if got := actor.GroupID(ctx); got != "g-owner" {
		t.Errorf("actor group = %q, want the callback's g-owner", got)
	}
	if got := actor.Actor(ctx); got != "owner" {
		t.Errorf("actor = %q, want owner", got)
	}
	if len(args) != 1 || args["text"] != "x" {
		t.Errorf("args = %v, want only the non-reserved text arg", args)
	}

	// Group-only callback (no entity): the group still reaches the actor ctx.
	groupOnly, _ := applyCallbackIdentity(context.Background(), map[string]string{contextargs.CallbackGroupID: "g-owner"})
	if got := actor.GroupID(groupOnly); got != "g-owner" {
		t.Errorf("actor group on a group-only callback = %q, want g-owner", got)
	}
}

// A callback without a group key leaves the outer ctx's group alone: the
// host never blanks or invents a scope.
func TestApplyCallbackIdentityKeepsOuterGroupWithoutGroupKey(t *testing.T) {
	outer := actor.WithGroupID(actor.WithActor(context.Background(), "caller"), "g-caller")

	ctx, args := applyCallbackIdentity(outer, map[string]string{
		contextargs.CallbackEntityID:  "owner",
		contextargs.CallbackSessionID: "s-wf",
		"text":                        "x",
	})

	if got := actor.GroupID(ctx); got != "g-caller" {
		t.Errorf("actor group = %q, want the outer g-caller untouched", got)
	}
	for _, k := range []string{contextargs.CallbackEntityID, contextargs.CallbackGroupID, contextargs.CallbackSessionID} {
		if _, leaked := args[k]; leaked {
			t.Errorf("reserved key %q leaked into args %v", k, args)
		}
	}
	if args["text"] != "x" {
		t.Errorf("args = %v, want text kept", args)
	}
}
