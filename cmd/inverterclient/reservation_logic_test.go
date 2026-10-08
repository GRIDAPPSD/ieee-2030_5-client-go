package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

var t0 = time.Unix(1_800_000_000, 0)

// One function converts the reservation's charging-positive power to the DER
// discharge-positive sign: a charge becomes negative, a discharge positive.
func TestChargingToDER_ChargeIsNegativeDischargeIsPositive(t *testing.T) {
	if got := chargingToDER(2000); got != -2000 {
		t.Errorf("charge 2000 W -> %v, want -2000", got)
	}
	if got := chargingToDER(-1500); got != 1500 {
		t.Errorf("discharge 1500 W -> %v, want 1500", got)
	}
	if got := chargingToDER(0); got != 0 || math.Signbit(got) {
		t.Errorf("zero -> %v (signbit %v), want +0", got, math.Signbit(got))
	}
	if got := chargingToDER(chargingToDER(750)); got != 750 {
		t.Errorf("round trip 750 -> %v", got)
	}
}

func fptr(v float64) *float64 { return &v }
func iptr(v int) *int         { return &v }

func writeRequestFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "frq-request.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConsumeOverlay(t *testing.T) {
	t.Run("absent file is no overlay", func(t *testing.T) {
		o, err := consumeOverlay(filepath.Join(t.TempDir(), "none.json"))
		if err != nil || o != (frqOverlay{}) {
			t.Fatalf("got %+v, %v; want zero overlay, nil", o, err)
		}
	})
	t.Run("fields are read and the file deleted", func(t *testing.T) {
		p := writeRequestFile(t, `{"energy_wh": 4000, "power_w": 2000}`)
		o, err := consumeOverlay(p)
		if err != nil {
			t.Fatal(err)
		}
		if o.EnergyWh == nil || *o.EnergyWh != 4000 || o.PowerW == nil || *o.PowerW != 2000 || o.StartInS != nil || o.DurationS != nil {
			t.Errorf("overlay = %+v, want energy 4000 power 2000 and nothing else", o)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("request file still present after it was consumed: %v", err)
		}
	})
	t.Run("malformed file is an error and is still deleted", func(t *testing.T) {
		p := writeRequestFile(t, `{"energy_wh": `)
		if _, err := consumeOverlay(p); err == nil {
			t.Error("malformed file accepted")
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Error("malformed file left to be re-read by the next signal")
		}
	})
	t.Run("unknown key is an error", func(t *testing.T) {
		p := writeRequestFile(t, `{"energy_wh": 4000, "powr_w": 1}`)
		if _, err := consumeOverlay(p); err == nil {
			t.Error("misspelt key accepted")
		}
	})
}

func TestOverlayApply_KeepsDefaultsForAbsentFields(t *testing.T) {
	base := frqSettings{EnergyWh: 6000, PowerW: 3000, StartInS: 2400, DurationS: 3600, MinLeadS: 60, GraceS: 60}
	got := frqOverlay{PowerW: fptr(1000), DurationS: iptr(900)}.apply(base)
	want := frqSettings{EnergyWh: 6000, PowerW: 1000, StartInS: 2400, DurationS: 900, MinLeadS: 60, GraceS: 60}
	if got != want {
		t.Errorf("apply = %+v, want %+v", got, want)
	}
}

