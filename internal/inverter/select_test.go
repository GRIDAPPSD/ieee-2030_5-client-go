package inverter

import (
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// prog builds a minimal DERProgram for selection tests. defaultHref optionally
// attaches a DefaultDERControlLink so the nil-link case can be exercised by
// passing "".
func prog(mRID string, primacy uint8, defaultHref string) sep2.DERProgram {
	p := sep2.DERProgram{
		MRID:    mRID,
		Primacy: primacy,
	}
	if defaultHref != "" {
		p.DefaultDERControlLink = &sep2.Link{Href: defaultHref}
	}
	return p
}

// progMap turns a slice of programs into the map keyed by mRID that
// SelectHighestPriority consumes. Mirrors how cmd/inverterclient/main.go's
// Phase 2c walk caches DERPrograms.
func progMap(ps ...sep2.DERProgram) map[string]sep2.DERProgram {
	m := make(map[string]sep2.DERProgram, len(ps))
	for _, p := range ps {
		m[p.MRID] = p
	}
	return m
}

func TestSelectHighestPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		progs    map[string]sep2.DERProgram
		wantOK   bool
		wantMRID string
		wantPrim uint8
	}{
		{
			// Case 1: empty cache → no selection.
			name:   "empty cache returns ok=false",
			progs:  map[string]sep2.DERProgram{},
			wantOK: false,
		},
		{
			// Case 2: single entry → returned as-is.
			name:     "single entry returned unchanged",
			progs:    progMap(prog("abc", 5, "/derp/0/dderc")),
			wantOK:   true,
			wantMRID: "abc",
			wantPrim: 5,
		},
		{
			// Case 3: lowest primacy wins among three distinct primacies.
			name: "lowest primacy wins over higher primacies",
			progs: progMap(
				prog("p3", 3, "/derp/3/dderc"),
				prog("p1", 1, "/derp/1/dderc"),
				prog("p2", 2, "/derp/2/dderc"),
			),
			wantOK:   true,
			wantMRID: "p1",
			wantPrim: 1,
		},
		{
			// Case 4: tie on primacy broken by mRID lex-min.
			name: "mRID tie-break picks lex-min",
			progs: progMap(
				prog("B0FF", 1, "/derp/B/dderc"),
				prog("A0FF", 1, "/derp/A/dderc"),
			),
			wantOK:   true,
			wantMRID: "A0FF",
			wantPrim: 1,
		},
		{
			// Case 5: three-way tie all on primacy=2, lex-min wins.
			name: "three-way primacy tie picks lex-min mRID",
			progs: progMap(
				prog("C", 2, "/derp/C/dderc"),
				prog("A", 2, "/derp/A/dderc"),
				prog("B", 2, "/derp/B/dderc"),
			),
			wantOK:   true,
			wantMRID: "A",
			wantPrim: 2,
		},
		{
			// Case 6: winner with nil DefaultDERControlLink does not panic
			// and is still selected. Phase 5 handles the nil-link fallback.
			name:     "nil DefaultDERControlLink winner is safe",
			progs:    progMap(prog("only", 7, "")),
			wantOK:   true,
			wantMRID: "only",
			wantPrim: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := SelectHighestPriority(tt.progs)
			if ok != tt.wantOK {
				t.Fatalf("SelectHighestPriority ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.MRID != tt.wantMRID {
				t.Errorf("winner MRID = %q, want %q", got.MRID, tt.wantMRID)
			}
			if got.Primacy != tt.wantPrim {
				t.Errorf("winner Primacy = %d, want %d", got.Primacy, tt.wantPrim)
			}
		})
	}
}

// TestSelectHighestPriorityNilDefaultLinkAccessSafe verifies that the nil
// DefaultDERControlLink case (case 6) is not just selectable but also safe
// to inspect after selection : guarding against a panic if a Phase 5 caller
// reads the field directly.
func TestSelectHighestPriorityNilDefaultLinkAccessSafe(t *testing.T) {
	t.Parallel()

	got, ok := SelectHighestPriority(progMap(prog("only", 7, "")))
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if got.DefaultDERControlLink != nil {
		t.Fatalf("DefaultDERControlLink = %+v, want nil", got.DefaultDERControlLink)
	}
}
