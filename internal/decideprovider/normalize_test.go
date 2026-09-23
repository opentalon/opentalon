package decideprovider

import (
	"math"
	"testing"
)

func sums(t *testing.T, d *Decision) {
	t.Helper()
	var s float64
	for _, p := range d.Probabilities {
		if p < 0 {
			t.Fatalf("negative probability: %#v", d.Probabilities)
		}
		s += p
	}
	if math.Abs(s-1) > 1e-9 {
		t.Fatalf("probabilities sum to %v, want 1: %#v", s, d.Probabilities)
	}
	if math.Abs(d.Confidence-d.Probabilities[d.Chosen]) > 1e-9 {
		t.Fatalf("confidence %v != probabilities[chosen] %v", d.Confidence, d.Probabilities[d.Chosen])
	}
}

func TestNormalizeProbabilities_RenormalizesAndZeroFills(t *testing.T) {
	choices := []string{"A", "B", "C"}
	// Raw has a stray label D (dropped), omits C (zero-filled), and doesn't
	// sum to 1 (renormalized).
	dec, err := normalizeProbabilities(map[string]float64{"A": 3, "B": 1, "D": 5}, choices)
	if err != nil {
		t.Fatal(err)
	}
	sums(t, dec)
	if dec.Chosen != "A" {
		t.Errorf("chosen = %q, want A", dec.Chosen)
	}
	if _, ok := dec.Probabilities["D"]; ok {
		t.Error("stray label D survived")
	}
	if dec.Probabilities["C"] != 0 {
		t.Errorf("unseen choice C = %v, want 0", dec.Probabilities["C"])
	}
	if math.Abs(dec.Probabilities["A"]-0.75) > 1e-9 {
		t.Errorf("A = %v, want 0.75", dec.Probabilities["A"])
	}
}

func TestNormalizeProbabilities_AllZeroIsUniform(t *testing.T) {
	choices := []string{"A", "B"}
	dec, err := normalizeProbabilities(map[string]float64{}, choices)
	if err != nil {
		t.Fatal(err)
	}
	sums(t, dec)
	if dec.Probabilities["A"] != 0.5 || dec.Probabilities["B"] != 0.5 {
		t.Errorf("expected uniform, got %#v", dec.Probabilities)
	}
	if dec.Chosen != "A" { // tie-break to declared order
		t.Errorf("chosen = %q, want A (first declared)", dec.Chosen)
	}
}

func TestNormalizeProbabilities_TieBreakDeclaredOrder(t *testing.T) {
	dec, err := normalizeProbabilities(map[string]float64{"B": 1, "A": 1}, []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Chosen != "A" {
		t.Errorf("tie chosen = %q, want A (earliest declared)", dec.Chosen)
	}
}

func TestNormalizeProbabilities_NoChoices(t *testing.T) {
	if _, err := normalizeProbabilities(map[string]float64{"A": 1}, nil); err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestSoftmaxLogits_MissingChoiceIsZero(t *testing.T) {
	choices := []string{"A", "B", "C"}
	// Only A and B have logits; C should get probability 0.
	dec, err := softmaxLogits(map[string]float64{"A": 2.0, "B": 1.0}, choices)
	if err != nil {
		t.Fatal(err)
	}
	sums(t, dec)
	if dec.Probabilities["C"] != 0 {
		t.Errorf("C = %v, want 0", dec.Probabilities["C"])
	}
	if dec.Chosen != "A" {
		t.Errorf("chosen = %q, want A (highest logit)", dec.Chosen)
	}
	// Softmax of {2,1}: e^2/(e^2+e^1) ≈ 0.7311.
	if math.Abs(dec.Probabilities["A"]-0.7310585786) > 1e-6 {
		t.Errorf("A = %v, want ~0.731", dec.Probabilities["A"])
	}
}

func TestSoftmaxLogits_NoTokensIsUniform(t *testing.T) {
	dec, err := softmaxLogits(map[string]float64{}, []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	sums(t, dec)
	if dec.Probabilities["A"] != 0.5 {
		t.Errorf("expected uniform fallback, got %#v", dec.Probabilities)
	}
}
