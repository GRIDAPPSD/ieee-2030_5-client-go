package main

// The aggregator's flow reservation on demand
// (GRIDAPPSD/ieee-2030_5-client-go#73): SIGUSR1 posts one request on the
// aggregator's own EndDevice, the answer is followed by polling, and inside
// a grant the battery devices are dispatched. The pure decisions live in
// reservation_logic.go; this file holds the state and the I/O.
//
// Everything here runs on the main tick loop's goroutine, so none of it
// takes a lock.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// reservationClient is what the reserver needs of the SEP2 client. Every
// method acts for the aggregator's own EndDevice.
type reservationClient interface {
	PostFlowReservationRequest(ctx context.Context, listHref string, req sep2.FlowReservationRequest) (string, error)
	GetFlowReservationResponses(ctx context.Context, listHref string) (sep2.FlowReservationResponseList, error)
	PostFlowReservationResponseResponse(ctx context.Context, replyToHref string, resp sep2.FlowReservationResponseResponse) error
}

// ioTimeout bounds one reservation request, so a stalled server cannot
// hold the tick loop.
const ioTimeout = 15 * time.Second

// dispatcher turns the current grant into per-device battery setpoints and
// accounts the energy actually moved. It does not own the devices: the
// managed fleet asks it for a plan each tick and reports back what the
// devices achieved.
type dispatcher struct {
	minStep time.Duration // dispatch.tick_s: the shortest step the energy cap assumes

	terms       *grantTerms
	deliveredWh float64 // charging positive, integrated from achieved output

	lastAt       time.Time
	lastStep     time.Duration
	prevInside   bool    // the previous step ran inside the grant interval
	lastAchieved float64 // fleet achieved power after the previous step, charging positive
}

func newDispatcher(minStep time.Duration) *dispatcher {
	return &dispatcher{minStep: minStep}
}

// reset forgets the grant and the energy accounting. A new request starts
// from zero delivered; a revised answer to the same request does not reset,
// because the energy moved under the earlier answer was really moved.
func (d *dispatcher) reset() {
	*d = dispatcher{minStep: d.minStep}
}

// setGrant installs the terms to dispatch under, or nil for none.
func (d *dispatcher) setGrant(t *grantTerms) {
	d.terms = t
}

// plan returns the setpoint each battery should be commanded this step, in
// the DER convention, keyed by member key. Inside a grant interval every
// member has an entry (zero when the split gave it nothing, so a full
// battery does not run its recording against the bound); outside it the
// plan is empty and the devices run free.
func (d *dispatcher) plan(now time.Time, members []fleetMember) map[string]float64 {
	elapsed := time.Duration(0)
	if !d.lastAt.IsZero() {
		elapsed = now.Sub(d.lastAt)
	}
	// Energy moved since the previous step: the output it achieved, held
	// until the end of the interval or now, whichever comes first.
	if d.terms != nil && d.prevInside && elapsed > 0 {
		until := now
		if d.terms.End.Before(until) {
			until = d.terms.End
		}
		if span := until.Sub(d.lastAt); span > 0 {
			d.deliveredWh += d.lastAchieved * span.Hours()
		}
	}
	step := max(d.minStep, d.lastStep)
	d.lastAt, d.lastStep = now, elapsed

	if d.terms == nil || now.Before(d.terms.Start) || !now.Before(d.terms.End) {
		d.prevInside = false
		return nil
	}
	d.prevInside = true
	target := fleetTargetW(*d.terms, d.deliveredWh, now, step)
	shares := splitTarget(target, members)
	out := make(map[string]float64, len(members))
	for _, m := range members {
		out[m.key] = chargingToDER(shares[m.key])
	}
	return out
}

// record stores the fleet's achieved power after the step, charging
// positive, for the next plan to integrate.
func (d *dispatcher) record(achievedChargingW float64) {
	d.lastAchieved = achievedChargingW
}

// currentRequest is the reservation the aggregator is following.
type currentRequest struct {
	requestFacts
	mrid  string
	kind  answerKind
	terms grantTerms
	done  bool            // the window and grace have passed: no more reads
	acked map[string]bool // response mRIDs already acknowledged as received
	owed  map[string]bool // response mRIDs that asked for an acknowledgement and have none yet
	noted map[string]bool // log lines already written once, by key
}

