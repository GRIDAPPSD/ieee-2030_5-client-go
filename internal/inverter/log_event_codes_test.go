// Table 34 logEventCode constant tests.
//
// Pins the numeric values per IEEE 2030.5-2018 §9.5 Table 34 (PEN == 0
// general category). Downstream alarm detectors emit these codes; a
// regression to the constants would silently change what the server
// receives, so the test is a tripwire.

package inverter

import "testing"

func TestLogEventCodes_Table34_PenZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"LogEventCodeVoltageLow / LE_VOLT_LO / LVRT trip", LogEventCodeVoltageLow, 1},
		{"LogEventCodeVoltageHigh / LE_VOLT_HI / HVRT trip", LogEventCodeVoltageHigh, 2},
		{"LogEventCodeFrequencyLow / LE_FREQ_LO", LogEventCodeFrequencyLow, 3},
		{"LogEventCodeFrequencyHigh / LE_FREQ_HI", LogEventCodeFrequencyHigh, 4},
		{"LogEventCodeReactiveLimit / LE_REACTIVE_LIMIT / VV curtailment", LogEventCodeReactiveLimit, 5},
		{"LogEventCodeActiveLimit / LE_ACTIVE_LIMIT / FW curtailment", LogEventCodeActiveLimit, 6},
		{"LogEventCodeGenDisable / LE_GEN_DISABLE / offline", LogEventCodeGenDisable, 7},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
}

// TestLogEventCodes_AllDistinct asserts the 7 codes do not collide. A
// duplicated constant would make two alarm classes report under the
// same code; the rate-limiter would conflate them and the server
// operator would lose the trip-class signal.
func TestLogEventCodes_AllDistinct(t *testing.T) {
	t.Parallel()
	all := []uint8{
		LogEventCodeVoltageLow,
		LogEventCodeVoltageHigh,
		LogEventCodeFrequencyLow,
		LogEventCodeFrequencyHigh,
		LogEventCodeReactiveLimit,
		LogEventCodeActiveLimit,
		LogEventCodeGenDisable,
	}
	seen := make(map[uint8]int, len(all))
	for i, c := range all {
		if prev, hit := seen[c]; hit {
			t.Errorf("code %d duplicated at indexes %d and %d", c, prev, i)
		}
		seen[c] = i
	}
}
