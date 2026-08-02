package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestContextOverflow_RecognisesProviderWordings(t *testing.T) {
	// The vLLM/OVH wording is the one that killed a production session on
	// 2026-08-01, verbatim from the log including the double-encoded body the
	// OpenAI client wraps it in.
	cases := []struct {
		name     string
		err      error
		measured int
	}{
		{
			"vllm/ovh",
			errors.New(`openai api error (status 400): {"message":"{\"error\":{\"message\":\"Input length (132235) exceeds model's maximum context length (131072).\",\"type\":\"BadRequestError\"}}"}`),
			132235,
		},
		{
			"openai",
			errors.New("This model's maximum context length is 8192 tokens. However, your messages resulted in 8500 tokens. Please reduce the length of the messages."),
			8500,
		},
		{
			"anthropic",
			errors.New("prompt is too long: 200000 tokens > 199999 maximum"),
			200000,
		},
		{
			"no number given",
			errors.New("openai api error (status 400): context_length_exceeded"),
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			measured, ok := ContextOverflow(tc.err)
			if !ok {
				t.Fatalf("not recognised as an overflow: %v", tc.err)
			}
			if measured != tc.measured {
				t.Errorf("measured = %d, want %d", measured, tc.measured)
			}
		})
	}
}

func TestContextOverflow_LeavesOtherFailuresAlone(t *testing.T) {
	// A false positive here is worse than a false negative: it would treat a
	// genuinely broken endpoint as our own oversized prompt, skip the fallback,
	// and leave the endpoint marked healthy. The dedicated-node outage that
	// masked the real cause on 2026-08-01 is the first case below.
	for _, err := range []error{
		nil,
		errors.New(`openai api error (status 400): {"message":"app has no replica running"}`),
		errors.New("openai api error (status 429): rate limit exceeded"),
		errors.New("openai api error (status 500): internal server error"),
		errors.New("dial tcp: connection refused"),
		errors.New("invalid tool schema: None is not of type object"),
	} {
		if _, ok := ContextOverflow(err); ok {
			t.Errorf("wrongly classified as a context overflow: %v", err)
		}
	}
}

func TestContextOverflow_SeesThroughWrapping(t *testing.T) {
	// The orchestrator wraps provider errors before anyone inspects them.
	wrapped := fmt.Errorf("LLM completion: %w",
		errors.New("Input length (140000) exceeds model's maximum context length (131072)."))
	measured, ok := ContextOverflow(wrapped)
	if !ok || measured != 140000 {
		t.Errorf("wrapped error: measured=%d ok=%v, want 140000 true", measured, ok)
	}
}
