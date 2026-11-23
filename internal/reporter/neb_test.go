package reporter

import (
	"math"
	"testing"
)

func TestNebDifficulty(t *testing.T) {
	// mainnet block 7727: bits 0xb88e8208 -> target 0x8e8208 << 184
	if d := nebDifficulty(3096347144); math.Abs(d-5.0563917198852656e+14) > 1e3 {
		t.Fatalf("difficulty %v", d)
	}
	if d := nebDifficulty(0xb8000001); d != 0 { // not normalised
		t.Fatalf("difficulty of a bad compact %v", d)
	}
}
