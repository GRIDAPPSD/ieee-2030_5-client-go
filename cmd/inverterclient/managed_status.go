package main

// DER capability, settings, status and availability for each managed device
// (GRIDAPPSD/ieee-2030_5-client-go#76).
//
// Each managed device writes to the DER resources its own EndDevice
// advertises, with the device as the guard target. Capability and settings
// go out once, from the configured rating. Status and availability go out
// at the status interval and only from a tick that read and applied the
// device: a device whose replay source fails sends nothing, so the server
// never holds a value the device did not just produce.

import (
	"context"
	"log"
	"math"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// DERType values, IEEE 2030.5-2018 DERType.
const (
	derTypePV      uint8 = 4
	derTypeStorage uint8 = 80
)

// derLinks are the resource hrefs of a managed device's first DER.
type derLinks struct {
	capability, settings, status, availability string
}

// setStatusInterval sets how often managed devices PUT status and
// availability. A nil fleet ignores it; zero means the report interval.
func (f *managedFleet) setStatusInterval(d time.Duration) {
	if f != nil {
		f.statusInterval = d
	}
}

func (f *managedFleet) statusEvery() time.Duration {
	if f.statusInterval > 0 {
		return f.statusInterval
	}
	return f.reportInterval
}

// activePowerOf converts watts to the wire type, whose value is an int16:
// a rating past that range is carried with a power-of-ten multiplier
// instead of wrapping.
func activePowerOf(w float64) sep2.ActivePower {
	v, mult := math.Round(w), 0
	for (v > math.MaxInt16 || v < math.MinInt16) && mult < math.MaxInt8 {
		v = math.Round(v / 10)
		mult++
	}
	return sep2.ActivePower{Multiplier: int8(mult), Value: int16(v)}
}

// capabilityFor is the DERCapability a managed device reports from its
// configured rating.
func capabilityFor(kind string, ratedW float64) sep2.DERCapability {
	maxW := activePowerOf(ratedW)
	c := sep2.DERCapability{RTGMaxW: &maxW}
	t := derTypePV
	if kind == "battery" {
		t = derTypeStorage
		charge, discharge := activePowerOf(ratedW), activePowerOf(ratedW)
		c.RTGMaxChargeRateW, c.RTGMaxDischargeRateW = &charge, &discharge
	}
	c.Type = &t
	return c
}

// settingsFor is the DERSettings a managed device reports from its
// configured rating.
func settingsFor(kind string, ratedW float64, at int64) sep2.DERSettings {
	maxW := activePowerOf(ratedW)
	s := sep2.DERSettings{SetMaxW: &maxW, UpdatedTime: at}
	if kind == "battery" {
		charge, discharge := activePowerOf(ratedW), activePowerOf(ratedW)
		s.SetMaxChargeRateW, s.SetMaxDischargeRateW = &charge, &discharge
	}
	return s
}

// availabilityFor is the DERAvailability of a managed device. A PV device
// offers its rating. A battery offers its rating for discharge only while
// it holds charge, and states how long it can discharge and how long it can
// still charge at that rating; soc is its state of charge, 0 to 1, or nil
// when the backend does not report one (then only the rating is offered).
func availabilityFor(kind string, ratedW, capacityWh float64, soc *float64, at int64) sep2.DERAvailability {
	a := sep2.DERAvailability{ReadingTime: at}
	avail := ratedW
	if kind == "battery" && soc != nil {
		if *soc <= 0 {
			avail = 0
		}
		if ratedW > 0 {
			discharge := uint32(math.Round(*soc * capacityWh / ratedW * 3600))
			charge := uint32(math.Round((1 - *soc) * capacityWh / ratedW * 3600))
			a.AvailabilityDuration, a.MaxChargeDuration = &discharge, &charge
		}
	}
	w := activePowerOf(avail)
	a.StatWAvail = &w
	return a
}

// socOf returns the device's state of charge when its backend reports one.
func (md *managedDevice) socOf() *float64 {
	s, ok := md.dev.(interface{ SOC() float64 })
	if !ok || md.kind != "battery" {
		return nil
	}
	v := s.SOC()
	return &v
}

// resolveDER reads the device's DERList and keeps the links of its first
// DER, once. A failure is logged and retried on a later tick, with the
// mirror's backoff.
func (md *managedDevice) resolveDER(ctx context.Context) bool {
	if md.der != nil {
		return true
	}
	if md.edev.DERListLink == nil || md.edev.DERListLink.Href == "" {
		return false
	}
	if md.now().Before(md.nextDERTry) {
		return false
	}
	var list sep2.DERList
	_, err := md.client.Get(inverter.WithTarget(ctx, md.lfdi), guard.KindEndDeviceRead, md.edev.DERListLink.Href, &list)
	md.derTries++
	switch {
	case err != nil:
		log.Printf("managed device %s: GET DERList attempt %d: %v", md.name, md.derTries, err)
	case len(list.DER) == 0:
		log.Printf("managed device %s: DERList attempt %d lists no DER", md.name, md.derTries)
	default:
		l := &derLinks{}
		der := list.DER[0]
		if der.DERCapabilityLink != nil {
			l.capability = der.DERCapabilityLink.Href
		}
		if der.DERSettingsLink != nil {
			l.settings = der.DERSettingsLink.Href
		}
		if der.DERStatusLink != nil {
			l.status = der.DERStatusLink.Href
		}
		if der.DERAvailabilityLink != nil {
			l.availability = der.DERAvailabilityLink.Href
		}
		md.der = l
		md.reporter.SetManagedDERHrefs(l.status, l.availability)
		return true
	}
	md.nextDERTry = md.now().Add(mirrorBackoff(md.mirrorBase, md.derTries))
	return false
}

// putSetup PUTs the capability and settings once from the configured
// rating. A link the DER does not advertise is logged and skipped; a failed
// PUT is retried on the next tick.
func (md *managedDevice) putSetup(ctx context.Context) {
	ctx = inverter.WithTarget(ctx, md.lfdi)
	ok := true
	if md.der.capability == "" {
		log.Printf("managed device %s: DER has no DERCapabilityLink; skipping DERCapability PUT", md.name)
	} else if !md.setupCap {
		if err := md.client.PutDERCapability(ctx, md.der.capability, capabilityFor(md.kind, md.ratedW)); err != nil {
			log.Printf("managed device %s: PUT DERCapability: %v", md.name, err)
			ok = false
		} else {
			md.setupCap = true
		}
	}
	if md.der.settings == "" {
		log.Printf("managed device %s: DER has no DERSettingsLink; skipping DERSettings PUT", md.name)
	} else if !md.setupSet {
		if err := md.client.PutDERSettings(ctx, md.der.settings, settingsFor(md.kind, md.ratedW, md.client.Now().Unix())); err != nil {
			log.Printf("managed device %s: PUT DERSettings: %v", md.name, err)
			ok = false
		} else {
			md.setupSet = true
		}
	}
	md.setupDone = ok
}

// reportDER sends the device's capability and settings (once) and, when
// the status interval has passed, its status and availability. The caller
// calls it only for a device it just read and applied.
func (md *managedDevice) reportDER(ctx context.Context, state inverter.InverterState, every time.Duration) {
	if md.edev.DERListLink == nil || !md.resolveDER(ctx) {
		return
	}
	if !md.setupDone {
		md.putSetup(ctx)
	}
	if md.now().Sub(md.lastStatus) < every {
		return
	}
	md.lastStatus = md.now()
	at := md.client.Now()
	state.Time = at
	soc := md.socOf()
	if err := md.reporter.ReportStatusWithSOC(ctx, state, soc); err != nil {
		log.Printf("managed device %s: DERStatus PUT failed: %v", md.name, err)
	}
	if err := md.reporter.ReportAvailability(ctx, availabilityFor(md.kind, md.ratedW, md.capacityWh, soc, at.Unix())); err != nil {
		log.Printf("managed device %s: DERAvailability PUT failed: %v", md.name, err)
	}
}
