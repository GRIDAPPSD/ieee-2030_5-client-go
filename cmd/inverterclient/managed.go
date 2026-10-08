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

// maxMirrorAttempts bounds the MirrorUsagePoint POSTs one managed device
// makes, the one at start included. A mirror that still fails after that is
// logged as given up on, and the device's readings stay off.
const maxMirrorAttempts = 5

// startManagedForRole starts the aggregator's managed devices from the sim
// config. An aggregator run without a sim config is valid (it has no managed
// set to resolve), so that state is logged rather than silent and returns a
// nil fleet.
func startManagedForRole(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, edevListHref string, simFile *simconfig.File, reportInterval time.Duration) (*managedFleet, error) {
	if simFile == nil || len(simFile.Managed) == 0 {
		log.Println("aggregator role: no managed devices configured (no sim config); no device sessions, mirrors or readings will run")
		return nil, nil
	}
	return startManaged(ctx, client, dcap, edevListHref, simFile.Managed, reportInterval)
}

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

	// The mirror is retried on later ticks until it exists or
	// maxMirrorAttempts is spent. mupListHref is empty when the server
	// advertised no MirrorUsagePointList, which no retry can fix.
	client       *inverter.SEP2Client
	mupListHref  string
	mirrorTries  int
	hasMirror    bool
	gaveUpLogged bool
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
	for _, m := range configured {
		if strings.EqualFold(m.LFDI, client.LFDI()) {
			return nil, fmt.Errorf("managed device %s (%s) is the aggregator's own LFDI; the aggregator never mirrors itself", m.Name, m.LFDI)
		}
	}
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

	md := &managedDevice{name: m.Name, lfdi: m.LFDI, ratedW: m.Device.RatedW, dev: replay, client: client}
	if dcap.MirrorUsagePointListLink == nil {
		log.Printf("managed device %s: DeviceCapability has no MirrorUsagePointListLink; readings disabled", m.Name)
	} else {
		md.mupListHref = dcap.MirrorUsagePointListLink.Href
	}
	if err := md.setReporter(""); err != nil {
		return nil, err
	}
	// A failed first attempt is logged and retried on later ticks.
	md.tryMirror(ctx)
	return md, nil
}

// setReporter replaces the device's reporter with one posting to mmrHref.
func (md *managedDevice) setReporter(mmrHref string) error {
	reporter, err := inverter.NewReporter(md.client, md.lfdi, "", mmrHref)
	if err != nil {
		return fmt.Errorf("managed device %s: %w", md.name, err)
	}
	md.reporter = reporter.AsManager()
	return nil
}

// tryMirror makes one MirrorUsagePoint attempt when the device has none and
// attempts remain, and logs the outcome. It reports whether the device now
// has a mirror.
func (md *managedDevice) tryMirror(ctx context.Context) bool {
	if md.hasMirror {
		return true
	}
	if md.mupListHref == "" {
		return false
	}
	if md.mirrorTries >= maxMirrorAttempts {
		if !md.gaveUpLogged {
			log.Printf("managed device %s: gave up on the MirrorUsagePoint after %d attempts; readings stay disabled", md.name, maxMirrorAttempts)
			md.gaveUpLogged = true
		}
		return false
	}
	md.mirrorTries++
	mupLoc, err := md.client.CreateMirrorUsagePoint(inverter.WithTarget(ctx, md.lfdi), md.mupListHref, inverter.DeviceLFDI(md.lfdi), sep2.MirrorUsagePoint{
		MRID:                inverter.MirrorUsagePointMRID(md.lfdi),
		Description:         md.name + " Metering",
		ServiceCategoryKind: 0,
		Status:              1,
	})
	switch {
	case err != nil:
		log.Printf("managed device %s: create MirrorUsagePoint attempt %d of %d: %v", md.name, md.mirrorTries, maxMirrorAttempts, err)
		return false
	case mupLoc == "":
		log.Printf("managed device %s: MirrorUsagePoint POST attempt %d of %d returned empty Location", md.name, md.mirrorTries, maxMirrorAttempts)
		return false
	}
	if err := md.setReporter(mupLoc + "/mr"); err != nil {
		log.Printf("%v", err)
		return false
	}
	md.hasMirror = true
	log.Printf("managed device %s: MirrorUsagePoint %s", md.name, mupLoc)
	return true
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
		md.tryMirror(ctx)
		if time.Since(md.lastReport) < f.reportInterval {
			continue
		}
		if !md.hasMirror {
			log.Printf("managed device %s: reading skipped: no MirrorUsagePoint for this device", md.name)
			md.lastReport = time.Now()
			continue
		}
		if err := md.reporter.ReportMetering(ctx, state); err != nil {
			log.Printf("managed device %s: ReportMetering failed: %v", md.name, err)
		}
		md.lastReport = time.Now()
	}
}
