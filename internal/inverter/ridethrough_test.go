package inverter

import (
	"testing"
	"time"
)

func TestCheckEnterServiceNormal(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	if !CheckEnterService(grid) {
		t.Error("normal grid should pass enter service check")
	}
}

func TestCheckEnterServiceLowVoltage(t *testing.T) {
	grid := GridState{VoltsPU: 0.90, FreqHz: 60.0}
	if CheckEnterService(grid) {
		t.Error("0.90 p.u. should fail enter service (min 0.917)")
	}
}

func TestCheckEnterServiceHighFreq(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.5}
	if CheckEnterService(grid) {
		t.Error("60.5 Hz should fail enter service (max 60.1)")
	}
}

func TestVoltageTrip(t *testing.T) {
	tests := []struct {
		name     string
		voltsPU  float64
		duration time.Duration
		trip     bool
	}{
		{"deep sag short", 0.45, 100 * time.Millisecond, false},
		{"deep sag trip", 0.45, 200 * time.Millisecond, true},
		{"moderate sag short", 0.65, 5 * time.Second, false},
		{"moderate sag trip", 0.65, 15 * time.Second, true},
		{"normal", 1.0, 60 * time.Second, false},
		{"high voltage short", 1.15, 5 * time.Second, false},
		{"high voltage trip", 1.15, 15 * time.Second, true},
		{"very high trip", 1.25, 200 * time.Millisecond, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckVoltageTrip(tt.voltsPU, tt.duration)
			if got != tt.trip {
				t.Errorf("CheckVoltageTrip(%.2f, %v) = %v, want %v",
					tt.voltsPU, tt.duration, got, tt.trip)
			}
		})
	}
}

func TestFrequencyTrip(t *testing.T) {
	tests := []struct {
		name     string
		freqHz   float64
		duration time.Duration
		trip     bool
	}{
		{"very low freq", 56.5, 200 * time.Millisecond, true},
		{"low freq short", 58.0, 100 * time.Second, false},
		{"low freq trip", 58.0, 350 * time.Second, true},
		{"normal", 60.0, 600 * time.Second, false},
		{"high freq short", 61.0, 100 * time.Second, false},
		{"high freq trip", 61.0, 350 * time.Second, true},
		{"very high freq", 62.5, 200 * time.Millisecond, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckFrequencyTrip(tt.freqHz, tt.duration)
			if got != tt.trip {
				t.Errorf("CheckFrequencyTrip(%.1f, %v) = %v, want %v",
					tt.freqHz, tt.duration, got, tt.trip)
			}
		})
	}
}

func TestRideThroughNormalToTrip(t *testing.T) {
	grid := GridState{VoltsPU: 0.45, FreqHz: 60.0}
	result := EvaluateRideThrough(grid, 200*time.Millisecond, RTNormal)
	if !result.ShouldTrip {
		t.Error("deep sag for 200ms should trip")
	}
}

func TestRideThroughNormalToRideThrough(t *testing.T) {
	grid := GridState{VoltsPU: 0.75, FreqHz: 60.0}
	result := EvaluateRideThrough(grid, 1*time.Second, RTNormal)
	if result.ShouldTrip {
		t.Error("0.75 p.u. for 1s should ride through, not trip")
	}
	if result.State != RTRidingThrough {
		t.Errorf("state = %d, want RTRidingThrough", result.State)
	}
}

func TestRideThroughReconnect(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	result := EvaluateRideThrough(grid, 350*time.Second, RTTripped)
	if !result.CanReconnect {
		t.Error("normal grid after 350s should allow reconnect")
	}
}

func TestRideThroughReconnectTooSoon(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	result := EvaluateRideThrough(grid, 100*time.Second, RTTripped)
	if result.CanReconnect {
		t.Error("100s is too soon for reconnect (need 300s)")
	}
}
