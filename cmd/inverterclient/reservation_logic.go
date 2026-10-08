package main

// The pure parts of the aggregator's flow reservation
// (GRIDAPPSD/ieee-2030_5-client-go#73): building the request, choosing the
// effective answer, and turning a grant into per-device setpoints. Nothing
// here touches the network or a clock; reservation.go drives it.
//
// Sign convention: a reservation is in the request's convention, charging
// positive (energyRequested, powerRequested, energyAvailable and
// powerAvailable keep the sign of the energy the aggregator asked for). A
// device setpoint is in the DER convention, discharge positive. The only
// place the two meet is chargingToDER.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// chargingToDER converts a power in the reservation's charging-positive
// convention to the DER discharge-positive convention. It is its own
// inverse, so the same function reads a device's achieved output back into
// the reservation's convention.
func chargingToDER(chargingW float64) float64 {
	return 0 - chargingW
}

// frqSettings is one reservation as the operator wants it, after the
// config defaults and the request file have been merged. EnergyWh is
// charging positive; PowerW is a magnitude, given the energy's direction.
type frqSettings struct {
	EnergyWh  float64
	PowerW    float64
	StartInS  int
	DurationS int
	MinLeadS  int
	GraceS    int
}

// frqOverlay is the request file the reserve script writes: only the
// fields the operator gave. A nil field keeps the config default.
type frqOverlay struct {
	EnergyWh  *float64 `json:"energy_wh"`
	PowerW    *float64 `json:"power_w"`
	StartInS  *int     `json:"start_in_s"`
	DurationS *int     `json:"duration_s"`
}

// apply returns s with every field the overlay carries replaced.
func (o frqOverlay) apply(s frqSettings) frqSettings {
	if o.EnergyWh != nil {
		s.EnergyWh = *o.EnergyWh
	}
	if o.PowerW != nil {
		s.PowerW = *o.PowerW
	}
	if o.StartInS != nil {
		s.StartInS = *o.StartInS
	}
	if o.DurationS != nil {
		s.DurationS = *o.DurationS
	}
	return s
}

// consumeOverlay reads the request file at path and deletes it, so one file
// feeds one request. An absent file is no overlay. The file is deleted
// before it is parsed: a malformed file must not be re-read by every later
// signal. If it cannot be deleted the request is refused, for the same
// reason. A key the overlay does not know is an error, so a misspelt field
// does not silently fall back to the default.
func consumeOverlay(path string) (frqOverlay, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return frqOverlay{}, nil
	}
	if err != nil {
		return frqOverlay{}, fmt.Errorf("read request file %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return frqOverlay{}, fmt.Errorf("delete request file %s: %w", path, err)
	}
	var o frqOverlay
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return frqOverlay{}, fmt.Errorf("parse request file %s: %w", path, err)
	}
	return o, nil
}

const (
	maxInt48 = 1<<47 - 1
	maxInt16 = 1<<15 - 1
)

// signedEnergy encodes whole watt-hours as a SignedRealEnergy.
func signedEnergy(wh float64) (sep2.SignedRealEnergy, error) {
	v := math.Round(wh)
	if math.IsNaN(v) || math.Abs(v) > maxInt48 {
		return sep2.SignedRealEnergy{}, fmt.Errorf("energy %v Wh does not fit SignedRealEnergy", wh)
	}
	return sep2.SignedRealEnergy{Value: int64(v)}, nil
}

// activePower encodes whole watts as an ActivePower, whose value is a
// 16-bit integer: a larger power is carried with a power-of-ten multiplier
// and must be exact at that scale, never rounded.
func activePower(w float64) (sep2.ActivePower, error) {
	v := math.Round(w)
	if math.IsNaN(v) || math.Abs(v) > 1e12 {
		return sep2.ActivePower{}, fmt.Errorf("power %v W does not fit ActivePower", w)
	}
	n, mult := int64(v), int8(0)
	for n > maxInt16 || n < -maxInt16 {
		if n%10 != 0 {
			return sep2.ActivePower{}, fmt.Errorf("power %v W does not fit ActivePower exactly", w)
		}
		n /= 10
		mult++
	}
	return sep2.ActivePower{Multiplier: mult, Value: int16(n)}, nil
}

// maxStartInS bounds start_in_s so that StartInS seconds fits a
// time.Duration with room to spare (about 68 years).
const maxStartInS = 1<<31 - 1

