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
	// answerPending: no answer yet and the requested start has not come.
	answerPending answerKind = iota
	// answerGranted: an answer with an interval, energy and power.
	answerGranted
	// answerDenied: an answer whose interval has zero duration.
	answerDenied
	// answerExpired: the requested start came with no answer. Terminal.
	answerExpired
	// answerInvalid: an answer that cannot be executed (no interval, energy
	// or power, or zero energy). Nothing is dispatched under it.
	answerInvalid
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
		return "not granted (expired at the requested start)"
	case answerInvalid:
		return "unusable answer"
	}
	return fmt.Sprintf("answerKind(%d)", int(k))
}

// grantTerms is a grant in the request's convention: EnergyWh and PowerW
// are the multiplier-applied energyAvailable and powerAvailable, over
// [Start, End).
type grantTerms struct {
	ResponseMRID string
	Start, End   time.Time
	EnergyWh     float64
	PowerW       float64
}

// requestedStart is the start the request asked for.
func requestedStart(req sep2.FlowReservationRequest) time.Time {
	if req.IntervalRequested == nil {
		return time.Time{}
	}
	return time.Unix(req.IntervalRequested.Start, 0)
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

// judge classifies the request whose mRID is subject and whose requested
// start is reqStart, against the response list read at now. It must be
// given a list from a read that succeeded: an unreadable list is not an
// empty one and is not judged.
func judge(list sep2.FlowReservationResponseList, subject string, reqStart, now time.Time) (answerKind, grantTerms) {
	resp, ok := effectiveResponse(list, subject)
	if !ok {
		if !now.Before(reqStart) {
			return answerExpired, grantTerms{}
		}
		return answerPending, grantTerms{}
	}
	if resp.Interval != nil && resp.Interval.Duration == 0 {
		return answerDenied, grantTerms{ResponseMRID: resp.MRID}
	}
	if resp.Interval == nil || resp.EnergyAvailable == nil || resp.PowerAvailable == nil {
		return answerInvalid, grantTerms{ResponseMRID: resp.MRID}
	}
	t := grantTerms{
		ResponseMRID: resp.MRID,
		Start:        time.Unix(resp.Interval.Start, 0),
		End:          time.Unix(resp.Interval.Start, 0).Add(time.Duration(resp.Interval.Duration) * time.Second),
		EnergyWh:     scaled(resp.EnergyAvailable.Value, resp.EnergyAvailable.Multiplier),
		PowerW:       math.Abs(scaled(int64(resp.PowerAvailable.Value), resp.PowerAvailable.Multiplier)),
	}
	if t.EnergyWh == 0 || t.PowerW == 0 {
		return answerInvalid, grantTerms{ResponseMRID: resp.MRID}
	}
	return answerGranted, t
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
