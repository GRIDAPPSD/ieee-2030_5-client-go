package main

// Phase 2b: Registration resource read + PIN match check + commissioning gate.
//
// Extracted from main() to give the deferred integration
// cases a function seam to test against. The previous inline form lived
// straight inside main() and called log.Fatalf on the fatal paths (missing-
// link-lost mid-poll, GET error, non-zero PIN mismatch), which tears down
// any test process that drives it.
//
// runPhase2bRegistration owns ONLY the control flow that lived in the inline
// switch : the missing-RegistrationLink branch (bypass vs idle), the
// RegistrationLink-resolved branch (PIN read + match enforcement), and the
// idle loops that re-fetch the EndDevice (missing link) or the Registration
// resource (PIN-not-yet-provisioned). It does not touch the upstream
// EndDeviceList lookup / Register POST path: those still live in main().
//
// Behavior contract (frozen; verified identical
// against GRIDAPPSD/ieee-2030_5-server-go#46):
//
//   - edev.RegistrationLink == nil && (!cfg.CSIP || cfg.AllowUnregistered):
//     log "skipping Phase 2b" once, return edev unchanged.
//   - edev.RegistrationLink == nil && CSIP-strict: log "Awaiting
//     RegistrationLink", idle-loop on dcap.PollRate, re-fetch the
//     EndDevice via LookupOwnEndDevice until RegistrationLink appears.
//     ctx-cancel exits via context.Canceled; lost edevListHref between
//     polls is a *phase2bFatal; re-lookup transport error is *phase2bFatal.
//   - RegistrationLink resolved: log "=== Phase 2b: Registration ===",
//     enter the PIN-match loop:
//       - GET Registration error -> *phase2bFatal.
//       - cfg.ExpectedPIN == 0 -> log skip + return.
//       - rg.PIN matches cfg.ExpectedPIN -> log "matches expected;
//         proceeding" + return. THIS EXACT STRING is the CSIP commissioning
//         signal; downstream phases grep for it.
//       - rg.PIN == 0 (server not provisioned yet) -> log + idle on
//         pinPollInterval(rg.PollRate). ctx-cancel exits via
//         context.Canceled.
//       - rg.PIN != 0 && rg.PIN != cfg.ExpectedPIN -> *phase2bFatal with
//         the literal "PIN mismatch" + "CSIP V1.2 BASIC-001 step 5" + both
//         PINs redacted via redactPIN.
//
// All redactPIN calls preserved (IEEE 2030.5 §8.2.1).
//
// log.Fatalf stays in main() for two reasons:
//   1. Exit-code preservation: log.Fatalf calls os.Exit(1); pushing the call
//      down here would shrink testability gains we just made.
// 2. Pattern symmetry with walkDERProgramTree, which also
//      returns wrapped errors and lets the outer main() owner log.Fatalf.
//
// Test seam: phase2bPollMin and phase2bPollDefault are vars (not consts) so
// phase2b_test.go can shrink the floor below 60s without faking time. The
// production pinPollInterval() helper is unchanged : these vars shadow its
// policy only inside runPhase2bRegistration. Mirrors the
// minTimeSyncPollRate pattern. The production binary never writes to these.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// phase2bFatal is returned when production behavior calls log.Fatalf inside
// the inline Phase 2b block. main() unwraps via errors.As and calls
// log.Fatalf at the top level so the exit-code-1 contract is preserved.
// Tests assert on the error value without tearing down the test process.
//
// inner is set when the fatal originated from a wrapped transport error
// (GetRegistration / LookupOwnEndDevice) so callers can errors.Is against
// context.Canceled / context.DeadlineExceeded down the chain. nil when the
// fatal is intrinsic (PIN mismatch / lost edevListHref). Error() reproduces
// the production log.Fatalf format byte-for-byte; downstream consumers
// must not change those strings without updating the consumers that grep
// for them (see CSIP commissioning signal comment in runPhase2bRegistration).
type phase2bFatal struct {
	reason string
	inner  error
}

func (e *phase2bFatal) Error() string { return e.reason }

func (e *phase2bFatal) Unwrap() error { return e.inner }

// phase2bPollMin and phase2bPollDefault mirror the floor/default applied by
// pinPollInterval. Declared as vars so the tests can shrink the
// floor without faking time; see phase2b_test.go. Production behavior is
// identical to the previous inline form (60s floor, 30min default-on-zero).
var (
	phase2bPollMin     = 60 * time.Second
	phase2bPollDefault = 30 * time.Minute
)

// phase2bPollInterval is the in-scope mirror of pinPollInterval that honors
// phase2bPollMin / phase2bPollDefault so tests can compress the cadence.
// Identical policy to pinPollInterval at the production-default values.
func phase2bPollInterval(rate uint32) time.Duration {
	d := time.Duration(rate) * time.Second
	if d <= 0 {
		d = phase2bPollDefault
	}
	if d < phase2bPollMin {
		d = phase2bPollMin
	}
	return d
}

