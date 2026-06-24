package main

// Phase 2c (continued): DERProgram-walk outer for-loop (IEEE-036 : plan-1
// phase 4 continued).
//
// Extracted from main() by IEEE-076 to give the 2 deferred IEEE-073 Phase 2c
// DERProgram-walk integration cases a function seam to test against. The
// previous inline form lived straight inside main() and called log.Fatalf on
// the walk-error path, which tears down any test process that drives it.
// Same structural template as IEEE-074's Phase 2b extraction
// (cmd/inverterclient/phase2b.go) and IEEE-075's Phase 2c FSAList extraction
// (cmd/inverterclient/phase2c_fsalist.go).
//
// runPhase2cDERProgramWalk owns ONLY the outer for-loop body that lived in
// the inline block : repeated calls to walkDERProgramTree, the
// non-empty-aggregate cache-and-break branch, the CSIP-strict empty-aggregate
// idle-on-PollRate branch, and the non-CSIP empty-aggregate proceed branch.
// It does not touch the inner walkDERProgramTree (already package-level,
// extracted by IEEE-036 itself and fully covered by IEEE-073's tests) and it
// does not touch the IEEE-037 Primacy selection or the IEEE-041 DefaultDERControl
// re-GET that follow in main().
//
// Behavior contract (frozen at IEEE-036 / merged main@1210539; verified
// identical):
//
//   - empty FSAList (len(fsaList.FunctionSetAssignments) == 0): NOT this
//     function's concern. The caller (main()) gates entry on a non-empty
//     fsaList, and the IEEE-035 / IEEE-072 path already covers empty-FSAList
//     short-circuit. This function is only entered when there is at least
//     one FSA to walk.
//   - Banner: log "=== Phase 2c: DERProgram Tree Walk ===" once on entry.
//   - Each iteration of the outer loop re-allocates derProgramsByMRID, calls
//     walkDERProgramTree to populate it, and then:
//       - walkDERProgramTree error -> *derProgramWalkFatal wrapping the walk
//         error (Unwrap chains to ctx.Err() if cancel raced an inner GET).
//       - len(derProgramsByMRID) > 0 -> log "Phase 2c (DERProgram walk):
//         cached %d DERProgram(s) across %d FSA(s)" + return the cache, nil.
//       - len == 0 && CSIP-strict: idle on phase2cDERProgramPollInterval
//         (dcap.PollRate). ctx-cancel returns ctx.Err().
//       - len == 0 && (!CSIP || AllowUnregistered): log "No DERPrograms
//         enumerated; proceeding with empty cache (--csip off or
//         --allow-unregistered)" + return the (empty) cache, nil.
//
// log.Fatalf stays in main() for two reasons:
//  1. Exit-code preservation: log.Fatalf calls os.Exit(1); pushing it down
//     here would shrink the testability gain we just made.
//  2. Pattern symmetry with runPhase2bRegistration (IEEE-074),
//     runPhase2cFSAList (IEEE-075), and walkDERProgramTree (IEEE-036), which
//     also return wrapped errors and let the outer main() owner log.Fatalf.
//
// Test seam: phase2cDERProgramPollMin and phase2cDERProgramPollDefault are
// vars (not consts) so phase2c_derprogram_test.go can shrink the floor below
// 60s without faking time. The production pinPollInterval() helper is
// unchanged : these vars shadow its policy only inside
// runPhase2cDERProgramWalk. Mirrors IEEE-074's phase2bPollMin/phase2bPollDefault
// and IEEE-075's phase2cFSAListPollMin/phase2cFSAListPollDefault patterns
// (and IEEE-070's minTimeSyncPollRate before them). The production binary
// never writes to these.

