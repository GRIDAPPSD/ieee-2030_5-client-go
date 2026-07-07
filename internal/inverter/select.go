package inverter

import "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

// SelectHighestPriority picks the highest-priority DERProgram from the cache
// IEEE-036 populates during the Phase 2c FSA-walk in cmd/inverterclient/main.go.
//
// Selection rule per CSIP V1.2 CORE-012 step 2 and IEEE 2030.5-2018 §10.1.3
// list ordering:
//
//  1. Lowest Primacy value wins.
//  2. Ties on Primacy are broken by MRID lexicographic ascending (lex-min wins).
//
// Edge cases:
//
//   - Empty cache → returns the zero value and ok=false. Phase 5 callers must
//     treat ok=false as "no DERProgram selected" and fall back to their own
//     default control base.
//   - Single entry → returned as-is, ok=true.
//   - Winner with a nil DefaultDERControlLink is still valid; Phase 5 falls
//     back to its own default when the link is absent.
//
// The function is pure: no I/O, no error path. Callers iterate the map in
// arbitrary order, so a stable selection requires both selection criteria :
// Primacy alone is insufficient when ties exist.
func SelectHighestPriority(progs map[string]sep2.DERProgram) (sep2.DERProgram, bool) {
	if len(progs) == 0 {
		return sep2.DERProgram{}, false
	}
	var (
		winner sep2.DERProgram
		seen   bool
	)
	for _, p := range progs {
		switch {
		case !seen:
			winner = p
			seen = true
		case p.Primacy < winner.Primacy:
			winner = p
		case p.Primacy == winner.Primacy && p.MRID < winner.MRID:
			winner = p
		}
	}
	return winner, true
}
