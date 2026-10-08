package main

// DER controls for the aggregator's managed devices
// (GRIDAPPSD/ieee-2030_5-client-go#74). Each managed device runs its own
// session: FSA list, DERProgram walk, DefaultDERControl, a DERControlList
// poll and an event state machine, all read as that device (the target on
// the request context). A control's setpoint is that device's own target,
// so one control read by two devices gives each the full value.
//
// controls.response decides what a session does:
//
//	follow  the device's output follows the active control, and Responses
//	        are posted
//	ack     Responses are posted; the output is not changed
//	none    no session runs: no reads, no Responses, no change of output

import (
	"context"
	"log"
	mathrand "math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// defaultControlPoll is the poll interval when neither controls.poll_s nor
// the server's pollRate gives one.
const defaultControlPoll = 900 * time.Second

// controlSession is one managed device's control state, shared between its
// session goroutine (which writes it) and the fleet tick (which reads it).
type controlSession struct {
	mode       string
	pollS      int
	sm         *inverter.StateMachine
	defaultCtl atomic.Pointer[sep2.DefaultDERControl]

	// interval, when set, replaces the poll interval; a test seam.
	interval time.Duration

	// onExit, when set, runs as the session goroutine's last act before it
	// is counted done; a test seam for observing that WaitControls waits.
	onExit func()
}

func newControlSession(c *simconfig.Controls) *controlSession {
	cs := &controlSession{mode: simconfig.DefaultResponse, sm: inverter.NewStateMachine()}
	if c != nil {
		if c.Response != "" {
			cs.mode = c.Response
		}
		cs.pollS = c.PollS
	}
	return cs
}

func (cs *controlSession) follows() bool { return cs.mode == "follow" }

// base is the control base the device's output follows: the active event's,
// else the program default's, else nil. An ack session never changes output.
func (cs *controlSession) base() *sep2.DERControlBase {
	if !cs.follows() {
		return nil
	}
	return inverter.ActiveControlBase(cs.sm.Current(), cs.defaultCtl.Load())
}

// active reports whether an event is driving the device's output right now.
func (cs *controlSession) active() bool {
	if !cs.follows() {
		return false
	}
	snap := cs.sm.Current()
	return snap.State == inverter.StateEventStarted && snap.ActiveDERControl != nil && snap.ActiveDERControl.DERControlBase != nil
}

// pollInterval is controls.poll_s, else the server's pollRate (60s floor),
// else defaultControlPoll.
func (cs *controlSession) pollInterval(dcap sep2.DeviceCapability) time.Duration {
	switch {
	case cs.interval > 0:
		return cs.interval
	case cs.pollS > 0:
		return time.Duration(cs.pollS) * time.Second
	case dcap.PollRate > 0:
		return pinPollInterval(dcap.PollRate)
	}
	return defaultControlPoll
}

// targetedPoster posts a Response as one managed device: the request
// carries the device as its target, and the post is cancelled with the
// session.
type targetedPoster struct {
	client  responsePoster
	lfdi    string
	session context.Context
}

func (p targetedPoster) PostResponse(ctx context.Context, replyToHref string, resp sep2.DERControlResponse) error {
	ctx, cancel := context.WithCancel(inverter.WithTarget(ctx, p.lfdi))
	defer cancel()
	stop := context.AfterFunc(p.session, cancel)
	defer stop()
	return p.client.PostResponse(ctx, replyToHref, resp)
}

// controlsEnv is what a session needs of the aggregator process.
type controlsEnv struct {
	client *inverter.SEP2Client
	dcap   sep2.DeviceCapability
	cfg    inverter.SimConfig
}

// StartControls starts one control session goroutine per managed device
// whose controls.response is not "none". Every goroutine ends with ctx;
// WaitControls waits for them. A nil fleet starts nothing.
func (f *managedFleet) StartControls(ctx context.Context, env controlsEnv) {
	if f == nil {
		return
	}
	for _, md := range f.devices {
		if md.ctl == nil || md.ctl.mode == "none" {
			continue
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			if md.ctl.onExit != nil {
				defer md.ctl.onExit()
			}
			md.runControls(ctx, env)
		}()
	}
}

// WaitControls blocks until every control session goroutine has returned.
func (f *managedFleet) WaitControls() {
	if f != nil {
		f.wg.Wait()
	}
}

// runControls is one device's session. Discovering the device's programs is
// retried with backoff until it succeeds, so a server that is down or slow
// at start never leaves a follow device uncontrolled for the life of the
// process; only another device's session is unaffected by it either way.
func (md *managedDevice) runControls(ctx context.Context, env controlsEnv) {
	client := env.client
	tctx := inverter.WithTarget(ctx, md.lfdi)
	interval := md.ctl.pollInterval(env.dcap)

	prog, ok := md.discoverProgram(ctx, tctx, env, interval)
	if !ok {
		return
	}

	poster := targetedPoster{client: client, lfdi: md.lfdi, session: ctx}
	md.ctl.sm.AddTransitionHook(responsePOSTHook(poster, md.lfdi, client.Now))

	sched := inverter.NewScheduler(client.Now, mathrand.New(mathrand.NewPCG(uint64(time.Now().UnixNano()), 0xCAFEBABE)))
	cache := inverter.NewDERControlCache()
	href := prog.DERControlListLink.Href
	defaultHref := ""
	if prog.DefaultDERControlLink != nil {
		defaultHref = prog.DefaultDERControlLink.Href
	}
	prev := cache.Snapshot()
	step := func() {
		if defaultHref != "" {
			ddc, newHref, err := client.GetDefaultDERControl(tctx, defaultHref)
			switch {
			case err != nil:
				// The last default read, if any, keeps driving the device.
				if ctx.Err() == nil {
					log.Printf("managed device %s: GET DefaultDERControl failed (keeping the last default, if any): %v", md.name, err)
				}
			default:
				if newHref != "" {
					defaultHref = newHref
				}
				c := ddc.Copy()
				md.ctl.defaultCtl.Store(&c)
			}
		}
		// The state machine ticks every cycle, so an event ends on time
		// even while the control list cannot be read.
		var added, cancelled []sep2.DERControl
		list, newHref, err := client.GetDERControlList(tctx, href)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				log.Printf("managed device %s: DERControlList poll failed (continuing): %v", md.name, err)
			}
		default:
			if newHref != "" {
				href = newHref
			}
			cache.Refresh(list.DERControl)
			curr := cache.Snapshot()
			added, cancelled = diffSnapshots(prev, curr)
			prev = curr
		}
		md.ctl.sm.Tick(client.Now(), added, cancelled, sched)
	}

	log.Printf("managed device %s: following controls from %s every %s (controls.response %s)", md.name, href, interval, md.ctl.mode)
	step()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			step()
		}
	}
}

