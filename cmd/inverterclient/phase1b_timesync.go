package main

// Phase 1b: server-time sync (IEEE-031).
//
// Extracted from main() by IEEE-048 so the previously-fatal `log.Fatalf` call
// site on `SyncServerTime` failure can be replaced with graceful bypass and
// driven by a test against a stub server. Phase 7 exit criterion (1) in
// `plans/plan-1-csip-client-conformance/phase-7-http-semantics.md` requires
// the inverter to log a clear warning and continue when the server returns
// 404/501 on an optional function-set link — the Time resource is optional
// (the inline `else` branch already degrades to the local clock when
// DeviceCapability does not advertise TimeLink). This extraction unifies
// the "TimeLink absent" path with the "TimeLink fetch failed" path: both
// degrade to the local clock and let Phase 2 proceed.
//
// Behavior contract (preserves IEEE-031 happy path byte-for-byte):
//
//   - dcap.TimeLink == nil: log the same "no TimeLink; using local clock"
//     line the inline form emitted, return nil. No goroutine started.
//   - dcap.TimeLink != nil && SyncServerTime succeeds: log the same
//     "Server time: <ts> (offset from local: <d>)" line; spawn the periodic
//     RunTimeSync goroutine tied to ctx; return nil.
//   - dcap.TimeLink != nil && SyncServerTime returns a typed HTTP error
//     (`ErrBadRequest` / `ErrNotFound` / `ErrMethodNotAllowed` /
//     `ErrNotImplemented`) or `ErrResponseTransient` (5xx): log a warning
//     identifying the cause and the bypass policy, return nil. No goroutine
//     started — re-polling a permanently-broken endpoint would just spam
//     the log.
//   - dcap.TimeLink != nil && SyncServerTime returns ctx.Err()
//     (context.Canceled / DeadlineExceeded): return that error unchanged.
//     main() treats it the same as the previous inline ctx-cancel exits.
//   - dcap.TimeLink != nil && SyncServerTime returns any other transport /
//     parse error: log a warning and return nil. The previous inline form
//     called log.Fatalf here; the Phase 7 graceful-bypass policy is that
//     the Time resource is non-essential and the inverter must keep
//     running.
//
// log.Fatalf intentionally does NOT appear in this helper. The whole point
// of IEEE-048 is to replace the fatal-on-Time-failure call site. Tests in
// phase1b_timesync_test.go drive each branch via a stub gotls listener so
// the regression guard fires if anyone re-adds a Fatalf here.

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// timeSyncClient is the narrow surface runPhase1bTimeSync needs from
// *inverter.SEP2Client. Tests substitute a fake; production passes the
// real client. Defined at the consumer (this file) per Pike-rule on small
// interfaces.
type timeSyncClient interface {
	SyncServerTime(ctx context.Context, timeHref string) (sep2.Time, error)
	RunTimeSync(ctx context.Context, timeHref string, pollRate time.Duration)
	Now() time.Time
}

// runPhase1bTimeSync performs the initial Time-resource fetch and, on
// success, spawns the periodic refresh goroutine. Failure modes are
// classified per the Phase 7 graceful-bypass policy documented in the
// package comment above.
//
// Returns ctx.Err() when the sync is interrupted by context cancellation;
// returns nil for every other outcome (success, no TimeLink, HTTP error
// bypass, transport-error bypass).
func runPhase1bTimeSync(
	ctx context.Context,
	client timeSyncClient,
	dcap sep2.DeviceCapability,
) error {
	if dcap.TimeLink == nil {
		log.Println("DeviceCapability has no TimeLink; using local clock for outbound timestamps")
		return nil
	}
	timeHref := dcap.TimeLink.Href
	serverTime, err := client.SyncServerTime(ctx, timeHref)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		switch {
		case errors.Is(err, inverter.ErrBadRequest),
			errors.Is(err, inverter.ErrNotFound),
			errors.Is(err, inverter.ErrMethodNotAllowed),
			errors.Is(err, inverter.ErrNotImplemented):
			log.Printf("Phase 1b time sync %s: %v — Time resource unavailable; degrading to local clock and skipping periodic sync", timeHref, err)
		case errors.Is(err, inverter.ErrResponseTransient):
			log.Printf("Phase 1b time sync %s: %v — server transient failure; degrading to local clock and skipping periodic sync", timeHref, err)
		default:
			log.Printf("Phase 1b time sync %s: %v — unexpected failure; degrading to local clock and skipping periodic sync", timeHref, err)
		}
		return nil
	}
	offset := client.Now().Sub(time.Now())
	log.Printf("Server time: %s (offset from local: %s)",
		time.Unix(serverTime.CurrentTime, 0).UTC().Format(time.RFC3339),
		offset)
	go client.RunTimeSync(ctx, timeHref, inverter.DefaultTimeSyncPollRate)
	return nil
}
