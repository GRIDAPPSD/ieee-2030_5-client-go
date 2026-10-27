package main

// Phase 4: Metering Setup : POST a MirrorUsagePoint to the list href
// advertised by DeviceCapability, then build the Reporter that posts
// readings against it and DER status.
//
// Extracted from main() so a test can pin the identity wiring directly: a
// DER client only ever acts for its own EndDevice, so both the mirror and
// the reporter must carry client.LFDI(), never another device's identity
// and never empty (GRIDAPPSD/ieee-2030_5-client-go#68).
//
// Behavior contract (preserves the previous inline form byte-for-byte):
//
//   - dcap.MirrorUsagePointListLink == nil: log and skip mirror creation;
//     mmrHref stays "".
//   - CreateMirrorUsagePoint error, or an empty Location: log and leave
//     mmrHref "" (metering disabled, non-fatal).
//   - success: log the Location and derive mmrHref as mupLoc + "/mr" (CSIP
//     convention; see CreateMirrorUsagePoint's own doc). core removed
//     MirrorMeterReadingListLink : sep.xsd has no such child, so this is
//     derived rather than read off the wire.
//   - always returns a *inverter.Reporter wired to derStatusHref and
//     mmrHref (either may be "").

import (
	"context"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// runPhase4Metering POSTs the DER client's own MirrorUsagePoint (naming
// itself) and returns the Reporter that posts against it.
func runPhase4Metering(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, derStatusHref string) *inverter.Reporter {
	log.Println("=== Phase 4: Metering Setup ===")
	mmrHref := createOwnMirror(ctx, client, dcap)

	// client.LFDI() is derived from the leaf certificate at construction
	// (NewSEP2Client) and is never empty once the client exists, so
	// NewReporter's empty-LFDI guard is unreachable here; fail loudly
	// rather than silently disabling metering if that ever changes.
	reporter, err := inverter.NewReporter(client, client.LFDI(), derStatusHref, mmrHref)
	if err != nil {
		log.Fatalf("build reporter: %v", err)
	}
	return reporter
}

// createOwnMirror POSTs the DER client's own MirrorUsagePoint and returns the
// href its readings post to, or "" when none was created (no list link or a
// failed POST), each logged.
func createOwnMirror(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability) string {
	if dcap.MirrorUsagePointListLink == nil {
		log.Println("DeviceCapability has no MirrorUsagePointListLink; metering disabled")
		return ""
	}
	mupLoc, err := postMirrorUsagePoint(ctx, client, dcap.MirrorUsagePointListLink.Href, client.LFDI(), "PV Inverter Metering")
	if err != nil {
		log.Printf("create MirrorUsagePoint: %v (metering disabled)", err)
		return ""
	}
	log.Printf("MirrorUsagePoint: %s", mupLoc)
	return mupLoc + "/mr"
}

// ownReporter runs the single-device report step and replaces a mirror the
// server lost, under the same backoff a managed device uses.
type ownReporter struct {
	client   *inverter.SEP2Client
	dcap     sep2.DeviceCapability
	reporter *inverter.Reporter
	base     time.Duration
	now      func() time.Time
	retry    mirrorRetry
	lost     bool
}

// report posts one status and one metering reading. A 404 on the reading
// marks the mirror lost; readings are skipped, as for a managed device, until
// the backoff has elapsed and a new mirror is created.
func (o *ownReporter) report(ctx context.Context, state inverter.InverterState) {
	if err := o.reporter.ReportStatus(ctx, state); err != nil {
		log.Printf("ReportStatus failed: %v", err)
	}
	if o.lost {
		if !o.retry.ready(o.now()) {
			return
		}
		href := createOwnMirror(ctx, o.client, o.dcap)
		if href == "" {
			o.retry.failed(o.now(), o.base)
			return
		}
		o.reporter.SetMirrorHref(href)
		o.lost = false
	}
	if err := o.reporter.ReportMetering(ctx, state); err != nil {
		log.Printf("ReportMetering failed: %v", err)
		if inverter.MirrorLost(err) {
			o.lost = true
			o.retry.failed(o.now(), o.base)
		}
		return
	}
	o.retry.worked()
}