import (
	"context"
	"fmt"
	"log"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// derProgramWalkFatal is returned when production behavior calls log.Fatalf
// inside the inline Phase 2c DERProgram-walk block. main() unwraps via
// errors.As and calls log.Fatalf at the top level so the exit-code-1 contract
// is preserved. Tests assert on the error value without tearing down the test
// process.
//
// inner is set when the fatal originated from a wrapped walk error
// (walkDERProgramTree) so callers can errors.Is against context.Canceled /
// context.DeadlineExceeded down the chain (walkDERProgramTree wraps its inner
// GETs with %w). Error() reproduces the production log.Fatalf format
// byte-for-byte; downstream consumers must not change those strings without
// updating any consumers that grep for them.
type derProgramWalkFatal struct {
	reason string
	inner  error
}

func (e *derProgramWalkFatal) Error() string { return e.reason }

func (e *derProgramWalkFatal) Unwrap() error { return e.inner }

// phase2cDERProgramPollMin and phase2cDERProgramPollDefault mirror the floor /
// default applied by pinPollInterval. Declared as vars so the IEEE-076 tests
// can shrink the floor without faking time; see phase2c_derprogram_test.go.
// Production behavior is identical to the previous inline form (60s floor,
// 30min default-on-zero).
var (
	phase2cDERProgramPollMin     = 60 * time.Second
	phase2cDERProgramPollDefault = 30 * time.Minute
)

// phase2cDERProgramPollInterval is the in-scope mirror of pinPollInterval
// that honors phase2cDERProgramPollMin / phase2cDERProgramPollDefault so
// tests can compress the cadence. Identical policy to pinPollInterval at the
// production-default values. Duplicating the helper instead of parameterizing
// pinPollInterval honors Pike rule 1 (stay in scope): the shared
// poll-interval cleanup is a separate refactor, not part of this extraction.
func phase2cDERProgramPollInterval(rate uint32) time.Duration {
	d := time.Duration(rate) * time.Second
	if d <= 0 {
		d = phase2cDERProgramPollDefault
	}
	if d < phase2cDERProgramPollMin {
		d = phase2cDERProgramPollMin
	}
	return d
}

// runPhase2cDERProgramWalk drives the DERProgram-walk loop: repeatedly calls
// walkDERProgramTree, idle-polls on dcap.PollRate when the aggregate
// enumerated DERProgram set across all FSAs is empty under --csip strict, and
// falls through with an empty cache otherwise. Returns the cache keyed by
// mRID and either:
//   - (cache, nil): Phase 2c DERProgram walk cleared, caller proceeds with the
//     cache. cache may be empty when the non-CSIP / --allow-unregistered
//     bypass fired.
//   - (empty, context.Canceled / context.DeadlineExceeded): ctx fired
//     mid-idle.
//   - (empty, *derProgramWalkFatal): production behavior would have called
//     log.Fatalf; caller in main() does so. Tests assert on this value via
//     errors.As.
//
// Precondition: len(fsaList.FunctionSetAssignments) > 0. The caller is
// responsible for short-circuiting the empty-FSAList case (the IEEE-035 /
// IEEE-072 path; no walk is performed when there are no FSAs to walk).
//
// See package-level comment for the full behavior contract.
func runPhase2cDERProgramWalk(
	ctx context.Context,
	client *inverter.SEP2Client,
	fsaList sep2.FunctionSetAssignmentsList,
	cfg inverter.SimConfig,
	dcap sep2.DeviceCapability,
) (map[string]sep2.DERProgram, error) {
	log.Println("=== Phase 2c: DERProgram Tree Walk ===")
	for {
		derProgramsByMRID := make(map[string]sep2.DERProgram)
		if err := walkDERProgramTree(ctx, client, fsaList, derProgramsByMRID); err != nil {
			return map[string]sep2.DERProgram{}, &derProgramWalkFatal{
				reason: fmt.Sprintf("walk DERProgram tree: %v", err),
				inner:  err,
			}
		}
		if len(derProgramsByMRID) > 0 {
			log.Printf("Phase 2c (DERProgram walk): cached %d DERProgram(s) across %d FSA(s)",
				len(derProgramsByMRID), len(fsaList.FunctionSetAssignments))
			return derProgramsByMRID, nil
		}
		if cfg.CSIP && !cfg.AllowUnregistered {
			pollEvery := phase2cDERProgramPollInterval(dcap.PollRate)
			log.Printf("No DERPrograms enumerated across any FSA; re-polling every %s (CSIP V1.2 CORE-012 step 2 expects >=1 DERProgram per provisioned device)", pollEvery)
			select {
			case <-ctx.Done():
				return map[string]sep2.DERProgram{}, ctx.Err()
			case <-time.After(pollEvery):
			}
			continue
		}
		log.Println("No DERPrograms enumerated; proceeding with empty cache (--csip off or --allow-unregistered)")
		return derProgramsByMRID, nil
	}
}
