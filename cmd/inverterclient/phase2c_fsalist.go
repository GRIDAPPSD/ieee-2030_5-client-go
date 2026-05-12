package main

// Phase 2c: FunctionSetAssignmentsList discovery (IEEE-035 — plan-1 phase 4
// entry).
//
// Extracted from main() by IEEE-075 to give the 5 deferred IEEE-072 Phase 2c
// integration cases a function seam to test against. The previous inline form
// lived straight inside main() and called log.Fatalf on the fatal paths
// (missing FunctionSetAssignmentsListLink in CSIP strict, GET FSAList error),
// which tears down any test process that drives it. Same structural template
// as IEEE-074's Phase 2b extraction (see cmd/inverterclient/phase2b.go).
//
// runPhase2cFSAList owns ONLY the FSAList discovery control flow that lived
// in the inline switch — the missing-link branch (bypass vs fatal), the
// resolved-link branch (GET + empty-list idle vs accept), and the idle loop
// that re-polls dcap.PollRate when the server has not yet bound any FSAs.
// It does not touch the downstream IEEE-036 DERProgram-walk loop or the
// IEEE-037 Primacy selection: those still live in main() (the DERProgram-walk
// extraction is IEEE-076's scope; see backlog).
//
// Behavior contract (frozen at IEEE-035 / merged main@66de487; verified
// identical):
//
//   - edev.FunctionSetAssignmentsListLink == nil && (!cfg.CSIP ||
//     cfg.AllowUnregistered): log "skipping Phase 2c" once, return zero-value
//     FunctionSetAssignmentsList + nil error. Caller proceeds with an empty
//     fsaList — the IEEE-036 DERProgram-walk loop guard handles that case.
//   - edev.FunctionSetAssignmentsListLink == nil && CSIP-strict: return
//     *fsaListFatal citing "CSIP V1.2 CORE-012 step 1". Caller in main()
//     unwraps via errors.As and calls log.Fatalf so the exit-code-1 contract
//     is preserved.
//   - FunctionSetAssignmentsListLink resolved: log "=== Phase 2c: FSAList
//     Discovery ===", enter the GET loop:
//       - GET FSAList error -> *fsaListFatal wrapping the transport error
//         (Unwrap chains to ctx.Err() if cancel raced the GET).
//       - len(list.FunctionSetAssignments) > 0 -> log "FSAList: N entries
//         (paging cap 255; cursor walk deferred)" + return list, nil.
//       - len == 0 && CSIP-strict: idle on phase2cFSAListPollInterval
//         (dcap.PollRate). ctx-cancel returns ctx.Err().
//       - len == 0 && (!CSIP || AllowUnregistered): log "FSAList empty;
//         proceeding" + return list, nil.
//
// log.Fatalf stays in main() for two reasons:
//  1. Exit-code preservation: log.Fatalf calls os.Exit(1); pushing it down
//     here would shrink the testability gain we just made.
//  2. Pattern symmetry with runPhase2bRegistration (IEEE-074) and
//     walkDERProgramTree (IEEE-036), which also return wrapped errors and let
//     the outer main() owner log.Fatalf.
//
// Test seam: phase2cFSAListPollMin and phase2cFSAListPollDefault are vars
// (not consts) so phase2c_fsalist_test.go can shrink the floor below 60s
// without faking time. The production pinPollInterval() helper is unchanged
// — these vars shadow its policy only inside runPhase2cFSAList. Mirrors
// IEEE-074's phase2bPollMin/phase2bPollDefault pattern (and IEEE-070's
// minTimeSyncPollRate before it). The production binary never writes to
// these.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// fsaListFatal is returned when production behavior calls log.Fatalf inside
// the inline Phase 2c FSAList block. main() unwraps via errors.As and calls
// log.Fatalf at the top level so the exit-code-1 contract is preserved.
// Tests assert on the error value without tearing down the test process.
//
// inner is set when the fatal originated from a wrapped transport error
// (GetFSAList) so callers can errors.Is against context.Canceled /
// context.DeadlineExceeded down the chain. nil when the fatal is intrinsic
// (missing FunctionSetAssignmentsListLink in CSIP strict). Error() reproduces
// the production log.Fatalf format byte-for-byte; downstream consumers must
// not change those strings without updating any consumers that grep for them.
type fsaListFatal struct {
	reason string
	inner  error
}

func (e *fsaListFatal) Error() string { return e.reason }

func (e *fsaListFatal) Unwrap() error { return e.inner }

