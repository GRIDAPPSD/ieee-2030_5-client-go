package main

// Managed devices of the aggregator role (GRIDAPPSD/ieee-2030_5-client-go#72).
//
// The managed set is the configured devices the server's EndDeviceList also
// lists. Each one gets its own replay backend, one MirrorUsagePoint naming
// its LFDI, and readings that carry a flowDirection. Every request goes out
// with the device's LFDI as the guard target, so it is judged as a manager
// action for that device and an unmanaged LFDI is refused before any send.
// The aggregator posts no mirror for its own EndDevice.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// missingManaged returns an error naming every configured device the
// server's EndDeviceList does not list, or nil when all are listed.
func missingManaged(list sep2.EndDeviceList, configured []simconfig.Managed) error {
	listed := make(map[string]bool, len(list.EndDevice))
	for _, ed := range list.EndDevice {
		listed[strings.ToUpper(ed.LFDI)] = true
	}
	var missing []string
	for _, m := range configured {
		if !listed[strings.ToUpper(m.LFDI)] {
			missing = append(missing, fmt.Sprintf("%s (%s)", m.Name, m.LFDI))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("managed device not listed by the server: %s", strings.Join(missing, ", "))
	}
	return nil
}

// managedDevice is one device session: its backend, its reporter and when
// it last reported.
type managedDevice struct {
	name       string
	lfdi       string
	ratedW     float64
	dev        device.DERDevice
	reporter   *inverter.Reporter
	lastReport time.Time
}

// managedFleet is every managed device session of one aggregator process.
type managedFleet struct {
	devices        []*managedDevice
	reportInterval time.Duration
}

// startManaged resolves the managed set against the server's EndDeviceList,
// installs it in the client's guard, and starts a session per device. A
// configured device the server does not list, or one whose replay backend
// cannot be built, stops the start with an error naming it.
func startManaged(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, edevListHref string, configured []simconfig.Managed, reportInterval time.Duration) (*managedFleet, error) {
	log.Println("=== Managed devices ===")
	list, _, err := client.GetEndDeviceList(ctx, edevListHref)
	if err != nil {
		return nil, fmt.Errorf("list EndDevices for the managed set: %w", err)
	}
	if err := missingManaged(list, configured); err != nil {
		return nil, err
	}

	lfdis := make([]string, 0, len(configured))
	for _, m := range configured {
		lfdis = append(lfdis, m.LFDI)
	}
	client.SetManagedSet(guard.NewStaticManagedSet(lfdis...))

	fleet := &managedFleet{reportInterval: reportInterval}
	for _, m := range configured {
		md, err := startManagedDevice(ctx, client, dcap, m)
		if err != nil {
			return nil, err
		}
		fleet.devices = append(fleet.devices, md)
		log.Printf("managed device %s: LFDI %s", m.Name, m.LFDI)
	}
	return fleet, nil
}

func startManagedDevice(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, m simconfig.Managed) (*managedDevice, error) {
	soc := 0.0
	if m.Device.InitialSOC != nil {
		soc = *m.Device.InitialSOC
	}
	replay, err := device.NewReplay(device.ReplayConfig{
		File: m.Replay.File, Type: m.Device.Type, Clock: m.Replay.Clock, Scale: m.Replay.Scale,
		RatedW: m.Device.RatedW, CapacityWh: m.Device.CapacityWh, InitialSOC: soc,
	})
	if err != nil {
		return nil, fmt.Errorf("managed device %s (%s): %w", m.Name, m.LFDI, err)
	}

	// A mirror that cannot be made disables that device's readings, as it
	// does for the der role's own mirror; the session keeps running.
	var mmrHref string
	if dcap.MirrorUsagePointListLink == nil {
		log.Printf("managed device %s: DeviceCapability has no MirrorUsagePointListLink; readings disabled", m.Name)
	} else {
		mupLoc, err := client.CreateMirrorUsagePoint(inverter.WithTarget(ctx, m.LFDI), dcap.MirrorUsagePointListLink.Href, inverter.DeviceLFDI(m.LFDI), sep2.MirrorUsagePoint{
			MRID:                inverter.MirrorUsagePointMRID(m.LFDI),
			Description:         m.Name + " Metering",
			ServiceCategoryKind: 0,
			Status:              1,
		})
		switch {
		case err != nil:
			log.Printf("managed device %s: create MirrorUsagePoint: %v (readings disabled)", m.Name, err)
		case mupLoc == "":
			log.Printf("managed device %s: MirrorUsagePoint POST returned empty Location; readings disabled", m.Name)
		default:
			log.Printf("managed device %s: MirrorUsagePoint %s", m.Name, mupLoc)
			mmrHref = mupLoc + "/mr"
		}
	}

	reporter, err := inverter.NewReporter(client, m.LFDI, "", mmrHref)
	if err != nil {
		return nil, fmt.Errorf("managed device %s: %w", m.Name, err)
	}
	return &managedDevice{
		name: m.Name, lfdi: m.LFDI, ratedW: m.Device.RatedW,
		dev: replay, reporter: reporter.AsManager(),
	}, nil
}

// Tick advances every session one step: read the recorded output, apply it
// with no controls, and post a reading when the report interval has passed.
// A failing device is logged and does not stop the others.
func (f *managedFleet) Tick(ctx context.Context) {
	for _, md := range f.devices {
		reading, err := md.dev.ReadState(ctx)
		if err != nil {
			log.Printf("managed device %s: ReadState failed: %v", md.name, err)
			continue
		}
		controls := inverter.ApplyControlsWithCurves(nil, reading.Grid, reading.MaxPowerW, inverter.RatedW(md.ratedW), nil)
		state, err := md.dev.ApplySetpoint(ctx, controls)
		if err != nil {
			log.Printf("managed device %s: ApplySetpoint failed: %v", md.name, err)
			continue
		}
		if time.Since(md.lastReport) < f.reportInterval {
			continue
		}
		if err := md.reporter.ReportMetering(ctx, state); err != nil {
			log.Printf("managed device %s: ReportMetering failed: %v", md.name, err)
		}
		md.lastReport = time.Now()
	}
}
