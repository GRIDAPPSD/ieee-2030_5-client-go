// Package inverter_test covers the DERStatus wire values ReportStatus
// sends: operationalModeStatus (#86) and genConnectStatus.
package inverter_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// TestReportStatus_OperationalModeAndConnectStatus covers every
// ControlMode value and both Energized values, plus Connected variations,
// and asserts the posted operationalModeStatus and genConnectStatus wire
// values (IEEE 2030.5-2018 OperationalModeStatusType and ConnectStatusType).
func TestReportStatus_OperationalModeAndConnectStatus(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var mu sync.Mutex
	var got sep2.DERStatus
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/der/1/ders", func(w http.ResponseWriter, r *http.Request) {
		var status sep2.DERStatus
		if err := xml.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode DERStatus: %v", err)
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = status
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)
	reporter, err := inverter.NewReporter(client, testDeviceLFDI, "/edev/1/der/1/ders", "")
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}

	allModes := []inverter.ControlMode{
		inverter.ModeConstantPF, inverter.ModeVoltVar, inverter.ModeWattVar,
		inverter.ModeConstantQ, inverter.ModeVoltWatt, inverter.ModeFreqDroop,
		inverter.ModeDisconnected, inverter.ModeTripped, inverter.ModeEnterService,
	}

	type testCase struct {
		name            string
		mode            inverter.ControlMode
		connected       bool
		energized       bool
		wantConnectBits uint8
		wantOpMode      uint8
	}

	var cases []testCase
	for _, mode := range allModes {
		for _, energized := range []bool{true, false} {
			want := uint8(1) // off
			connectBits := uint8(1 << 0)
			if energized {
				want = 2 // operational mode
				connectBits |= 1 << 2
			}
			cases = append(cases, testCase{
				name:            mode.String() + "/energized=" + boolStr(energized),
				mode:            mode,
				connected:       true,
				energized:       energized,
				wantConnectBits: connectBits,
				wantOpMode:      want,
			})
		}
	}
	// genConnectStatus bit 0 (Connected) must track Connected independently
	// of Energized.
	cases = append(cases,
		testCase{name: "disconnected/not energized", mode: inverter.ModeConstantPF, connected: false, energized: false, wantConnectBits: 0, wantOpMode: 1},
		testCase{name: "disconnected/energized", mode: inverter.ModeConstantPF, connected: false, energized: true, wantConnectBits: 1 << 2, wantOpMode: 2},
	)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			state := inverter.InverterState{
				Connected: tc.connected,
				Energized: tc.energized,
				Mode:      tc.mode,
				Time:      time.Now(),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := reporter.ReportStatus(ctx, state); err != nil {
				t.Fatalf("ReportStatus: %v", err)
			}

			mu.Lock()
			status := got
			mu.Unlock()

			if status.OperationalModeStatus == nil {
				t.Fatalf("OperationalModeStatus = nil, want value %d", tc.wantOpMode)
			}
			if status.OperationalModeStatus.Value != tc.wantOpMode {
				t.Errorf("OperationalModeStatus.Value = %d, want %d", status.OperationalModeStatus.Value, tc.wantOpMode)
			}
			if status.GenConnectStatus == nil {
				t.Fatalf("GenConnectStatus = nil, want value %d", tc.wantConnectBits)
			}
			if uint8(status.GenConnectStatus.Value) != tc.wantConnectBits {
				t.Errorf("GenConnectStatus.Value = %#x, want %#x", uint8(status.GenConnectStatus.Value), tc.wantConnectBits)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
