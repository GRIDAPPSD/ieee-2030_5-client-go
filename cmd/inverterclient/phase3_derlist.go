package main

// Phase 3: DER Setup : GET the DERList advertised by EndDevice.DERListLink.
//
// Extracted from main() by IEEE-048 so the previously-fatal `log.Fatalf` call
// site on `client.Get(ctx, edev.DERListLink.Href, &derList)` failure can be
// replaced with graceful bypass and driven by a test against a stub server.
// Phase 7 exit criterion (1) in
// `plans/plan-1-csip-client-conformance/phase-7-http-semantics.md` requires
// the inverter to log a clear warning and continue when the server returns
// 404/501 on an optional function-set link. The DER setup PUTs that follow
// the list fetch (DERCapability, DERSettings, DERStatus discovery) are
// one-shot capability advertisement; the rest of the simulator does not
// hold a reference to derList past Phase 3, so a missing list is recoverable
// : Phase 5 status reporting is gated separately on `derStatusHref` which
// stays empty and produces its own "status reporting disabled" log line.
//
// Behavior contract (preserves IEEE-047 happy path byte-for-byte):
//
//   - GET succeeds: return (list, true, "", nil). Caller proceeds with the
//     existing inline Phase 3 setup loop.
//   - GET returns a typed HTTP error (`ErrBadRequest` / `ErrNotFound` /
//     `ErrMethodNotAllowed` / `ErrNotImplemented`) or `ErrResponseTransient`
//     (5xx): log a warning identifying the cause and the bypass policy,
//     return (zero, false, "", nil). Caller skips Phase 3.
//   - GET returns ctx.Err() (context.Canceled / DeadlineExceeded): return
//     that error unchanged. main() treats it the same as the previous inline
//     ctx-cancel exits.
//   - GET returns any other transport / parse error: log a warning and
//     return (zero, false, "", nil). The previous inline form called
//     log.Fatalf here; the Phase 7 graceful-bypass policy is that the DER
//     setup is non-essential and the inverter must keep running.
//   - IEEE-047 newHref is surfaced unchanged to the caller (one-shot in
//     Phase 3 : the inverter does not cache the DER list href, but main()
//     logs the redirect for diagnostics).
//
// log.Fatalf intentionally does NOT appear in this helper. The whole point
// of IEEE-048 is to replace the fatal-on-DER-list-failure call site. Tests
// in phase3_derlist_test.go drive each branch via a stub gotls listener so
// the regression guard fires if anyone re-adds a Fatalf here.

import (
	"context"
	"errors"
	"log"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// derListClient is the narrow surface fetchDERListForSetup needs from
// *inverter.SEP2Client. Tests substitute a fake; production passes the real
// client. Defined at the consumer per Pike-rule on small interfaces.
type derListClient interface {
	Get(ctx context.Context, path string, out interface{}) (string, error)
}

// fetchDERListForSetup GETs the DERList at derListHref. Failure modes are
// classified per the Phase 7 graceful-bypass policy documented in the
// package comment above.
//
// Returns (list, true, newHref, nil) on a successful fetch. Returns
// (zero, false, "", nil) when the fetch fails in a way the inverter must
// bypass. Returns (zero, false, "", err) only when ctx is cancelled : in
// that case main() exits the same way it would have under the prior inline
// form.
//
// derListHref is the caller-resolved value of edev.DERListLink.Href; the
// caller is responsible for the nil-link guard outside this helper.
func fetchDERListForSetup(
	ctx context.Context,
	client derListClient,
	derListHref string,
) (sep2.DERList, bool, string, error) {
	var derList sep2.DERList
	newHref, err := client.Get(ctx, derListHref, &derList)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return sep2.DERList{}, false, "", err
		}
		switch {
		case errors.Is(err, inverter.ErrBadRequest),
			errors.Is(err, inverter.ErrNotFound),
			errors.Is(err, inverter.ErrMethodNotAllowed),
			errors.Is(err, inverter.ErrNotImplemented):
			log.Printf("Phase 3 GET DER list %s: %v : DER list unavailable; skipping Phase 3 DER setup", derListHref, err)
		case errors.Is(err, inverter.ErrResponseTransient):
			log.Printf("Phase 3 GET DER list %s: %v : server transient failure; skipping Phase 3 DER setup", derListHref, err)
		default:
			log.Printf("Phase 3 GET DER list %s: %v : unexpected failure; skipping Phase 3 DER setup", derListHref, err)
		}
		return sep2.DERList{}, false, "", nil
	}
	return derList, true, newHref, nil
}