// runPhase2bRegistration walks EndDevice.RegistrationLink (idling in CSIP-
// strict mode until the server publishes it), GETs the Registration
// resource, and refuses to proceed unless the server-presented pIN matches
// cfg.ExpectedPIN. Returns the resolved EndDevice (possibly re-looked-up
// during a missing-link idle) and either:
//   - nil error: Phase 2b cleared, caller may proceed.
//   - context.Canceled / context.DeadlineExceeded: ctx fired mid-idle.
//   - *phase2bFatal: production behavior would have called log.Fatalf;
//     caller in main() does so. Tests assert on this value via errors.As.
//   - any other wrapped error: unexpected transport failure (not currently
//     emitted; reserved for future surface growth).
//
// See package-level comment for the full behavior contract.
func runPhase2bRegistration(
	ctx context.Context,
	client *inverter.SEP2Client,
	edev sep2.EndDevice,
	edevListHref string,
	cfg inverter.SimConfig,
	dcap sep2.DeviceCapability,
) (sep2.EndDevice, error) {
	switch {
	case edev.RegistrationLink == nil && (!cfg.CSIP || cfg.AllowUnregistered):
		log.Println("EndDevice has no RegistrationLink; skipping Phase 2b (--csip off or --allow-unregistered)")
		return edev, nil

	case edev.RegistrationLink == nil:
		// CSIP-strict: idle until the server publishes RegistrationLink.
		log.Println("=== Phase 2b: Awaiting RegistrationLink ===")
		for edev.RegistrationLink == nil {
			pollEvery := phase2bPollInterval(dcap.PollRate)
			log.Printf("EndDevice has no RegistrationLink (CSIP V1.2 BASIC-001 step 5 requires it); re-polling EndDevice every %s. Pass --allow-unregistered to bypass.", pollEvery)
			select {
			case <-ctx.Done():
				return edev, ctx.Err()
			case <-time.After(pollEvery):
			}
			if edevListHref == "" {
				return edev, &phase2bFatal{reason: "EndDeviceListLink lost between polls; cannot re-lookup own EndDevice"}
			}
			// On 301 LookupOwnEndDevice surfaces the new edev-list
			// base href; update our local copy so the next idle iteration
			// hits the new URL directly. The follow has already happened
			// inside LookupOwnEndDevice : newEdev is the live response.
			newEdev, newEdevListHref, err := client.LookupOwnEndDevice(ctx, edevListHref)
			if err != nil {
				return edev, &phase2bFatal{
					reason: fmt.Sprintf("re-lookup own EndDevice: %v", err),
					inner:  err,
				}
			}
			if newEdevListHref != "" {
				log.Printf("Phase 2b re-lookup: 301 follow : cached edev-list href %s → %s",
					edevListHref, newEdevListHref)
				edevListHref = newEdevListHref
			}
			edev = newEdev
		}
		log.Printf("RegistrationLink appeared: %s", edev.RegistrationLink.Href)
		fallthrough

	default:
		log.Println("=== Phase 2b: Registration ===")
		for {
			rg, err := client.GetRegistration(ctx, edev.RegistrationLink.Href)
			if err != nil {
				return edev, &phase2bFatal{
					reason: fmt.Sprintf("GET Registration: %v", err),
					inner:  err,
				}
			}
			if cfg.ExpectedPIN == 0 {
				log.Printf("Registration: href=%s pIN=%s (--pin not set; skipping match check)", edev.RegistrationLink.Href, redactPIN(uint(rg.PIN)))
				return edev, nil
			}
			if uint(rg.PIN) == cfg.ExpectedPIN {
				log.Printf("Registration: pIN=%s matches expected; proceeding", redactPIN(uint(rg.PIN)))
				return edev, nil
			}
			if rg.PIN == 0 {
				// Server hasn't provisioned us yet; transient. Idle on registration.PollRate.
				pollEvery := phase2bPollInterval(rg.PollRate)
				log.Printf("Registration: server PIN not yet provisioned (pIN=%s); re-polling Registration every %s", redactPIN(uint(rg.PIN)), pollEvery)
				select {
				case <-ctx.Done():
					return edev, ctx.Err()
				case <-time.After(pollEvery):
				}
				continue
			}
			// Non-zero mismatch : wrong device/server pair. Operator must intervene.
			return edev, &phase2bFatal{reason: fmt.Sprintf(
				"Registration: PIN mismatch : server=%s expected=%s (CSIP V1.2 BASIC-001 step 5; check --pin or device provisioning)",
				redactPIN(uint(rg.PIN)), redactPIN(cfg.ExpectedPIN))}
		}
	}
}