// note reports whether key is new, and remembers it: a line keyed by a
// response is written once per response, not once per poll.
func (c *currentRequest) note(key string) bool {
	if c.noted[key] {
		return false
	}
	c.noted[key] = true
	return true
}

// reserver posts and follows the aggregator's flow reservation.
type reserver struct {
	client   reservationClient
	selfLFDI string
	reqHref  string // the aggregator's own FlowReservationRequestList
	respHref string // the aggregator's own FlowReservationResponseList
	cfg      simconfig.Frq
	disp     *dispatcher
	now      func() time.Time
	newMRID  func(selfLFDI string, now time.Time) (string, error)

	cur      *currentRequest
	nextPoll time.Time
}

// newReserver builds a reserver on the aggregator's own EndDevice links.
// An EndDevice that advertises either link missing cannot reserve; the
// error says which.
func newReserver(client reservationClient, selfLFDI string, edev sep2.EndDevice, cfg simconfig.Frq, disp *dispatcher, now func() time.Time) (*reserver, error) {
	if edev.FlowReservationRequestListLink == nil || edev.FlowReservationResponseListLink == nil {
		return nil, fmt.Errorf("the server's EndDevice advertises no FlowReservationRequestListLink and FlowReservationResponseListLink")
	}
	return &reserver{
		client: client, selfLFDI: selfLFDI,
		reqHref: edev.FlowReservationRequestListLink.Href, respHref: edev.FlowReservationResponseListLink.Href,
		cfg: cfg, disp: disp, now: now, newMRID: inverter.NewFlowReservationRequestMRID,
	}, nil
}