// buildFlowRequest validates s and builds the FlowReservationRequest it
// describes, starting StartInS after now and carrying mrid. A request that
// would start no later than MinLeadS from now is refused: the server holds
// a pending request only until its start, so one starting sooner than its
// answer deadline is answered by the fallback, not by the operator.
func buildFlowRequest(s frqSettings, now time.Time, mrid string) (sep2.FlowReservationRequest, error) {
	switch {
	case s.EnergyWh == 0 || math.IsNaN(s.EnergyWh):
		return sep2.FlowReservationRequest{}, errors.New("energy_wh must be non-zero")
	case s.PowerW <= 0 || math.IsNaN(s.PowerW):
		return sep2.FlowReservationRequest{}, errors.New("power_w must be positive")
	case s.DurationS <= 0 || int64(s.DurationS) > math.MaxUint32:
		return sep2.FlowReservationRequest{}, fmt.Errorf("duration_s %d must be between 1 and %d", s.DurationS, uint32(math.MaxUint32))
	case s.GraceS <= 0 || s.GraceS >= s.DurationS:
		return sep2.FlowReservationRequest{}, fmt.Errorf("answer_grace_s %d must be positive and below duration_s %d", s.GraceS, s.DurationS)
	case s.StartInS > maxStartInS:
		return sep2.FlowReservationRequest{}, fmt.Errorf("start_in_s %d is above the limit of %d", s.StartInS, maxStartInS)
	case s.StartInS <= s.MinLeadS:
		return sep2.FlowReservationRequest{}, fmt.Errorf("start_in_s %d is not later than min_lead_s %d: the request would start too soon", s.StartInS, s.MinLeadS)
	}
	energy, err := signedEnergy(s.EnergyWh)
	if err != nil {
		return sep2.FlowReservationRequest{}, err
	}
	dir := 1.0
	if s.EnergyWh < 0 {
		dir = -1
	}
	power, err := activePower(dir * s.PowerW)
	if err != nil {
		return sep2.FlowReservationRequest{}, err
	}
	return sep2.FlowReservationRequest{
		MRID:            mrid,
		CreationTime:    now.Unix(),
		EnergyRequested: &energy,
		PowerRequested:  &power,
		IntervalRequested: &sep2.DateTimeInterval{
			Start:    now.Add(time.Duration(s.StartInS) * time.Second).Unix(),
			Duration: uint32(s.DurationS),
		},
		RequestStatus: sep2.RequestStatus{DateTime: now.Unix(), RequestStatus: sep2.RequestStatusRequested},
	}, nil
}

// answerKind is where a request stands.
type answerKind int

const (
	// answerPending: no answer yet and the requested start plus the grace
	// has not come.
	answerPending answerKind = iota
	// answerGranted: the first answer was created in time and the live
	// answer is a usable grant.
	answerGranted
	// answerDenied: the live answer's interval has zero duration.
	answerDenied
	// answerExpired: no answer by the requested start plus the grace. Read
	// again until the request is done, since an answer created in time may
	// still show up.
	answerExpired
	// answerInvalid: the live answer cannot be executed (no interval,
	// energy or power, zero energy or power, or an interval outside the
	// requested window). Nothing is dispatched under it.
	answerInvalid
	// answerLate: the first answer was created after the requested start
	// plus the grace and the live answer is a usable grant. It is acted on
	// from now to the end of its window (IEEE 2030.5-2018 10.2.3.3 m).
	answerLate
	// answerRevoked: the first answer was created in time but every answer
	// since is cancelled or superseded. Nothing is dispatched.
	answerRevoked
)

func (k answerKind) String() string {
	switch k {
	case answerPending:
		return "pending"
	case answerGranted:
		return "granted"
	case answerDenied:
		return "denied"
	case answerExpired:
		return "not granted (no answer within the grace after the requested start)"
	case answerInvalid:
		return "unusable answer"
	case answerLate:
		return "granted late (first answer created after the grace)"
	case answerRevoked:
		return "revoked (every answer is cancelled or superseded)"
	}
	return fmt.Sprintf("answerKind(%d)", int(k))
}

// dispatches reports whether a request in this state is dispatched.
func (k answerKind) dispatches() bool {
	return k == answerGranted || k == answerLate
}

// grantTerms is a grant in the request's convention: EnergyWh and PowerW
// are the multiplier-applied energyAvailable and powerAvailable, over
// [Start, End).
type grantTerms struct {
	ResponseMRID string
	Start, End   time.Time
	EnergyWh     float64
	PowerW       float64
	// Clipped lists how the response differed from the request, empty when
	// it did not. It is logged once per response.
	Clipped string
}