func TestBuildFlowRequest_CarriesEnergyPowerIntervalAndMRID(t *testing.T) {
	s := frqSettings{EnergyWh: 6000, PowerW: 3000, StartInS: 2400, DurationS: 3600, MinLeadS: 60, GraceS: 60}
	req, err := buildFlowRequest(s, t0, "ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if req.MRID != "ABCDEF" {
		t.Errorf("mRID = %q", req.MRID)
	}
	if req.EnergyRequested == nil || req.EnergyRequested.Value != 6000 || req.EnergyRequested.Multiplier != 0 {
		t.Errorf("energy = %+v, want 6000 x10^0", req.EnergyRequested)
	}
	if req.PowerRequested == nil || req.PowerRequested.Value != 3000 {
		t.Errorf("power = %+v, want 3000", req.PowerRequested)
	}
	if iv := req.IntervalRequested; iv == nil || iv.Start != t0.Unix()+2400 || iv.Duration != 3600 {
		t.Errorf("interval = %+v, want start %d duration 3600", iv, t0.Unix()+2400)
	}
	if req.RequestStatus.RequestStatus != sep2.RequestStatusRequested || req.RequestStatus.DateTime != t0.Unix() {
		t.Errorf("RequestStatus = %+v, want Requested at %d", req.RequestStatus, t0.Unix())
	}
	if req.CreationTime != t0.Unix() {
		t.Errorf("creationTime = %d", req.CreationTime)
	}
}

// A discharge request (negative energy) carries a negative power, so the
// server sees one direction, not a charge of power against a discharge of
// energy.
func TestBuildFlowRequest_DischargeKeepsOneDirection(t *testing.T) {
	s := frqSettings{EnergyWh: -4000, PowerW: 2000, StartInS: 2400, DurationS: 3600, MinLeadS: 60, GraceS: 60}
	req, err := buildFlowRequest(s, t0, "M")
	if err != nil {
		t.Fatal(err)
	}
	if req.EnergyRequested.Value != -4000 || req.PowerRequested.Value != -2000 {
		t.Errorf("energy %d power %d, want -4000 and -2000", req.EnergyRequested.Value, req.PowerRequested.Value)
	}
}

func TestBuildFlowRequest_Refusals(t *testing.T) {
	ok := frqSettings{EnergyWh: 6000, PowerW: 3000, StartInS: 2400, DurationS: 3600, MinLeadS: 60, GraceS: 60}
	cases := map[string]func(*frqSettings){
		"starts exactly at the minimum lead": func(s *frqSettings) { s.StartInS = 60 },
		"starts sooner than the lead":        func(s *frqSettings) { s.StartInS = 10 },
		"starts in the past":                 func(s *frqSettings) { s.StartInS = -5 },
		"zero energy":                        func(s *frqSettings) { s.EnergyWh = 0 },
		"zero power":                         func(s *frqSettings) { s.PowerW = 0 },
		"zero duration":                      func(s *frqSettings) { s.DurationS = 0 },
		"zero grace":                         func(s *frqSettings) { s.GraceS = 0 },
		"grace not below the duration":       func(s *frqSettings) { s.DurationS = 60 },
		"start_in_s overflows a Duration":    func(s *frqSettings) { s.StartInS = math.MaxInt64 },
		"power not exact at int16 scale":     func(s *frqSettings) { s.PowerW = 40001 },
	}
	for name, mutate := range cases {
		s := ok
		mutate(&s)
		if _, err := buildFlowRequest(s, t0, "M"); err == nil {
			t.Errorf("%s: request built, want a refusal", name)
		}
	}
	// One past the lead is accepted: the boundary is strict.
	s := ok
	s.StartInS = 61
	if _, err := buildFlowRequest(s, t0, "M"); err != nil {
		t.Errorf("start 61 s with lead 60 s: %v", err)
	}
}

func TestBuildFlowRequest_LargePowerUsesMultiplier(t *testing.T) {
	s := frqSettings{EnergyWh: 100000, PowerW: 40000, StartInS: 2400, DurationS: 3600, MinLeadS: 60, GraceS: 60}
	req, err := buildFlowRequest(s, t0, "M")
	if err != nil {
		t.Fatal(err)
	}
	if got := scaled(int64(req.PowerRequested.Value), req.PowerRequested.Multiplier); got != 40000 {
		t.Errorf("power decodes to %v W, want 40000 (value %d x10^%d)", got, req.PowerRequested.Value, req.PowerRequested.Multiplier)
	}
}