// Trigger handles one SIGUSR1: merge the config defaults with the request
// file (consuming it), validate, and post one request. A failure is logged
// and posts nothing. A request already being followed is replaced as the
// one to follow; withdrawing it is not done here.
func (r *reserver) Trigger(ctx context.Context) {
	if r == nil {
		return
	}
	overlay, err := consumeOverlay(r.cfg.RequestFile)
	if err != nil {
		log.Printf("flow reservation: not posted: %v", err)
		return
	}
	s := overlay.apply(frqSettings{
		EnergyWh: r.cfg.EnergyWh, PowerW: r.cfg.PowerW,
		StartInS: r.cfg.StartInS, DurationS: r.cfg.DurationS, MinLeadS: r.cfg.MinLeadS, GraceS: r.cfg.AnswerGraceS,
	})
	now := r.now()
	mrid, err := r.newMRID(r.selfLFDI, now)
	if err != nil {
		log.Printf("flow reservation: not posted: %v", err)
		return
	}
	req, err := buildFlowRequest(s, now, mrid)
	if err != nil {
		log.Printf("flow reservation: not posted: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	loc, err := r.client.PostFlowReservationRequest(ctx, r.reqHref, req)
	if err != nil {
		log.Printf("flow reservation: post failed: %v", err)
		return
	}
	if r.cur != nil {
		log.Printf("flow reservation: now following request %s instead of %s", mrid, r.cur.mrid)
	}
	r.cur = &currentRequest{
		requestFacts: factsOf(req), mrid: mrid, kind: answerPending,
		acked: map[string]bool{}, owed: map[string]bool{}, noted: map[string]bool{},
	}
	r.disp.reset()
	r.nextPoll = now
	log.Printf("flow reservation: posted request %s at %s: %.0f Wh at up to %.0f W, start %s, %ds",
		mrid, loc, s.EnergyWh, s.PowerW, r.cur.start.UTC().Format(time.RFC3339), s.DurationS)
}

// grace is the time after the requested start within which the first answer
// counts as on time, and after the requested end the request is followed
// to.
func (r *reserver) grace() time.Duration {
	return time.Duration(r.cfg.AnswerGraceS) * time.Second
}

// Poll reads the response list when the poll interval has passed and
// updates the answer. No state stops the reads before the end of the
// request's window plus the grace: a denial, an expiry or a cancellation can
// each be followed by a newer answer. A failed or truncated read changes
// nothing: an unreadable list is not read as no answer.
func (r *reserver) Poll(ctx context.Context) {
	if r == nil || r.cur == nil || r.cur.done {
		return
	}
	c := r.cur
	now := r.now()
	if !now.Before(c.end.Add(r.grace())) {
		r.finish(c)
		return
	}
	if now.Before(r.nextPoll) {
		return
	}
	r.nextPoll = now.Add(time.Duration(r.cfg.PollS) * time.Second)

	readCtx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	list, err := r.client.GetFlowReservationResponses(readCtx, r.respHref)
	if err != nil {
		log.Printf("flow reservation: reading answers failed, keeping state %q: %v", c.kind, err)
		return
	}
	if got := uint32(len(list.FlowReservationResponse)); list.All > got {
		log.Printf("flow reservation: answer list is truncated (%d of %d entries), keeping state %q", got, list.All, c.kind)
		return
	}
	kind, terms := judge(list, c.mrid, c.requestFacts, r.grace(), now)
	if kind != c.kind || terms != c.terms {
		log.Printf("flow reservation: request %s is %s%s", c.mrid, kind, describeTerms(kind, terms))
	}
	if terms.Clipped != "" && c.note("clip:"+terms.ResponseMRID) {
		log.Printf("flow reservation: response %s read against request %s: %s", terms.ResponseMRID, c.mrid, terms.Clipped)
	}
	c.kind, c.terms = kind, terms
	if kind.dispatches() {
		t := terms
		r.disp.setGrant(&t)
	} else {
		r.disp.setGrant(nil)
	}
	for _, resp := range list.FlowReservationResponse {
		if resp.Subject == c.mrid {
			r.acknowledge(ctx, c, resp, now)
		}
	}
}

// finish ends the request: the grant is dropped, reads stop, and any
// acknowledgement still owed is logged once.
func (r *reserver) finish(c *currentRequest) {
	c.done = true
	r.disp.setGrant(nil)
	owed := make([]string, 0, len(c.owed))
	for mrid := range c.owed {
		owed = append(owed, mrid)
	}
	sort.Strings(owed)
	for _, mrid := range owed {
		log.Printf("flow reservation: request %s finished; response never acknowledged: %s", c.mrid, mrid)
	}
	log.Printf("flow reservation: request %s finished: %s", c.mrid, c.kind)
}

func describeTerms(kind answerKind, t grantTerms) string {
	if !kind.dispatches() {
		return ""
	}
	return fmt.Sprintf(": %.0f Wh at up to %.0f W from %s to %s (response %s)",
		t.EnergyWh, t.PowerW, t.Start.UTC().Format(time.RFC3339), t.End.UTC().Format(time.RFC3339), t.ResponseMRID)
}

// responseRequiredReceived is bit 0 of responseRequired: the sender wants
// to be told the response was received.
const responseRequiredReceived = 0x01

// acknowledge tells the server a response was received when the response
// asks for it, once per response, whether it is a grant, a denial, an
// unusable answer or one already cancelled or superseded. The
// acknowledgement's subject is the response's own mRID. A failed post is
// retried on every poll until the request finishes.
func (r *reserver) acknowledge(ctx context.Context, c *currentRequest, resp sep2.FlowReservationResponse, now time.Time) {
	if resp.ResponseRequired == nil || uint8(*resp.ResponseRequired)&responseRequiredReceived == 0 {
		return
	}
	if resp.ReplyTo == "" || resp.MRID == "" {
		if c.note("noreply:" + resp.MRID) {
			log.Printf("flow reservation: response %s has no replyTo or no mRID but asks to be acknowledged; nothing posted", resp.MRID)
		}
		return
	}
	if c.acked[resp.MRID] {
		return
	}
	c.owed[resp.MRID] = true
	status := sep2.ResponseStatusEventReceived
	ackCtx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	err := r.client.PostFlowReservationResponseResponse(ackCtx, resp.ReplyTo, sep2.FlowReservationResponseResponse{
		Response: sep2.Response{
			CreatedDateTime: now.Unix(),
			EndDeviceLFDI:   r.selfLFDI,
			Status:          &status,
			Subject:         resp.MRID,
		},
	})
	if err != nil {
		log.Printf("flow reservation: acknowledging response %s failed, will retry: %v", resp.MRID, err)
		return
	}
	c.acked[resp.MRID] = true
	delete(c.owed, resp.MRID)
	log.Printf("flow reservation: acknowledged response %s as received", resp.MRID)
}
