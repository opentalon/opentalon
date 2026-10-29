package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// TestNarrateConfirmation_FeedsRecentContextAndReturnsLLMText pins the
// confirmation narration: it hands BOTH the recent conversation (where a list
// call's count + example records live) AND the tool call to the LLM — so the LLM
// can resolve an opaque scope_token to a human description in the user's
// language — and returns the LLM's text verbatim.
func TestNarrateConfirmation_FeedsRecentContextAndReturnsLLMText(t *testing.T) {
	llm := &capturingLLM{responses: []string{"Sie möchten 2 Abräumwagen löschen — fortfahren?"}}
	orch := NewWithRules(llm, &fakeParser{}, NewToolRegistry(),
		state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{})

	recent := []provider.Message{
		{Role: provider.RoleUser, Content: "lösche die Abräumwagen"},
		{Role: provider.RoleUser, Content: "Items: 2 total • Abräumwagen [id: 2134281] • Abräumwagen [id: 2134285]"},
	}
	call := ToolCall{Action: "timly.delete-item", Args: map[string]string{"scope_token": "scope_xyz"}}

	got := orch.narrateConfirmation(context.Background(), recent, call, "lösche die Abräumwagen")

	if got != "Sie möchten 2 Abräumwagen löschen — fortfahren?" {
		t.Errorf("must return the LLM narration verbatim, got %q", got)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", len(llm.requests))
	}
	var sys, usr string
	for _, m := range llm.requests[0].Messages {
		switch m.Role {
		case provider.RoleSystem:
			sys = m.Content
		case provider.RoleUser:
			usr = m.Content
		}
	}
	if !strings.Contains(strings.ToLower(sys), "language") {
		t.Errorf("system prompt must steer the user's language, got: %q", sys)
	}
	// The preceding list result (count + an example id) must reach the LLM so it
	// can describe the opaque scope_token meaningfully.
	if !strings.Contains(usr, "2134281") {
		t.Errorf("recent tool result (example id) missing from LLM input, got: %q", usr)
	}
	if !strings.Contains(usr, "scope_xyz") {
		t.Errorf("tool args should be in the LLM input, got: %q", usr)
	}
}

// TestNarrateConfirmation_NilLLMReturnsEmpty pins the fallback hook: with no LLM
// the method returns "" so the caller uses the static template.
func TestNarrateConfirmation_NilLLMReturnsEmpty(t *testing.T) {
	orch := &Orchestrator{}
	if got := orch.narrateConfirmation(context.Background(), nil, ToolCall{Action: "x"}, ""); got != "" {
		t.Errorf("nil LLM must return empty string, got %q", got)
	}
}

// TestNarrateConfirmation_SingleRecordPromptDemandsFields pins what the prompt
// asks for on a call that names ONE record. The count-only version of this
// prompt produced confirmations of the shape "shall I change all the specified
// detail attributes?", which a user can only answer yes to blind — three fields
// on a machine were overwritten with another machine's values that way. The
// fields have to be named individually, with the value they change from, and a
// missing previous value has to be admitted rather than invented or dropped.
func TestNarrateConfirmation_SingleRecordPromptDemandsFields(t *testing.T) {
	llm := &capturingLLM{responses: []string{"ok?"}}
	orch := NewWithRules(llm, &fakeParser{}, NewToolRegistry(),
		state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{})

	call := ToolCall{Action: "timly.update-item", Args: map[string]string{"id": "42", "serial_number": "X-9"}}
	orch.narrateConfirmation(context.Background(), nil, call, "setz die Seriennummer auf X-9")

	if len(llm.requests) != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", len(llm.requests))
	}
	var sys string
	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleSystem {
			sys = m.Content
		}
	}

	for _, want := range []string{
		"FIELD BY FIELD",              // the fields are named, not summarised
		"previous value -> new value", // and named with what they change from
		"the specified attributes",    // the phrasing that is called out as unacceptable
		"not known here",              // a missing previous value is admitted
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt must cover %q, got: %q", want, sys)
		}
	}

	// The batch rule has to survive alongside it: a count is still the right
	// answer for many records, just not for one.
	if !strings.Contains(sys, "COUNT of affected records") {
		t.Errorf("system prompt lost the batch count rule, got: %q", sys)
	}
}

// narratorSystemPrompt runs narrateConfirmation once and returns the system
// message the narrator LLM received.
func narratorSystemPrompt(t *testing.T, ctx context.Context, userMessage string) string {
	t.Helper()
	llm := &capturingLLM{responses: []string{"ok?"}}
	orch := NewWithRules(llm, &fakeParser{}, NewToolRegistry(),
		state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{})

	recent := []provider.Message{
		{Role: provider.RoleUser, Content: "setz die Seriennummer von Maschine 7 auf X-9"},
		{Role: provider.RoleAssistant, Content: "Soll ich die Seriennummer ändern?"},
		{Role: provider.RoleUser, Content: userMessage},
		{Role: provider.RoleTool, Content: `{"id": 42, "name": "Machine 7", "serial_number": "A-1"}`},
	}
	call := ToolCall{Action: "items.update", Args: map[string]string{"id": "42", "serial_number": "X-9"}}
	orch.narrateConfirmation(ctx, recent, call, userMessage)

	if len(llm.requests) != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", len(llm.requests))
	}
	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleSystem {
			return m.Content
		}
	}
	t.Fatal("narrator request carried no system message")
	return ""
}