func resp(mrid, subject string, created int64, status uint8, iv *sep2.DateTimeInterval, whAvail int64, wAvail int16) sep2.FlowReservationResponse {
	r := sep2.FlowReservationResponse{Subject: subject}
	r.MRID = mrid
	r.CreationTime = created
	r.EventStatus = &sep2.EventStatus{CurrentStatus: status}
	r.Interval = iv
	if whAvail != 0 {
		r.EnergyAvailable = &sep2.SignedRealEnergy{Value: whAvail}
	}
	if wAvail != 0 {
		r.PowerAvailable = &sep2.ActivePower{Value: wAvail}
	}
	return r
}

func listOf(rs ...sep2.FlowReservationResponse) sep2.FlowReservationResponseList {
	return sep2.FlowReservationResponseList{FlowReservationResponse: rs}
}

func TestEffectiveResponse(t *testing.T) {
	iv := &sep2.DateTimeInterval{Start: t0.Unix() + 100, Duration: 3600}
	t.Run("newest non-cancelled for the request wins", func(t *testing.T) {
		got, ok := effectiveResponse(listOf(
			resp("OLD", "REQ", 100, sep2.EventStatusScheduled, iv, 1, 1),
			resp("NEW", "REQ", 300, sep2.EventStatusScheduled, iv, 1, 1),
			resp("MID", "REQ", 200, sep2.EventStatusScheduled, iv, 1, 1),
		), "REQ")
		if !ok || got.MRID != "NEW" {
			t.Fatalf("got %q ok=%v, want NEW", got.MRID, ok)
		}
	})
	t.Run("a cancelled newest is skipped for the older live one", func(t *testing.T) {
		got, ok := effectiveResponse(listOf(
			resp("LIVE", "REQ", 100, sep2.EventStatusScheduled, iv, 1, 1),
			resp("DEAD", "REQ", 300, sep2.EventStatusCancelled, iv, 1, 1),
		), "REQ")
		if !ok || got.MRID != "LIVE" {
			t.Fatalf("got %q ok=%v, want LIVE", got.MRID, ok)
		}
	})
	t.Run("cancelled with randomization and superseded are not live", func(t *testing.T) {
		for _, st := range []uint8{3, sep2.EventStatusSuperseded} {
			if _, ok := effectiveResponse(listOf(resp("X", "REQ", 100, st, iv, 1, 1)), "REQ"); ok {
				t.Errorf("status %d treated as live", st)
			}
		}
	})
	t.Run("another request's response is ignored", func(t *testing.T) {
		if _, ok := effectiveResponse(listOf(resp("X", "OTHER", 100, sep2.EventStatusScheduled, iv, 1, 1)), "REQ"); ok {
			t.Error("response for another request taken")
		}
	})
	t.Run("of two equal creation times the later in the list wins", func(t *testing.T) {
		got, ok := effectiveResponse(listOf(
			resp("FIRST", "REQ", 100, sep2.EventStatusScheduled, iv, 1, 1),
			resp("SECOND", "REQ", 100, sep2.EventStatusScheduled, iv, 1, 1),
		), "REQ")
		if !ok || got.MRID != "SECOND" {
			t.Fatalf("got %q ok=%v, want SECOND", got.MRID, ok)
		}
	})
	t.Run("a response without an event status is live", func(t *testing.T) {
		r := resp("X", "REQ", 100, 0, iv, 1, 1)
		r.EventStatus = nil
		if _, ok := effectiveResponse(listOf(r), "REQ"); !ok {
			t.Error("nil EventStatus treated as cancelled")
		}
	})
}

