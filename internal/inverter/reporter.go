package inverter

import (
	"context"
	"log"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// Reporter sends periodic DERStatus PUTs and MirrorMeterReading POSTs.
//
// Hrefs are derived by main.go from the advertised link graph
// rather than built from ID segments. Empty hrefs cause the corresponding
// report to be skipped silently : used when the server's DeviceCapability
// or EndDevice didn't advertise the corresponding link, in which case the
// inverter must continue running locally without poking endpoints that
// don't exist on the server side.
type Reporter struct {
	client        *SEP2Client
	derStatusHref string // empty → skip status PUT
	mmrHref       string // empty → skip metering POST
}

// NewReporter creates a reporter that PUTs status to derStatusHref and POSTs
// meter readings to mmrHref. Either or both may be empty to disable that
// channel (see Reporter doc).
//
// See TestReporter_EmptyHrefsAreNoOps in client_test.go for coverage:
//  1. derStatusHref empty -> ReportStatus is a no-op, returns nil, zero HTTP.
//  2. mmrHref empty -> ReportMetering is a no-op, returns nil, zero HTTP.
//  3. both set -> exactly one PUT and one POST per ReportStatus/ReportMetering
//     call, to the exact hrefs passed in.
func NewReporter(client *SEP2Client, derStatusHref, mmrHref string) *Reporter {
	return &Reporter{
		client:        client,
		derStatusHref: derStatusHref,
		mmrHref:       mmrHref,
	}
}

// ReportStatus sends a DERStatus PUT to the server. Returns nil immediately
// (without error) when the configured derStatusHref is empty : the server
// did not advertise a DERStatusLink, so there is nothing to report against.
func (r *Reporter) ReportStatus(ctx context.Context, state InverterState) error {
	if r.derStatusHref == "" {
		return nil
	}

	connectValue := uint8(0)
	if state.Connected {
		connectValue = 1
	}
	modeValue := uint8(state.Mode)

	status := sep2.DERStatus{
		GenConnectStatus: &sep2.ConnectStatusType{
			DateTime: state.Time.Unix(),
			Value:    connectValue,
		},
		OperationalModeStatus: &sep2.OperationalModeStatusType{
			DateTime: state.Time.Unix(),
			Value:    modeValue,
		},
		ReadingTime: state.Time.Unix(),
	}

	err := r.client.PutDERStatus(ctx, r.derStatusHref, status)
	if err != nil {
		log.Printf("reporter: status PUT failed: %v", err)
	}
	return err
}

// ReportMetering sends a MirrorMeterReading POST with active power. Returns
// nil immediately when mmrHref is empty (see Reporter doc).
func (r *Reporter) ReportMetering(ctx context.Context, state InverterState) error {
	if r.mmrHref == "" {
		return nil
	}

	uomW := sep2.UomWatts
	activeW := int64(state.ActivePowerW)

	// Outbound identifier derives from the server-synced clock
	// (client.Now()), not local wall-clock. state.Time is simulation time
	// (2024 sunrise + accelerated delta), so it stays where it is on the
	// reading payload fields : replacing it would jump reported timestamps
	// out of the simulation's time domain. The MRID is a unique ID the
	// server may correlate against its own clock, so it goes through Now().
	mmr := sep2.MirrorMeterReading{
		MRID:           "reading-" + r.client.Now().Format("20060102-150405"),
		Description:    "Active Power",
		LastUpdateTime: state.Time.Unix(),
		ReadingType: &sep2.ReadingType{
			Uom: &uomW,
		},
		Reading: &sep2.Reading{
			Value: &activeW,
			TimePeriod: &sep2.DateTimeInterval{
				Start:    state.Time.Unix(),
				Duration: 1,
			},
		},
	}

	err := r.client.PostMeterReading(ctx, r.mmrHref, mmr)
	if err != nil {
		log.Printf("reporter: metering POST failed: %v", err)
	}
	return err
}
