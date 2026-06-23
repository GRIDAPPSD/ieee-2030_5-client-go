package inverter

import (
	"math"
	"testing"
)

func TestFreqDroopAtNominal(t *testing.T) {
	p := FreqDroop(60.0, 60.0, 5.0, 8000, 10000)
	if p != 8000 {
		t.Errorf("at nominal freq P = %.0f, want 8000 (no change)", p)
	}
}

func TestFreqDroopUnderFreq(t *testing.T) {
	// At 59.0 Hz (1 Hz below nominal), droop=5%, rated=10kW
	// deltaF = -1.0, deltaP = -(-1.0 / (0.05*60)) * 10000 = 3333
	// adjusted = 8000 + 3333 = 11333, clamped to 10000
	p := FreqDroop(59.0, 60.0, 5.0, 8000, 10000)
	if p > 10000 {
		t.Errorf("under-freq P = %.0f, should be clamped to 10000", p)
	}
	if p < 8000 {
		t.Errorf("under-freq P = %.0f, should increase (not decrease)", p)
	}
}

func TestFreqDroopOverFreq(t *testing.T) {
	// At 62.0 Hz (2 Hz above nominal), well outside deadband
	p := FreqDroop(62.0, 60.0, 5.0, 8000, 10000)
	if p >= 8000 {
		t.Errorf("over-freq P = %.0f, should decrease below 8000", p)
	}
	if p < 0 {
		t.Errorf("over-freq P = %.0f, should not be negative", p)
	}
}

func TestFreqDroopWithinDeadband(t *testing.T) {
	// Small deviation within deadband — no adjustment
	p := FreqDroop(60.5, 60.0, 5.0, 8000, 10000)
	// 0.5 Hz deviation, deadband is ~1.02 Hz (0.017*60), so 0.5 < 1.02
	if math.Abs(p-8000) > 1 {
		t.Errorf("within deadband P = %.0f, want 8000", p)
	}
}

func TestFreqDroopZeroDroop(t *testing.T) {
	p := FreqDroop(59.0, 60.0, 0, 8000, 10000)
	if p != 8000 {
		t.Errorf("zero droop P = %.0f, want 8000", p)
	}
}