// TestNarrateConfirmation_ReplyLanguageDirectiveReachesNarrator pins the
// language of a confirmation raised in a turn started by the confirmation
// button: the user message is only the button label, so "the language of the
// user's latest request" says nothing, and the narrator used to answer in the
// label's language. The turn's reply-language directive — already resolved from
// the user's earlier words — must reach the narrator and outrank that guess.
func TestNarrateConfirmation_ReplyLanguageDirectiveReachesNarrator(t *testing.T) {
	directive := "## Reply language\nReply in German. Use German for your entire reply, regardless of the language of any " +
		"retrieved context or earlier messages. Technical terms (field, tool and status names) stay in English.\n\n"
	ctx := withReplyLanguageDirective(context.Background(), directive)

	sys := narratorSystemPrompt(t, ctx, "Approve")

	if !strings.Contains(sys, strings.TrimSpace(directive)) {
		t.Errorf("narrator system prompt must carry the turn's reply-language directive, got: %q", sys)
	}
	if !strings.Contains(sys, "takes precedence over rule 1") {
		t.Errorf("directive must be ranked above the latest-request language rule, got: %q", sys)
	}
	if !strings.HasPrefix(sys, confirmationNarratePrompt) {
		t.Errorf("the standing narration rules must stay intact ahead of the directive, got: %q", sys)
	}
}

// TestNarrateConfirmation_NoDirectiveKeepsPromptUnchanged pins the fallback:
// with no reply-language directive on the turn, the narrator's system prompt is
// exactly the standing rules, so rule 1 alone decides the language.
func TestNarrateConfirmation_NoDirectiveKeepsPromptUnchanged(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no directive":    context.Background(),
		"empty directive": withReplyLanguageDirective(context.Background(), ""),
	} {
		t.Run(name, func(t *testing.T) {
			if sys := narratorSystemPrompt(t, ctx, "Approve"); sys != confirmationNarratePrompt {
				t.Errorf("system prompt must be byte-for-byte the standing rules, got: %q", sys)
			}
		})
	}
}

// TestNarrateConfirmation_FollowsTheToolsHint pins the per-tool wording: an
// action that carries a ConfirmationHint is narrated under that hint (what the
// user decides on, no argument listing, no yes/no question) instead of the
// general field-by-field rules, which fit a record change only.
func TestNarrateConfirmation_FollowsTheToolsHint(t *testing.T) {
	llm := &capturingLLM{responses: []string{"Ich zeige Ihnen, wie Sie „Bleistift HB“ anlegen. Loslegen?"}}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "timly",
		Actions: []Action{{
			Name:             "walk-through-task",
			ConfirmationHint: "Ask in one sentence whether to show how the example item is created.",
		}},
	}, &echoExecutor{}); err != nil {
		t.Fatal(err)
	}
	orch := NewWithRules(llm, &fakeParser{}, registry, state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{})

	call := ToolCall{Plugin: "timly", Action: "walk-through-task", Args: map[string]string{"task": "create_item", "example_name": "Bleistift HB"}}
	got := orch.narrateConfirmation(context.Background(), nil, call, "Wie lege ich einen Bleistift an?")

	if got != "Ich zeige Ihnen, wie Sie „Bleistift HB“ anlegen. Loslegen?" {
		t.Errorf("must return the LLM narration verbatim, got %q", got)
	}
	var sys string
	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleSystem {
			sys = m.Content
		}
	}
	if !strings.Contains(sys, "show how the example item is created") {
		t.Errorf("system prompt must carry the tool's hint, got: %q", sys)
	}
	if strings.Contains(sys, "field: previous value") {
		t.Errorf("the field-by-field rules must give way to the hint, got: %q", sys)
	}
	if !strings.Contains(strings.ToLower(sys), "form of address") {
		t.Errorf("system prompt must keep the user's form of address, got: %q", sys)
	}

	// Without a hint the general rules apply as before.
	plain := ToolCall{Plugin: "timly", Action: "update-item", Args: map[string]string{"id": "7"}}
	orch.narrateConfirmation(context.Background(), nil, plain, "ändere die Seriennummer")
	for _, m := range llm.requests[1].Messages {
		if m.Role == provider.RoleSystem && !strings.Contains(m.Content, "field: previous value") {
			t.Errorf("an action without a hint keeps the general rules, got: %q", m.Content)
		}
	}

	// The static fallback (narration unavailable) keeps a hinted action's arguments out of sight as well.
	if got := orch.confirmationFallback(call); strings.Contains(got, "example_name") || strings.Contains(got, "walk-through-task") {
		t.Errorf("fallback for a hinted action shows neither its arguments nor its name, got: %q", got)
	}
	if got := orch.confirmationFallback(plain); !strings.Contains(got, "- id: 7") {
		t.Errorf("fallback for a plain action lists its arguments, got: %q", got)
	}
}

// TestNarrateConfirmation_HintFoundUnderMCPNames pins the lookup through the
// name normalisation the mcp-plugin needs: the manifest carries the prefixed
// form ("timly__walk-through-task"), the model emits the bare one.
func TestNarrateConfirmation_HintFoundUnderMCPNames(t *testing.T) {
	llm := &capturingLLM{responses: []string{"Loslegen?"}}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name:    "timly",
		Actions: []Action{{Name: "timly__walk-through-task", ConfirmationHint: "Ask whether to show it."}},
	}, &echoExecutor{}); err != nil {
		t.Fatal(err)
	}
	orch := NewWithRules(llm, &fakeParser{}, registry, state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{})

	orch.narrateConfirmation(context.Background(), nil, ToolCall{Plugin: "timly", Action: "walk-through-task"}, "zeig es mir")

	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleSystem && !strings.Contains(m.Content, "Ask whether to show it.") {
			t.Errorf("the hint must be found under the normalised action name, got: %q", m.Content)
		}
	}
}