func TestJudge(t *testing.T) {
	start := t0.Add(2400 * time.Second)
	rq := requestFacts{start: start, end: start.Add(time.Hour), energyWh: 6000, powerW: 3000}
	grace := 60 * time.Second
	iv := &sep2.DateTimeInterval{Start: start.Unix(), Duration: 3600}
	granted := resp("G", "REQ", start.Unix()-100, sep2.EventStatusScheduled, iv, 6000, 3000)

	t.Run("no answer before the start plus the grace is pending", func(t *testing.T) {
		for _, now := range []time.Time{start.Add(-time.Second), start, start.Add(grace - time.Second)} {
			if k, _ := judge(listOf(), "REQ", rq, grace, now); k != answerPending {
				t.Errorf("at %v from the start: got %v, want pending", now.Sub(start), k)
			}
		}
	})
	t.Run("no answer at the start plus the grace is not granted", func(t *testing.T) {
		if k, _ := judge(listOf(), "REQ", rq, grace, start.Add(grace)); k != answerExpired {
			t.Errorf("got %v, want expired", k)
		}
		if k, _ := judge(listOf(), "REQ", rq, grace, start.Add(time.Hour)); k != answerExpired {
			t.Errorf("long after start: got %v, want expired", k)
		}
	})
	t.Run("the first answer is on time up to the grace and late after it", func(t *testing.T) {
		now := start.Add(10 * time.Minute)
		for created, want := range map[int64]answerKind{
			start.Unix() + 60: answerGranted,
			start.Unix() + 61: answerLate,
		} {
			g := granted
			g.CreationTime = created
			if k, _ := judge(listOf(g), "REQ", rq, grace, now); k != want {
				t.Errorf("created %d s after the start: got %v, want %v", created-start.Unix(), k, want)
			}
		}
	})
	t.Run("a late first answer still carries its terms", func(t *testing.T) {
		g := granted
		g.CreationTime = start.Unix() + 500
		k, terms := judge(listOf(g), "REQ", rq, grace, start.Add(10*time.Minute))
		if k != answerLate || terms.EnergyWh != 6000 || terms.PowerW != 3000 || terms.ResponseMRID != "G" {
			t.Errorf("got %v %+v, want late with the 6000 Wh 3000 W terms", k, terms)
		}
	})
	t.Run("a revision of an on-time chain is followed even when created after the grace", func(t *testing.T) {
		rev := resp("G2", "REQ", start.Unix()+900, sep2.EventStatusScheduled, iv, 2000, 1000)
		k, terms := judge(listOf(granted, rev), "REQ", rq, grace, start.Add(time.Hour/2))
		if k != answerGranted || terms.ResponseMRID != "G2" || terms.EnergyWh != 2000 {
			t.Errorf("got %v %+v, want granted under the revision", k, terms)
		}
	})
	t.Run("an on-time chain with every answer cancelled is revoked", func(t *testing.T) {
		g := granted
		g.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
		k, terms := judge(listOf(g), "REQ", rq, grace, start.Add(time.Minute))
		if k != answerRevoked || terms != (grantTerms{}) {
			t.Errorf("got %v %+v, want revoked with no terms", k, terms)
		}
	})
	t.Run("zero duration is a denial", func(t *testing.T) {
		d := resp("D", "REQ", start.Unix()-100, sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: start.Unix(), Duration: 0}, 0, 0)
		if k, _ := judge(listOf(d), "REQ", rq, grace, t0); k != answerDenied {
			t.Errorf("got %v, want denied", k)
		}
	})
	t.Run("a grant carries its interval, energy and power", func(t *testing.T) {
		k, g := judge(listOf(granted), "REQ", rq, grace, t0)
		if k != answerGranted {
			t.Fatalf("got %v, want granted", k)
		}
		if g.EnergyWh != 6000 || g.PowerW != 3000 || !g.Start.Equal(start) || !g.End.Equal(start.Add(time.Hour)) || g.ResponseMRID != "G" || g.Clipped != "" {
			t.Errorf("terms = %+v", g)
		}
	})
	t.Run("multipliers are applied", func(t *testing.T) {
		r := granted
		r.EnergyAvailable = &sep2.SignedRealEnergy{Multiplier: 3, Value: 6}
		r.PowerAvailable = &sep2.ActivePower{Multiplier: 1, Value: -300}
		_, g := judge(listOf(r), "REQ", rq, grace, t0)
		if g.EnergyWh != 6000 || g.PowerW != 3000 {
			t.Errorf("energy %v power %v, want 6000 and 3000 (a negative power is a magnitude)", g.EnergyWh, g.PowerW)
		}
	})
	t.Run("the direction is the request's, not the grant's", func(t *testing.T) {
		dis := requestFacts{start: start, end: start.Add(time.Hour), energyWh: -3000, powerW: 3000}
		for _, wh := range []int64{3000, -3000} {
			r := granted
			r.EnergyAvailable = &sep2.SignedRealEnergy{Value: wh}
			r.PowerAvailable = &sep2.ActivePower{Value: int16(wh)}
			_, g := judge(listOf(r), "REQ", dis, grace, t0)
			if g.EnergyWh != -3000 {
				t.Errorf("granted %+d Wh to a discharge request: dispatched %v Wh, want -3000", wh, g.EnergyWh)
			}
			if (wh > 0) != (g.Clipped != "") {
				t.Errorf("granted %+d Wh: clipped note %q, want one exactly when the sign differs", wh, g.Clipped)
			}
		}
	})
	t.Run("magnitudes are clipped to the request", func(t *testing.T) {
		r := granted
		r.EnergyAvailable = &sep2.SignedRealEnergy{Value: 9000}
		r.PowerAvailable = &sep2.ActivePower{Value: 5000}
		_, g := judge(listOf(r), "REQ", rq, grace, t0)
		if g.EnergyWh != 6000 || g.PowerW != 3000 || g.Clipped == "" {
			t.Errorf("terms = %+v, want 6000 Wh 3000 W and a clipped note", g)
		}
	})
	t.Run("an answer missing its terms is unusable", func(t *testing.T) {
		for name, mutate := range map[string]func(*sep2.FlowReservationResponse){
			"no interval":         func(r *sep2.FlowReservationResponse) { r.Interval = nil },
			"no energy":           func(r *sep2.FlowReservationResponse) { r.EnergyAvailable = nil },
			"no power":            func(r *sep2.FlowReservationResponse) { r.PowerAvailable = nil },
			"zero energy":         func(r *sep2.FlowReservationResponse) { r.EnergyAvailable = &sep2.SignedRealEnergy{} },
			"explicit zero power": func(r *sep2.FlowReservationResponse) { r.PowerAvailable = &sep2.ActivePower{} },
		} {
			r := granted
			mutate(&r)
			if k, _ := judge(listOf(r), "REQ", rq, grace, t0); k != answerInvalid {
				t.Errorf("%s: got %v, want unusable", name, k)
			}
		}
	})
}

