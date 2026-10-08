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
	"sync"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// maxMirrorBackoff caps the wait between MirrorUsagePoint attempts for one
// device. The aggregator never gives up on a mirror: a server that is down
// for minutes at start must not leave a device without readings for the
// life of the process, so retries continue at this cadence.
const maxMirrorBackoff = 5 * time.Minute

// mirrorBackoff is the wait after the nth consecutive failed attempt: the
// report interval doubled per failure, capped at maxMirrorBackoff. A zero
// interval means no wait, so a retry happens on the next tick.
func mirrorBackoff(base time.Duration, failures int) time.Duration {
	d := base
	for i := 1; i < failures && d < maxMirrorBackoff; i++ {
		d *= 2
	}
	return min(d, maxMirrorBackoff)
}

// startManagedForRole starts the aggregator's managed devices from the sim
// config. An aggregator run without a sim config is valid (it has no managed
// set to resolve), so that state is logged rather than silent and returns a
// nil fleet.
func startManagedForRole(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, edevListHref string, simFile *simconfig.File, reportInterval time.Duration) (*managedFleet, error) {
	switch {
	case simFile == nil:
		log.Println("aggregator role: no sim config; no managed devices, so no device sessions, mirrors or readings will run")
		return nil, nil
	case len(simFile.Managed) == 0:
		log.Println("aggregator role: sim config lists no managed devices; no device sessions, mirrors or readings will run")
		return nil, nil
	}
	return startManaged(ctx, client, dcap, edevListHref, simFile.Managed, reportInterval)
}

// startManagedOrExit starts the managed devices and hands a start error to
// exit, which does not return in main (fatalf). It exists so a test can
// observe that the error reaches the exit path.
func startManagedOrExit(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, edevListHref string, simFile *simconfig.File, reportInterval time.Duration, exit func(format string, args ...any)) *managedFleet {
	fleet, err := startManagedForRole(ctx, client, dcap, edevListHref, simFile, reportInterval)
	if err != nil {
		exit("managed devices: %v", err)
		return nil
	}
	return fleet
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
	kind       string // "pv" or "battery": only a battery is dispatched under a grant
	ratedW     float64
	dev        device.DERDevice
	reporter   *inverter.Reporter
	lastReport time.Time

	// The mirror is retried on later ticks, with backoff, until it exists.
	// mupListHref is empty when the server advertised no
	// MirrorUsagePointList, which no retry can fix.
	client        *inverter.SEP2Client
	mupListHref   string
	mirrorTries   int
	mirrorBase    time.Duration
	nextMirrorTry time.Time
	hasMirror     bool
	now           func() time.Time

	// down is set when the last tick could not read or apply the device,
	// so a grant leaves it out of its split until a tick succeeds.
	down bool

	// edev is this device's own copy of its EndDevice, so its session
	// follows redirects without touching another device's links. ctl is its
	// control state, set before any session goroutine starts.
	edev sep2.EndDevice
	ctl  *controlSession

	// DER resources (managed_status.go): der is set once the device's
	// DERList has been read; setupDone once capability and settings are
	// PUT; lastStatus is when status and availability last went out.
	capacityWh         float64
	der                *derLinks
	derTries           int
	nextDERTry         time.Time
	setupDone          bool
	setupCap, setupSet bool
	lastStatus         time.Time
}

// managedFleet is every managed device session of one aggregator process.
type managedFleet struct {
	devices        []*managedDevice
	reportInterval time.Duration
	// statusInterval is how often status and availability go out; zero
	// means reportInterval.
	statusInterval time.Duration

	// dispatch, when set, plans the battery setpoints inside a grant. now is
	// the server-synchronized clock the grant's interval is read against.
	dispatch *dispatcher
	now      func() time.Time

	// wg counts the control session goroutines (StartControls).
	wg sync.WaitGroup
}

// setDispatcher installs the dispatcher that plans battery setpoints under
// a grant. A nil fleet (an aggregator with no managed devices) ignores it.
func (f *managedFleet) setDispatcher(d *dispatcher) {
	if f != nil {
		f.dispatch = d
	}
}

// batteries lists the battery devices as the grant split sees them. A
// device that reports its state of charge has it checked; one that does not
// is treated as able to move in either direction.
func (f *managedFleet) batteries() []fleetMember {
	var out []fleetMember
	for _, md := range f.devices {
		// A device a control is driving takes no share of the grant; its
		// achieved power still counts against it (Tick).
		if md.kind != "battery" || (md.ctl != nil && md.ctl.active()) {
			continue
		}
		m := fleetMember{key: md.lfdi, ratedW: md.ratedW, up: !md.down}
		if s, ok := md.dev.(interface{ SOC() float64 }); ok {
			m.soc, m.knownSOC = s.SOC(), true
		}
		out = append(out, m)
	}
	return out
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

	fleet := &managedFleet{reportInterval: reportInterval, now: client.Now}
	for _, m := range configured {
		md, err := startManagedDevice(ctx, client, dcap, m, reportInterval)
		if err != nil {
			return nil, err
		}
		md.edev = ownEndDevice(list, m.LFDI)
		md.ctl = newControlSession(m.Controls)
		fleet.devices = append(fleet.devices, md)
		log.Printf("managed device %s: LFDI %s", m.Name, m.LFDI)
	}
	return fleet, nil
}

