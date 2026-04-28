// Package vcr provides record/replay infrastructure for LLM integration tests.
//
// Cassettes are JSON files committed to git. Each cassette stores a prompt_hash
// computed from internal/prompts at record time. On replay, NewPlayer verifies
// the current hash matches; a mismatch means prompts changed and the cassette
// must be re-recorded with: make vcr-record-all
package vcr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
)

// LLMClient mirrors orchestrator.LLMClient; both Player and Recorder satisfy it.
type LLMClient interface {
	Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error)
}

// Cassette is the on-disk format for a recorded test scenario.
type Cassette struct {
	PromptHash   string        `json:"prompt_hash"`
	RecordedAt   time.Time     `json:"recorded_at"`
	Model        string        `json:"model"`
	Interactions []Interaction `json:"interactions"`
}

// Interaction is one Complete call within a cassette.
// Request is stored by Recorder for documentation; Player ignores it during replay.
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
	current := prompts.Hash()
	if c.PromptHash != current {
		return nil, fmt.Errorf(
			"vcr: cassette %s is stale\n  stored:  %s\n  current: %s\nRe-record with: make vcr-record-all",
			cassettePath, c.PromptHash, current,
		)
	}
	return &Player{cassette: &c, path: cassettePath}, nil
}

// Complete returns the next recorded response. Fails if the cassette is exhausted.
func (p *Player) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if p.pos >= len(p.cassette.Interactions) {
		return nil, fmt.Errorf("vcr: cassette %s exhausted after %d interactions", p.path, p.pos)
	}
	resp := p.cassette.Interactions[p.pos].Response
	p.pos++
	return resp, nil
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
			PromptHash: prompts.Hash(),
			RecordedAt: time.Now().UTC(),
			Model:      model,
		},
	}
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