// firstAnswer is the smallest creationTime of any status, the earlier of two
// equal times.
func TestFirstAnswer(t *testing.T) {
	iv := &sep2.DateTimeInterval{Start: t0.Unix(), Duration: 3600}
	got, ok := firstAnswer(listOf(
		resp("B", "REQ", 200, sep2.EventStatusCancelled, iv, 1, 1),
		resp("A", "REQ", 100, sep2.EventStatusScheduled, iv, 1, 1),
		resp("A2", "REQ", 100, sep2.EventStatusScheduled, iv, 1, 1),
		resp("X", "OTHER", 1, sep2.EventStatusScheduled, iv, 1, 1),
	), "REQ")
	if !ok || got.MRID != "A" {
		t.Errorf("got %q ok=%v, want A", got.MRID, ok)
	}
}

func TestFleetTargetW(t *testing.T) {
	g := grantTerms{Start: t0, End: t0.Add(time.Hour), EnergyWh: 6000, PowerW: 3000}
	step := 5 * time.Second
	cases := []struct {
		name      string
		terms     grantTerms
		delivered float64
		at        time.Time
		want      float64
	}{
		{"before the interval", g, 0, t0.Add(-time.Second), 0},
		{"at the end", g, 0, t0.Add(time.Hour), 0},
		{"power bound binds when the even rate is higher", g, 0, t0, 3000},
		{"even rate binds when lower than the power", grantTerms{Start: t0, End: t0.Add(time.Hour), EnergyWh: 1000, PowerW: 3000}, 0, t0, 1000},
		{"energy spent", g, 6000, t0.Add(10 * time.Minute), 0},
		{"energy overspent", g, 6500, t0.Add(10 * time.Minute), 0},
		// 1 Wh left with 5 s to go would be 720 W evenly, and 720 W over one
		// 5 s step is exactly the 1 Wh left; with 3 s to go the even rate is
		// 1200 W but one 5 s step at that rate would move 1.67 Wh, so the
		// step cap holds it to 720 W.
		{"even rate at the step length", g, 5999, t0.Add(time.Hour - 5*time.Second), 720},
		{"remaining energy caps one step", g, 5999, t0.Add(time.Hour - 3*time.Second), 720},
		{"discharge mirrors charge", grantTerms{Start: t0, End: t0.Add(time.Hour), EnergyWh: -6000, PowerW: 3000}, 0, t0, -3000},
		{"discharge delivered counts toward the same energy", grantTerms{Start: t0, End: t0.Add(time.Hour), EnergyWh: -6000, PowerW: 3000}, -6000, t0.Add(time.Minute), 0},
	}
	for _, c := range cases {
		got := fleetTargetW(c.terms, c.delivered, c.at, step)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: target %v W, want %v W", c.name, got, c.want)
		}
	}
}