// requestFacts is what the answer is read against: the requested window,
// the energy in the charging-positive convention and the power magnitude.
type requestFacts struct {
	start, end time.Time
	energyWh   float64
	powerW     float64
}

// factsOf reads the facts back from the request as it was built.
func factsOf(req sep2.FlowReservationRequest) requestFacts {
	var f requestFacts
	if iv := req.IntervalRequested; iv != nil {
		f.start = time.Unix(iv.Start, 0)
		f.end = f.start.Add(time.Duration(iv.Duration) * time.Second)
	}
	if req.EnergyRequested != nil {
		f.energyWh = scaled(req.EnergyRequested.Value, req.EnergyRequested.Multiplier)
	}
	if req.PowerRequested != nil {
		f.powerW = math.Abs(scaled(int64(req.PowerRequested.Value), req.PowerRequested.Multiplier))
	}
	return f
}

// direction is +1 for a charge request and -1 for a discharge request.
func (f requestFacts) direction() float64 {
	if f.energyWh < 0 {
		return -1
	}
	return 1
}

// effectiveResponse is the newest response for subject that is not
// cancelled (event status 2 or 3) and not superseded (4), by creationTime;
// of two equal creation times the later in the list wins. ok is false when
// there is none.
func effectiveResponse(list sep2.FlowReservationResponseList, subject string) (resp sep2.FlowReservationResponse, ok bool) {
	for _, r := range list.FlowReservationResponse {
		if r.Subject != subject {
			continue
		}
		if st := r.EventStatus; st != nil {
			switch st.CurrentStatus {
			case sep2.EventStatusCancelled, 3, sep2.EventStatusSuperseded:
				continue
			}
		}
		if !ok || r.CreationTime >= resp.CreationTime {
			resp, ok = r, true
		}
	}
	return resp, ok
}

func scaled(value int64, multiplier int8) float64 {
	return float64(value) * math.Pow10(int(multiplier))
}

// firstAnswer is the response for subject with the smallest creationTime,
// whatever its status; of two equal creation times the earlier in the list.
func firstAnswer(list sep2.FlowReservationResponseList, subject string) (resp sep2.FlowReservationResponse, ok bool) {
	for _, r := range list.FlowReservationResponse {
		if r.Subject == subject && (!ok || r.CreationTime < resp.CreationTime) {
			resp, ok = r, true
		}
	}
	return resp, ok
}

// judge classifies the request whose mRID is subject and whose facts are rq,
// against the response list read at now, with the grace after the requested
// start within which the first answer counts as on time. It must be given a
// complete list from a read that succeeded: an unreadable or truncated list
// is not an empty one and is not judged.
//
// The time test applies once, to the first answer: a revision is a change to
// an answer that came in time, so every later member of an on-time chain is
// followed. Direction always comes from the request; the grant supplies
// magnitudes, clipped to what was asked.
func judge(list sep2.FlowReservationResponseList, subject string, rq requestFacts, grace time.Duration, now time.Time) (answerKind, grantTerms) {
	first, found := firstAnswer(list, subject)
	if !found {
		if now.Before(rq.start.Add(grace)) {
			return answerPending, grantTerms{}
		}
		return answerExpired, grantTerms{}
	}
	late := time.Unix(first.CreationTime, 0).After(rq.start.Add(grace))
	resp, ok := effectiveResponse(list, subject)
	if !ok {
		return answerRevoked, grantTerms{}
	}
	if resp.Interval != nil && resp.Interval.Duration == 0 {
		return answerDenied, grantTerms{ResponseMRID: resp.MRID}
	}
	if resp.Interval == nil || resp.EnergyAvailable == nil || resp.PowerAvailable == nil {
		return answerInvalid, grantTerms{ResponseMRID: resp.MRID}
	}
	t, ok := readGrant(resp, rq)
	if !ok {
		return answerInvalid, grantTerms{ResponseMRID: resp.MRID}
	}
	if late {
		return answerLate, t
	}
	return answerGranted, t
}

