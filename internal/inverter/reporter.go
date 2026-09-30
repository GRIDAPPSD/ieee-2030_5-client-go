package inverter

import (
	"context"
	"fmt"
	"log"
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
	readingSeq    atomic.Uint64
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
	// readingSeq disambiguates two readings for the same device within the
	// same formatted second, which the clock alone cannot.
	seq := r.readingSeq.Add(1)
	mmr := sep2.MirrorMeterReading{
		MRID:           readingMRID(r.deviceLFDI, r.client.Now(), seq),
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