// phase2cFSAListPollMin and phase2cFSAListPollDefault mirror the floor /
// default applied by pinPollInterval. Declared as vars so the IEEE-075 tests
// can shrink the floor without faking time; see phase2c_fsalist_test.go.
// Production behavior is identical to the previous inline form (60s floor,
// 30min default-on-zero).
var (
	phase2cFSAListPollMin     = 60 * time.Second
	phase2cFSAListPollDefault = 30 * time.Minute
)

// phase2cFSAListPollInterval is the in-scope mirror of pinPollInterval that
// honors phase2cFSAListPollMin / phase2cFSAListPollDefault so tests can
// compress the cadence. Identical policy to pinPollInterval at the
// production-default values. Duplicating the helper instead of parameterizing
// pinPollInterval honors Pike rule 1 (stay in scope): the shared poll-interval
// cleanup is a separate refactor, not part of this extraction.
func phase2cFSAListPollInterval(rate uint32) time.Duration {
	d := time.Duration(rate) * time.Second
	if d <= 0 {
		d = phase2cFSAListPollDefault
	}
	if d < phase2cFSAListPollMin {
		d = phase2cFSAListPollMin
	}
	return d
}

// runPhase2cFSAList walks EndDevice.FunctionSetAssignmentsListLink and returns
// the resolved FunctionSetAssignmentsList. CSIP V1.2 CORE-012 step 1:
// missing link is fatal under --csip strict, bypassed otherwise; empty list
// idles on dcap.PollRate under --csip strict and proceeds otherwise.
//
// Returns either:
//   - (list, nil): Phase 2c discovery cleared, caller proceeds with `list`.
//     `list` may be empty when the bypass paths fired (missing link with
//     CSIP off / --allow-unregistered, or empty list with same).
//   - (zero, context.Canceled / context.DeadlineExceeded): ctx fired
//     mid-idle.
//   - (zero, *fsaListFatal): production behavior would have called
//     log.Fatalf; caller in main() does so. Tests assert on this value via
//     errors.As.
//
// See package-level comment for the full behavior contract.
func runPhase2cFSAList(
	ctx context.Context,
	client *inverter.SEP2Client,
	edev sep2.EndDevice,
	cfg inverter.SimConfig,
	dcap sep2.DeviceCapability,
) (sep2.FunctionSetAssignmentsList, error) {
	var fsaList sep2.FunctionSetAssignmentsList

	switch {
	case edev.FunctionSetAssignmentsListLink == nil && (!cfg.CSIP || cfg.AllowUnregistered):
		log.Println("EndDevice has no FunctionSetAssignmentsListLink; skipping Phase 2c (--csip off or --allow-unregistered)")
		return fsaList, nil

	case edev.FunctionSetAssignmentsListLink == nil:
		return fsaList, &fsaListFatal{reason: "EndDevice has no FunctionSetAssignmentsListLink (CSIP V1.2 CORE-012 step 1 requires it); pass --allow-unregistered to bypass"}

	default:
		log.Println("=== Phase 2c: FSAList Discovery ===")
		for {
			// IEEE-047: on 301 GetFSAList returns the new FSAList base href
			// (paging query stripped). Update the cached link on the
			// EndDevice so the next idle-poll iteration and any other
			// caller that re-reads edev.FunctionSetAssignmentsListLink
			// uses the new URL directly.
			list, newHref, err := client.GetFSAList(ctx, edev.FunctionSetAssignmentsListLink.Href)
			if err != nil {
				return sep2.FunctionSetAssignmentsList{}, &fsaListFatal{
					reason: fmt.Sprintf("GET FSAList: %v", err),
					inner:  err,
				}
			}
			if newHref != "" {
				log.Printf("FSAList: 301 follow — cached href %s → %s",
					edev.FunctionSetAssignmentsListLink.Href, newHref)
				edev.FunctionSetAssignmentsListLink.Href = newHref
			}
			if len(list.FunctionSetAssignments) > 0 {
				fsaList = list
				log.Printf("FSAList: %d entries (paging cap 255; cursor walk deferred)", len(fsaList.FunctionSetAssignments))
				return fsaList, nil
			}
			if cfg.CSIP && !cfg.AllowUnregistered {
				pollEvery := phase2cFSAListPollInterval(dcap.PollRate)
				log.Printf("FSAList empty; re-polling every %s (CSIP V1.2 CORE-012 expects >=1 FSA per provisioned device)", pollEvery)
				select {
				case <-ctx.Done():
					return sep2.FunctionSetAssignmentsList{}, ctx.Err()
				case <-time.After(pollEvery):
				}
				continue
			}
			log.Println("FSAList empty; proceeding (--csip off or --allow-unregistered)")
			fsaList = list
			return fsaList, nil
		}
	}
}
