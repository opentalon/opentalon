package decideprovider

import (
	"errors"
	"math"
)

// errNoChoices is returned when a request or response yields no usable choices.
var errNoChoices = errors.New("decideprovider: no declared choices")

// normalizeProbabilities turns a backend's raw per-label scores into a
// calibrated Decision over exactly the declared choices:
//
//   - Labels not in choices are dropped (stray training/model labels).
//   - Declared choices absent from raw are zero-filled.
//   - Negative scores are clamped to 0, then the vector is renormalized to sum 1
//     (if every score is 0, mass is spread uniformly so the result is still a
//     valid distribution rather than NaN).
//   - Chosen is the argmax; ties break to the earliest choice in declared order.
//
// This is the calibration seam issue #361 calls for: whatever a backend hands
// back, the executor always sees an honest distribution whose Chosen mass is its
// Confidence.
func normalizeProbabilities(raw map[string]float64, choices []string) (*Decision, error) {
	if len(choices) == 0 {
		return nil, errNoChoices
	}
	probs := make(map[string]float64, len(choices))
	var sum float64
	for _, c := range choices {
		v := raw[c]
		if v < 0 || math.IsNaN(v) {
			v = 0
		}
		probs[c] = v
		sum += v
	}
	if sum == 0 {
		// No signal: uniform rather than a degenerate all-zero vector.
		u := 1.0 / float64(len(choices))
		for _, c := range choices {
			probs[c] = u
		}
		sum = 1
	}
	for c := range probs {
		probs[c] /= sum
	}
	return finalize(probs, choices), nil
}

// softmaxLogits converts per-label logits into a Decision by softmaxing over the
// declared choices only. Choices with no logit are treated as -inf (probability
// 0). This is the local-logits path: read the choice-token logits, softmax them
// here rather than trusting the model to be calibrated.
func softmaxLogits(logits map[string]float64, choices []string) (*Decision, error) {
	if len(choices) == 0 {
		return nil, errNoChoices
	}
	// Max-subtraction for numerical stability, over present logits only.
	maxL := math.Inf(-1)
	present := make(map[string]bool, len(choices))
	for _, c := range choices {
		if v, ok := logits[c]; ok {
			present[c] = true
			if v > maxL {
				maxL = v
			}
		}
	}
	if math.IsInf(maxL, -1) {
		// No choice token appeared: fall back to uniform.
		return normalizeProbabilities(nil, choices)
	}
	probs := make(map[string]float64, len(choices))
	var sum float64
	for _, c := range choices {
		if !present[c] {
			probs[c] = 0
			continue
		}
		e := math.Exp(logits[c] - maxL)
		probs[c] = e
		sum += e
	}
	for c := range probs {
		probs[c] /= sum
	}
	return finalize(probs, choices), nil
}

// finalize picks the argmax with a declared-order tie-break and assembles the
// Decision. probs is assumed already normalized to sum 1 over choices.
func finalize(probs map[string]float64, choices []string) *Decision {
	chosen := choices[0]
	best := math.Inf(-1)
	for _, c := range choices { // declared order → deterministic tie-break
		if probs[c] > best {
			best = probs[c]
			chosen = c
		}
	}
	return &Decision{
		Chosen:        chosen,
		Confidence:    probs[chosen],
		Probabilities: probs,
	}
}