// discoverProgram walks the device's FSA list and DERPrograms and returns
// the program to follow. A failed read is logged naming the device and
// retried after mirrorBackoff(interval, failures), forever. A walk that finds
// no program with a DERControlList is logged and repeated every interval,
// since a program may be added later. It returns false only when ctx ends.
func (md *managedDevice) discoverProgram(ctx, tctx context.Context, env controlsEnv, interval time.Duration) (sep2.DERProgram, bool) {
	failures := 0
	for {
		prog, found, what, err := md.walkPrograms(tctx, env)
		var wait time.Duration
		switch {
		case err == nil && found:
			return prog, true
		case err == nil:
			wait = interval
			log.Printf("managed device %s (%s): no DERProgram with a DERControlList; looking again in %s", md.name, md.lfdi, wait)
		default:
			if ctx.Err() != nil {
				return sep2.DERProgram{}, false
			}
			failures++
			wait = mirrorBackoff(interval, failures)
			log.Printf("managed device %s (%s): %s failed, attempt %d, retrying in %s: %v", md.name, md.lfdi, what, failures, wait, err)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return sep2.DERProgram{}, false
		case <-timer.C:
		}
	}
}

// walkPrograms makes one FSA list and DERProgram walk as the device. what
// names the failed phase.
func (md *managedDevice) walkPrograms(tctx context.Context, env controlsEnv) (prog sep2.DERProgram, found bool, what string, err error) {
	fsaList, err := runPhase2cFSAList(tctx, env.client, md.edev, env.cfg, env.dcap)
	if err != nil {
		return prog, false, "FSA list", err
	}
	programs := map[string]sep2.DERProgram{}
	if len(fsaList.FunctionSetAssignments) > 0 {
		if programs, err = runPhase2cDERProgramWalkOpts(tctx, env.client, fsaList, env.cfg, env.dcap, true); err != nil {
			return prog, false, "DERProgram walk", err
		}
	}
	// A program with no DERControlList has nothing to follow, so it never
	// outranks one that has.
	for mrid, p := range programs {
		if p.DERControlListLink == nil {
			delete(programs, mrid)
		}
	}
	prog, found = inverter.SelectHighestPriority(programs)
	return prog, found, "", nil
}