// readGrant turns a response that carries an interval, energy and power
// into terms read against the request: direction from the request,
// magnitudes and interval clipped to what was asked. ok is false when
// nothing usable is left: no overlap with the requested window, or zero
// energy or power.
func readGrant(resp sep2.FlowReservationResponse, rq requestFacts) (grantTerms, bool) {
	dir := rq.direction()
	gs := time.Unix(resp.Interval.Start, 0)
	ge := gs.Add(time.Duration(resp.Interval.Duration) * time.Second)
	start, end := gs, ge
	if start.Before(rq.start) {
		start = rq.start
	}
	if end.After(rq.end) {
		end = rq.end
	}
	grantedWh := scaled(resp.EnergyAvailable.Value, resp.EnergyAvailable.Multiplier)
	grantedW := scaled(int64(resp.PowerAvailable.Value), resp.PowerAvailable.Multiplier)
	wh, w := math.Abs(grantedWh), math.Abs(grantedW)
	if wh == 0 || w == 0 || !end.After(start) {
		return grantTerms{}, false
	}
	var notes []string
	if grantedWh*dir < 0 {
		notes = append(notes, fmt.Sprintf("energy sign differs from the request: asked %+.0f Wh, granted %+.0f Wh, dispatched in the request's direction", rq.energyWh, grantedWh))
	}
	if grantedW*dir < 0 {
		notes = append(notes, fmt.Sprintf("power sign differs from the request: asked %.0f W in the direction of the energy, granted %+.0f W", rq.powerW, grantedW))
	}
	if limit := math.Abs(rq.energyWh); wh > limit {
		notes = append(notes, fmt.Sprintf("energy %.0f Wh above the requested %.0f Wh, clipped", wh, limit))
		wh = limit
	}
	if w > rq.powerW {
		notes = append(notes, fmt.Sprintf("power %.0f W above the requested %.0f W, clipped", w, rq.powerW))
		w = rq.powerW
	}
	if !start.Equal(gs) || !end.Equal(ge) {
		notes = append(notes, fmt.Sprintf("interval %s to %s outside the requested window, clipped", gs.UTC().Format(time.RFC3339), ge.UTC().Format(time.RFC3339)))
	}
	if wh == 0 || w == 0 {
		return grantTerms{}, false
	}
	return grantTerms{
		ResponseMRID: resp.MRID,
		Start:        start,
		End:          end,
		EnergyWh:     dir * wh,
		PowerW:       w,
		Clipped:      strings.Join(notes, "; "),
	}, true
}

// fleetTargetW is the fleet's target power for the next step, charging
// positive, under t at now with deliveredWh already moved (in the same
// convention). It is zero outside [Start, End) and once the energy is
// spent. Otherwise its magnitude is the least of powerAvailable, the rate
// that spends the remaining energy evenly over the remaining time, and the
// rate that would spend the remaining energy in one step of length step, so
// that holding the target for a whole step cannot overshoot energyAvailable.
// step must be positive.
func fleetTargetW(t grantTerms, deliveredWh float64, now time.Time, step time.Duration) float64 {
	if now.Before(t.Start) || !now.Before(t.End) || step <= 0 {
		return 0
	}
	dir := 1.0
	if t.EnergyWh < 0 {
		dir = -1
	}
	remainingWh := math.Abs(t.EnergyWh) - dir*deliveredWh
	if remainingWh <= 0 {
		return 0
	}
	left := t.End.Sub(now).Seconds()
	return dir * math.Min(t.PowerW, math.Min(remainingWh*3600/left, remainingWh*3600/step.Seconds()))
}

// fleetMember is one battery as the split sees it.
type fleetMember struct {
	key      string
	ratedW   float64
	up       bool
	soc      float64
	knownSOC bool
}

// headroomW is how much power the member can move in the given direction:
// its rating, or zero when it is down, or full while charging, or empty
// while discharging.
func (m fleetMember) headroomW(charging bool) float64 {
	if !m.up {
		return 0
	}
	if m.knownSOC && ((charging && m.soc >= 1) || (!charging && m.soc <= 0)) {
		return 0
	}
	return m.ratedW
}

// splitTarget shares targetW (charging positive) across members in
// proportion to their headroom in the target's direction, after clipping it
// to the fleet's total headroom. The result is each member's share in the
// same convention, keyed by member key; no share exceeds its member's
// headroom.
func splitTarget(targetW float64, members []fleetMember) map[string]float64 {
	out := make(map[string]float64, len(members))
	if targetW == 0 {
		return out
	}
	charging := targetW > 0
	var total float64
	for _, m := range members {
		total += m.headroomW(charging)
	}
	if total <= 0 {
		return out
	}
	mag := math.Min(math.Abs(targetW), total)
	for _, m := range members {
		if h := m.headroomW(charging); h > 0 {
			share := mag * h / total
			if !charging {
				share = -share
			}
			out[m.key] = share
		}
	}
	return out
}
