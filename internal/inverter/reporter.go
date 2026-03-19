package inverter

import (
	"context"
	"log"
	"time"

	"github.com/craig8/ieee-2030_5-go/pkg/sep2"
)

// Reporter sends periodic DERStatus and metering data to the server.
type Reporter struct {
	client  *SEP2Client
	edevID  string
	derID   string
	mupHref string
}

// NewReporter creates a reporter for the given device/DER/mirror IDs.
func NewReporter(client *SEP2Client, edevID, derID, mupHref string) *Reporter {
	return &Reporter{
		client:  client,
		edevID:  edevID,
		derID:   derID,
		mupHref: mupHref,
	}
}

// ReportStatus sends a DERStatus PUT to the server.
func (r *Reporter) ReportStatus(ctx context.Context, state InverterState) error {
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

	err := r.client.PutDERStatus(ctx, r.edevID, r.derID, status)
	if err != nil {
		log.Printf("reporter: status PUT failed: %v", err)
	}
	return err
}

// ReportMetering sends a MirrorMeterReading POST with active and reactive power.
func (r *Reporter) ReportMetering(ctx context.Context, state InverterState) error {
	if r.mupHref == "" {
		return nil
	}

	uomW := sep2.UomWatts
	activeW := int64(state.ActivePowerW)

	mmr := sep2.MirrorMeterReading{
		MRID:           "reading-" + time.Now().Format("20060102-150405"),
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

	err := r.client.PostMeterReading(ctx, r.mupHref, mmr)
	if err != nil {
		log.Printf("reporter: metering POST failed: %v", err)
	}
	return err
}
