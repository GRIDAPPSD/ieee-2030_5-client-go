package inverter

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// Reporter sends periodic DERStatus PUTs and MirrorMeterReading POSTs for
// one DER, named by deviceLFDI.
//
// Hrefs are derived by main.go from the advertised link graph
// rather than built from ID segments. Empty hrefs cause the corresponding
// report to be skipped silently : used when the server's DeviceCapability
// or EndDevice didn't advertise the corresponding link, in which case the
// inverter must continue running locally without poking endpoints that
// don't exist on the server side.
type Reporter struct {
	client        *SEP2Client
	deviceLFDI    string
	derStatusHref string // empty -> skip status PUT
	mmrHref       string // empty -> skip metering POST
	// derAvailabilityHref is set by a manager (SetManagedDERHrefs); empty
	// -> skip the availability PUT.
	derAvailabilityHref string
	readingSeq          atomic.Uint64
	// manager makes the reporter act for deviceLFDI as a manager: its
	// requests carry that LFDI to the guard, and its readings carry a
	// flowDirection. Off for a der-role reporter, whose requests and
	// readings stay as they were.
	manager bool
}

// AsManager marks the reporter as one a manager runs for a managed device.
// Its POSTs are judged by the guard as actions for deviceLFDI, and each
// reading carries flowDirection so the server's fleet view can tell export
// from import. Call it before the first report.
func (r *Reporter) AsManager() *Reporter {
	r.manager = true
	return r
}

// SetManagedDERHrefs points a manager's reporter at its device's DERStatus
// and DERAvailability resources, taken from the device's own DER.
func (r *Reporter) SetManagedDERHrefs(derStatusHref, derAvailabilityHref string) {
	r.derStatusHref = derStatusHref
	r.derAvailabilityHref = derAvailabilityHref
}

// flowDirectionFor gives the flowDirection of an active power reading in
// the DER sign (positive delivering): 19 (received from customer, export)
// for output, 1 (delivered to customer, import) for a battery charging.
// Source: IEEE 2030.5-2018, FlowDirectionType (1 forward, delivered to the
// customer; 19 reverse, received from the customer) and Table E.2, which
// gives DER active power (W) flowDirection 19.
func flowDirectionFor(activeW float64) uint8 {
	if activeW < 0 {
		return sep2.FlowDirectionForward
	}
	return sep2.FlowDirectionReverse
}

// NewReporter creates a reporter for the DER named by deviceLFDI, PUTting
// status to derStatusHref and POSTing meter readings to mmrHref. Either or
// both hrefs may be empty to disable that channel (see Reporter doc).
// deviceLFDI is required : it names the reading in every mRID this
// reporter mints, so an empty value would mint readings for no device.
//
// See TestReporter_EmptyHrefsAreNoOps in client_test.go for coverage:
//  1. derStatusHref empty -> ReportStatus is a no-op, returns nil, zero HTTP.
//  2. mmrHref empty -> ReportMetering is a no-op, returns nil, zero HTTP.
//  3. both set -> exactly one PUT and one POST per ReportStatus/ReportMetering
//     call, to the exact hrefs passed in.
func NewReporter(client *SEP2Client, deviceLFDI, derStatusHref, mmrHref string) (*Reporter, error) {
	if deviceLFDI == "" {
		return nil, fmt.Errorf("device LFDI required")
	}
	return &Reporter{
		client:        client,
		deviceLFDI:    deviceLFDI,
		derStatusHref: derStatusHref,
		mmrHref:       mmrHref,
	}, nil
}

// ReportStatus sends a DERStatus PUT to the server. Returns nil immediately
// (without error) when the configured derStatusHref is empty : the server
// did not advertise a DERStatusLink, so there is nothing to report against.
func (r *Reporter) ReportStatus(ctx context.Context, state InverterState) error {
	return r.ReportStatusWithSOC(ctx, state, nil)
}

