// Package vcr provides record/replay infrastructure for LLM integration tests.
//
// Cassettes are JSON files committed to git. Each cassette stores a prompt_hash
// (from internal/prompts) and a scenarios_hash (from internal/scenarios/testdata)
// at record time. On replay, NewPlayer verifies both hashes; a mismatch means
// prompts or scenarios changed and the cassette must be re-recorded with:
// make vcr-record-all
package vcr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/scenarios"
)

// LLMClient mirrors orchestrator.LLMClient; both Player and Recorder satisfy it.
type LLMClient interface {
	Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error)
}

// Cassette is the on-disk format for a recorded test scenario.
type Cassette struct {
	PromptHash    string        `json:"prompt_hash"`
	ScenariosHash string        `json:"scenarios_hash"`
	RecordedAt    time.Time     `json:"recorded_at"`
	Model         string        `json:"model"`
	Interactions  []Interaction `json:"interactions"`
}

// Interaction is one Complete call within a cassette.
type Interaction struct {
	Request  *provider.CompletionRequest  `json:"request,omitempty"`
	Response *provider.CompletionResponse `json:"response"`
}

// Player replays a cassette sequentially. It satisfies LLMClient and
// orchestrator.LLMClient via structural typing.
type Player struct {
	cassette *Cassette
	path     string
	pos      int
}

// NewPlayer loads cassettePath and verifies its prompt_hash against the current
// prompts.Hash(). Returns an error if the file is missing, malformed, or stale.
func NewPlayer(cassettePath string) (*Player, error) {
	data, err := os.ReadFile(cassettePath)
	if err != nil {
		return nil, fmt.Errorf("vcr: open cassette %s: %w", cassettePath, err)
	}
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("vcr: parse cassette %s: %w", cassettePath, err)
	}
	if cur := prompts.Hash(); c.PromptHash != cur {
		return nil, fmt.Errorf(
			"vcr: cassette %s is stale (prompts changed)\n  stored:  %s\n  current: %s\nRe-record with: make vcr-record-all",
			cassettePath, c.PromptHash, cur,
		)
	}
	if cur := scenarios.Hash(); c.ScenariosHash != cur {
		return nil, fmt.Errorf(
			"vcr: cassette %s is stale (scenarios changed)\n  stored:  %s\n  current: %s\nRe-record with: make vcr-record-all",
			cassettePath, c.ScenariosHash, cur,
		)
	}
	return &Player{cassette: &c, path: cassettePath}, nil
}

func (p *Player) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// Complete returns the next recorded response. Fails if the cassette is exhausted.
func (p *Player) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if p.pos >= len(p.cassette.Interactions) {
		return nil, fmt.Errorf("vcr: cassette %s exhausted after %d interactions", p.path, p.pos)
	}
	in := p.cassette.Interactions[p.pos]
	p.pos++
	if diff := requestDiff(in.Request, req); diff != "" {
		return nil, fmt.Errorf("vcr: cassette %s interaction %d: request differs from the recorded one: %s", p.path, p.pos, diff)
	}
	return in.Response, nil
}

func requestDiff(recorded, sent *provider.CompletionRequest) string {
	var diffs []string
	if r, s := toolNames(recorded), toolNames(sent); !slices.Equal(r, s) {
		diffs = append(diffs, fmt.Sprintf("tools %q, recorded %q", s, r))
	}
	if r, s := hasNativePreamble(recorded), hasNativePreamble(sent); r != s {
		diffs = append(diffs, fmt.Sprintf("native preamble %t, recorded %t", s, r))
	}
	return strings.Join(diffs, "; ")
}

func toolNames(req *provider.CompletionRequest) []string {
	if req == nil {
		return nil
	}
	names := make([]string, len(req.Tools))
	for i, td := range req.Tools {
		names[i] = td.Name
	}
	return names
}

func hasNativePreamble(req *provider.CompletionRequest) bool {
	return req != nil && slices.ContainsFunc(req.Messages, func(m provider.Message) bool {
		return m.Role == provider.RoleSystem && strings.Contains(m.Content, prompts.OrchestratorPreambleNative)
	})
}

// Recorder wraps a real LLMClient and appends each interaction to an in-memory
// cassette. Call Save() when the scenario completes.
type Recorder struct {
	inner    LLMClient
	cassette Cassette
	path     string
}

// NewRecorder wraps inner. model is stored in the cassette for reference.
// The cassette is written to path on Save().
func NewRecorder(inner LLMClient, path, model string) *Recorder {
	return &Recorder{
		inner: inner,
		path:  path,
		cassette: Cassette{
			PromptHash:    prompts.Hash(),
			ScenariosHash: scenarios.Hash(),
			RecordedAt:    time.Now().UTC(),
			Model:         model,
		},
	}
}

func (r *Recorder) SupportsFeature(f provider.Feature) bool {
	fp, ok := r.inner.(interface{ SupportsFeature(provider.Feature) bool })
	return ok && fp.SupportsFeature(f)
}

// Complete forwards to the real LLM and records the interaction.
func (r *Recorder) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	resp, err := r.inner.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	r.cassette.Interactions = append(r.cassette.Interactions, Interaction{
		Request:  req,
		Response: resp,
	})
	return resp, nil
}

// Save writes the cassette as indented JSON. Creates parent directories as needed.
func (r *Recorder) Save() error {
	data, err := json.MarshalIndent(r.cassette, "", "  ")
	if err != nil {
		return fmt.Errorf("vcr: marshal cassette: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("vcr: mkdir %s: %w", filepath.Dir(r.path), err)
	}
	if err := os.WriteFile(r.path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("vcr: write cassette %s: %w", r.path, err)
	}
	return nil
}