// ownEndDevice returns a copy of the listed EndDevice with the given LFDI,
// its FSA list link copied too: that link is rewritten when the server
// redirects, and a shared one would move every device's session.
func ownEndDevice(list sep2.EndDeviceList, lfdi string) sep2.EndDevice {
	for _, ed := range list.EndDevice {
		if !strings.EqualFold(ed.LFDI, lfdi) {
			continue
		}
		if ed.FunctionSetAssignmentsListLink != nil {
			l := *ed.FunctionSetAssignmentsListLink
			ed.FunctionSetAssignmentsListLink = &l
		}
		return ed
	}
	return sep2.EndDevice{LFDI: lfdi}
}

func startManagedDevice(ctx context.Context, client *inverter.SEP2Client, dcap sep2.DeviceCapability, m simconfig.Managed, reportInterval time.Duration) (*managedDevice, error) {
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

	md := &managedDevice{name: m.Name, lfdi: m.LFDI, kind: m.Device.Type, ratedW: m.Device.RatedW, capacityWh: m.Device.CapacityWh, dev: replay, client: client, mirrorBase: reportInterval, now: time.Now}
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
	if md.der != nil {
		md.reporter.SetManagedDERHrefs(md.der.status, md.der.availability)
	}
	return nil
}

// tryMirror makes one MirrorUsagePoint attempt when the device has none and
// its backoff has elapsed, and logs the outcome. It reports whether the
// device now has a mirror.
func (md *managedDevice) tryMirror(ctx context.Context) bool {
	if md.hasMirror {
		return true
	}
	if md.mupListHref == "" {
		return false
	}
	if md.now().Before(md.nextMirrorTry) {
		return false
	}
	md.mirrorTries++
	if !md.postMirror(ctx) {
		md.nextMirrorTry = md.now().Add(mirrorBackoff(md.mirrorBase, md.mirrorTries))
		return false
	}
	return true
}

// postMirror makes one MirrorUsagePoint POST and installs the reporter that
// posts to the new mirror.
func (md *managedDevice) postMirror(ctx context.Context) bool {
	mupLoc, err := md.client.CreateMirrorUsagePoint(inverter.WithTarget(ctx, md.lfdi), md.mupListHref, inverter.DeviceLFDI(md.lfdi), sep2.MirrorUsagePoint{
		MRID:                inverter.MirrorUsagePointMRID(md.lfdi),
		Description:         md.name + " Metering",
		ServiceCategoryKind: 0,
		Status:              1,
	})
	switch {
	case err != nil:
		log.Printf("managed device %s: create MirrorUsagePoint attempt %d: %v", md.name, md.mirrorTries, err)
		return false
	case mupLoc == "":
		log.Printf("managed device %s: MirrorUsagePoint POST attempt %d returned empty Location", md.name, md.mirrorTries)
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
// (with the setpoint the dispatcher planned for a battery inside a grant,
// otherwise with no controls), and post a reading when the report interval
// has passed. A failing device is logged and does not stop the others. A
// nil fleet (an aggregator with no managed devices) ticks nothing.
func (f *managedFleet) Tick(ctx context.Context) {
	if f == nil {
		return
	}
	var plan map[string]float64
	if f.dispatch != nil {
		now := time.Now
		if f.now != nil {
			now = f.now
		}
		plan = f.dispatch.plan(now(), f.batteries())
	}
	var achievedChargingW float64
	for _, md := range f.devices {
		reading, err := md.dev.ReadState(ctx)
		if err != nil {
			md.down = true
			log.Printf("managed device %s: ReadState failed: %v", md.name, err)
			continue
		}
		var base *sep2.DERControlBase
		if md.ctl != nil {
			base = md.ctl.base()
		}
		controls := inverter.ApplyControlsWithCurves(base, reading.Grid, reading.MaxPowerW, inverter.RatedW(md.ratedW), nil)
		if w, ok := plan[md.lfdi]; ok {
			controls.ActivePowerW = w
		}
		state, err := md.dev.ApplySetpoint(ctx, controls)
		if err != nil {
			md.down = true
			log.Printf("managed device %s: ApplySetpoint failed: %v", md.name, err)
			continue
		}
		md.down = false
		md.reportDER(ctx, state, f.statusEvery())
		if md.kind == "battery" {
			achievedChargingW += chargingToDER(state.ActivePowerW)
		}
		md.tryMirror(ctx)
		if md.now().Sub(md.lastReport) < f.reportInterval {
			continue
		}
		if !md.hasMirror {
			log.Printf("managed device %s: reading skipped: no MirrorUsagePoint for this device", md.name)
			md.lastReport = md.now()
			continue
		}
		if err := md.reporter.ReportMetering(ctx, state); err != nil {
			log.Printf("managed device %s: ReportMetering failed: %v", md.name, err)
		}
		md.lastReport = md.now()
	}
	if f.dispatch != nil {
		f.dispatch.record(achievedChargingW)
	}
}
