package decideprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MaxQuestions caps one multi-question request. A review asks one question per
// guide section; well past any real guide, and small enough that a runaway
// caller can't build an unbounded request.
const MaxQuestions = 100

// Question is one typed choice about a shared state: an instruction, the
// declared choices in order, and an optional description per choice. A
// decision model answers "which of these choices fits the state, given the
// instruction" — a narrow question with described choices answers much more
// sharply than bare labels (opentalon issue #361, talooner issue #74).
type Question struct {
	// Instructions is the question itself ("Does the file follow 'Refund cap'?").
	Instructions string
	// Choices is the declared option set, in order (the tie-break order).
	Choices []string
	// Descriptions maps a choice to what it means. A choice without one is
	// sent with its label as the description.
	Descriptions map[string]string
}

// MultiRequest asks several Questions about one State in a single call, so the
// state (a guide, a file, its spec) is sent and billed once.
type MultiRequest struct {
	State string
	// Questions by caller-chosen key; Order lists the keys in request order.
	Questions map[string]Question
	Order     []string
}

// MultiDecision holds one Decision per question key, plus the usage of the
// whole call (individual Decisions carry no usage of their own).
type MultiDecision struct {
	Answers map[string]*Decision
	Usage   Usage
	// Model is the model the backend reports answering with; several, sorted
	// and comma-separated, when chunks of one call were answered by different
	// models; empty when the backend reports none.
	Model string
}

// MultiProvider is a backend that answers every question of a MultiRequest in
// one call (the hosted System-1 APIs: Jev, SubQ Snap).
type MultiProvider interface {
	DecideMany(ctx context.Context, req *MultiRequest) (*MultiDecision, error)
}

// Validate checks a MultiRequest before it reaches a backend: a state, 1 to
// MaxQuestions questions, each with an instruction and distinct, non-empty
// choices, and Order naming exactly the question keys.
func (r *MultiRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("decide requires a non-empty state")
	}
	if len(r.Questions) == 0 {
		return errors.New("decide requires at least one question")
	}
	if len(r.Questions) > MaxQuestions {
		return fmt.Errorf("decide accepts at most %d questions, got %d", MaxQuestions, len(r.Questions))
	}
	if len(r.Order) != len(r.Questions) {
		return errors.New("decide question order must list every question once")
	}
	for _, key := range r.Order {
		q, ok := r.Questions[key]
		if !ok {
			return fmt.Errorf("decide question order names unknown question %q", key)
		}
		if strings.TrimSpace(key) == "" {
			return errors.New("decide question keys must not be empty")
		}
		if strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("decide question %q needs instructions", key)
		}
		if len(q.Choices) < 2 {
			return fmt.Errorf("decide question %q needs at least two choices", key)
		}
		seen := make(map[string]bool, len(q.Choices))
		for _, c := range q.Choices {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("decide question %q has an empty choice", key)
			}
			if seen[c] {
				return fmt.Errorf("decide question %q lists choice %q twice", key, c)
			}
			seen[c] = true
		}
		for c := range q.Descriptions {
			if !seen[c] {
				return fmt.Errorf("decide question %q describes %q, which is not one of its choices", key, c)
			}
		}
	}
	return nil
}

// DecideMany answers req with p: in one call when p is a MultiProvider, else
// one Decide per question with the instruction and choice descriptions
// prepended to the state, summing usage. Either way every answer is a
// normalized Decision over its question's choices. The first failing question
// fails the whole request, so callers never act on a partial set.
func DecideMany(ctx context.Context, p Provider, req *MultiRequest) (*MultiDecision, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if mp, ok := p.(MultiProvider); ok {
		return mp.DecideMany(ctx, req)
	}
	out := &MultiDecision{Answers: make(map[string]*Decision, len(req.Order))}
	for _, key := range req.Order {
		q := req.Questions[key]
		dec, err := p.Decide(ctx, &Request{State: singleState(q, req.State), Choices: q.Choices})
		if err != nil {
			return nil, fmt.Errorf("question %q: %w", key, err)
		}
		if dec == nil {
			return nil, fmt.Errorf("question %q: decider %q returned no decision", key, p.ID())
		}
		out.Usage.InputTokens += dec.Usage.InputTokens
		out.Usage.OutputTokens += dec.Usage.OutputTokens
		// A copy: the backend's Decision is left as it returned it.
		answer := *dec
		answer.Usage = Usage{}
		out.Answers[key] = &answer
	}
	return out, nil
}

// singleState renders a question into the state for a backend that only takes
// {state, choices}: the instruction and what each choice means, then the state.
func singleState(q Question, state string) string {
	var b strings.Builder
	b.WriteString(q.Instructions)
	b.WriteString("\n")
	for _, c := range q.Choices {
		if d := q.Descriptions[c]; d != "" {
			fmt.Fprintf(&b, "- %s: %s\n", c, d)
		}
	}
	b.WriteString("\n")
	b.WriteString(state)
	return b.String()
}