// ReportStatusWithSOC is ReportStatus carrying a battery's state of charge
// (0 to 1) as StateOfChargeStatus, in hundredths of a percent. A nil soc
// leaves the element out.
func (r *Reporter) ReportStatusWithSOC(ctx context.Context, state InverterState, soc *float64) error {
	if r.derStatusHref == "" {
		return nil
	}

	status := sep2.DERStatus{
		GenConnectStatus: &sep2.ConnectStatusType{
			DateTime: state.Time.Unix(),
			Value:    sep2.HexBinary8(genConnectStatus(state.Connected, state.Energized)),
		},
		OperationalModeStatus: &sep2.OperationalModeStatusType{
			DateTime: state.Time.Unix(),
			Value:    operationalModeStatus(state.Energized),
		},
		ReadingTime: state.Time.Unix(),
	}
	if soc != nil {
		status.StateOfChargeStatus = &sep2.StateOfChargeStatusType{
			DateTime: state.Time.Unix(),
			Value:    uint16(math.Round(math.Max(0, math.Min(1, *soc)) * 10000)),
		}
	}
	if r.manager {
		ctx = WithTarget(ctx, r.deviceLFDI)
	}

	err := r.client.PutDERStatus(ctx, r.derStatusHref, status)
	if err != nil {
		log.Printf("reporter: status PUT failed: %v", err)
	}
	return err
}

// ReportAvailability sends a DERAvailability PUT. Returns nil immediately
// when no availability href is set.
func (r *Reporter) ReportAvailability(ctx context.Context, avail sep2.DERAvailability) error {
	if r.derAvailabilityHref == "" {
		return nil
	}
	if r.manager {
		ctx = WithTarget(ctx, r.deviceLFDI)
	}
	err := r.client.PutDERAvailability(ctx, r.derAvailabilityHref, avail)
	if err != nil {
		log.Printf("reporter: availability PUT failed: %v", err)
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
	rt := &sep2.ReadingType{Uom: &uomW}
	if r.manager {
		// Utility perspective, as the 2018 standard uses: the reading is
		// the magnitude and flowDirection carries the sign.
		if activeW < 0 {
			activeW = -activeW
		}
		dir := flowDirectionFor(state.ActivePowerW)
		rt.FlowDirection = &dir
		ctx = WithTarget(ctx, r.deviceLFDI)
	}

	// Outbound identifier derives from the server-synced clock
	// (client.Now()), not local wall-clock. state.Time is simulation time
	// (2024 sunrise + accelerated delta), so it stays where it is on the
	// reading payload fields : replacing it would jump reported timestamps
	// out of the simulation's time domain. The MRID is a unique ID the
	// server may correlate against its own clock, so it goes through Now().
	// readingSeq disambiguates two readings for the same device within the
	// same formatted second, which the clock alone cannot.
	seq := r.readingSeq.Add(1)
	mmr := sep2.MirrorMeterReading{
		MRID:           readingMRID(r.deviceLFDI, r.client.Now(), seq),
		Description:    "Active Power",
		LastUpdateTime: state.Time.Unix(),
		ReadingType:    rt,
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

// readingMRID builds a MirrorMeterReading mRID that is unique per device
// and per reading, as mRIDType (HexBinary128) requires: 32 hex characters,
// derived (never literal) since this client has no PEN setting configured.
// deviceLFDI separates devices; seq is a per-Reporter monotonic counter
// that separates two readings for one device within the same formatted
// second, which the clock alone cannot.
func readingMRID(deviceLFDI string, at time.Time, seq uint64) string {
	return deriveMRID("reading", deviceLFDI, at.Format("20060102-150405"), strconv.FormatUint(seq, 10))
}

// operationalModeStatus maps DER state to the operationalModeStatus wire
// value (IEEE 2030.5-2018 OperationalModeStatusType): 1 off, 2 operational
// mode. Kept in its own function so #87's tri-state Energized can add the
// 0 (unknown) case without touching ReportStatus.
func operationalModeStatus(energized bool) uint8 {
	if energized {
		return 2
	}
	return 1
}

// genConnectStatus maps DER state to the genConnectStatus bitmap
// (IEEE 2030.5-2018 ConnectStatusType): bit 0 connected, bit 2 operating.
// Bit 1 (available) has no corresponding InverterState field and is left
// unset.
func genConnectStatus(connected, energized bool) uint8 {
	var v uint8
	if connected {
		v |= 1 << 0
	}
	if energized {
		v |= 1 << 2
	}
	return v
}
