package device

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The committed recordings under sim/derms must load through the same
// parser the backend uses, so a regenerated file in an unexpected shape
// fails here and not at a demo.
func TestCommittedDermsRecordingsLoadAndHoldExpectedShape(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "..", "sim", "derms", "recordings")
	for _, tc := range []struct {
		file, typ string
		ratedW    float64
		at        [2]int
		want      float64
	}{
		{"agg-pv-1.csv", "pv", 4000, [2]int{12, 0}, 4000},
		{"agg-pv-1.csv", "pv", 4000, [2]int{3, 0}, 0},
		{"solo-pv-1.csv", "pv", 3000, [2]int{12, 0}, 3000},
		// battery_load +1800 while charging at 2 kW (efficiency shows), reported as -1800.
		{"agg-bat-1.csv", "battery", 5000, [2]int{11, 0}, -1800},
		{"agg-bat-1.csv", "battery", 5000, [2]int{22, 0}, 0},
	} {
		c := &clock{at(tc.at[0], tc.at[1])}
		r := newReplayAt(t, ReplayConfig{
			File: filepath.Join(dir, tc.file), Type: tc.typ, Clock: "wall", Scale: 1,
			RatedW: tc.ratedW, CapacityWh: 10000, InitialSOC: 0.5,
		}, c)
		got, err := r.ReadState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxPowerW != tc.want {
			t.Errorf("%s at %02d:%02d = %v, want %v", tc.file, tc.at[0], tc.at[1], got.MaxPowerW, tc.want)
		}
	}
}

func TestCommittedDermsRecordingsNoteTheGridLABDVersion(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "sim", "derms", "recordings", "GRIDLABD_VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "GridLAB-D 5.3.0") {
		t.Fatalf("version note = %q, want one starting GridLAB-D 5.3.0", b)
	}
}