func TestSplitTarget(t *testing.T) {
	a := fleetMember{key: "a", ratedW: 6000, up: true}
	b := fleetMember{key: "b", ratedW: 2000, up: true}

	t.Run("proportional to rating", func(t *testing.T) {
		got := splitTarget(4000, []fleetMember{a, b})
		if got["a"] != 3000 || got["b"] != 1000 {
			t.Errorf("split = %v, want a 3000 b 1000", got)
		}
	})
	t.Run("discharge is negative in the charging convention", func(t *testing.T) {
		got := splitTarget(-4000, []fleetMember{a, b})
		if got["a"] != -3000 || got["b"] != -1000 {
			t.Errorf("split = %v, want a -3000 b -1000", got)
		}
	})
	t.Run("clipped to the fleet's headroom", func(t *testing.T) {
		got := splitTarget(20000, []fleetMember{a, b})
		if got["a"] != 6000 || got["b"] != 2000 {
			t.Errorf("split = %v, want each at its rating", got)
		}
	})
	t.Run("a down device and a full battery get no share when charging", func(t *testing.T) {
		down := fleetMember{key: "down", ratedW: 6000, up: false}
		full := fleetMember{key: "full", ratedW: 6000, up: true, soc: 1, knownSOC: true}
		got := splitTarget(1000, []fleetMember{down, full, b})
		if got["b"] != 1000 {
			t.Errorf("split = %v, want all 1000 on b", got)
		}
		if _, ok := got["down"]; ok {
			t.Error("down device was given a share")
		}
		if _, ok := got["full"]; ok {
			t.Error("full battery was given a charge")
		}
	})
	t.Run("an empty battery still charges but does not discharge", func(t *testing.T) {
		empty := fleetMember{key: "empty", ratedW: 4000, up: true, soc: 0, knownSOC: true}
		if got := splitTarget(1000, []fleetMember{empty}); got["empty"] != 1000 {
			t.Errorf("charge: %v", got)
		}
		if got := splitTarget(-1000, []fleetMember{empty}); len(got) != 0 {
			t.Errorf("discharge of an empty battery: %v", got)
		}
	})
	t.Run("no headroom gives an empty split", func(t *testing.T) {
		if got := splitTarget(1000, []fleetMember{{key: "x", ratedW: 100, up: false}}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
}
