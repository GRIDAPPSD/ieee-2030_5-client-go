package main

// Backfill of the 2 deferred Phase 2c DERProgram-walk outer-loop integration
// cases (GRIDAPPSD/ieee-2030_5-server-go#97). The Phase 2c DERProgram-walk
// outer for-loop was extracted out of main() into runPhase2cDERProgramWalk
// (cmd/inverterclient/phase2c_derprogram.go, GRIDAPPSD/ieee-2030_5-server-go#100)
// so these cases : previously
// blocked by inline-in-main + log.Fatalf : can be driven directly. Same
// structural template as the Phase 2b backfill (phase2b_test.go) and
// the Phase 2c FSAList backfill (phase2c_fsalist_test.go).
//
// Reuses the main-package test bedrock:
//   - derWalkTestEnv / newDERWalkTestEnv      (TLS fixture + device cert)
//   - startDERWalkListener                    (gotls HTTPS server)
//   - newDERWalkClient                        (production SEP2Client)
//   - writeSepXML                             (sep+xml encoder)
//   - fsaFixture / derProgramFixture          (per-FSA / per-program builders)
//   - mountFSAFixture / buildFSAList          (handler + list builders)
// All declared in derprogram_phase_test.go and shared across this package's
// test files. captureLog is shared with phase2b_test.go / phase2c_fsalist_test.go.
//
// Cases covered:
//
//   1. TestRunPhase2cDERProgramWalk_EmptyAggregateIdleThenAppears
//        All FSAs return empty DERProgramList under --csip
//        strict -> runPhase2cDERProgramWalk idles on
//        phase2cDERProgramPollInterval (shrunk to 10ms). On a later poll, an
//        FSA's DERProgramList is non-empty and the function returns the cache.
//        Verifies BOTH the empty-aggregate idle path AND that the loop
//        actually retries on PollRate.
//   2. TestRunPhase2cDERProgramWalk_PollRateThrottlesEmptyAggregateLoop
//        All FSAs keep returning empty lists; runPhase2cDERProgramWalk
//        is parked in the idle loop on a tight 25ms cadence (test seam). The
//        spacing between successive DERProgramList GETs MUST be at least
//        25ms x tolerance, proving the time.After(pollEvery) gate is honored
//        and not bypassed by a busy loop. ctx-cancel after a fixed window
//        unwinds cleanly. Race-clean.
//
// Three bonus cases are bundled because they're cheap and lift coverage on
// the extracted function (and the *derProgramWalkFatal Error/Unwrap methods,
// matching phase2b.go / phase2c_fsalist.go sibling coverage) past 80% without
// inventing new fixture machinery:
//
//   3. TestRunPhase2cDERProgramWalk_HappyPathReturnsCache
//        Baseline: non-empty DERProgramList GET on a single FSA -> function
//        returns the populated cache after one iteration, no idle, no fatal.
//        Verifies the cache-and-return shape of the previous inline form.
//   4. TestRunPhase2cDERProgramWalk_WalkErrorReturnsFatal
//        Walk error (HTTP 500 on DERProgramList GET) -> *derProgramWalkFatal
//        whose Error() preserves the "walk DERProgram tree: %v" format
//        byte-for-byte and whose Unwrap() returns the wrapped walk error.
//        errors.As + errors.Is chain assertions.
//   5. TestRunPhase2cDERProgramWalk_EmptyAggregateAllowUnregisteredProceeds
//        Symmetric to the FSAList empty-list case for the DERProgram-walk
//        path: empty aggregate under cfg.AllowUnregistered -> log the
//        proceed marker, return the empty cache, nil error. No idle, no
//        retry.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// shrinkPhase2cDERProgramPoll compresses the package-level poll floor /
// default for the duration of the test so idle-loop tests run at sub-second
// cadence without faking time. Mirrors shrinkPhase2cFSAListPoll
// and shrinkPhase2bPoll.
//
// Tests that shrink the floor MUST NOT use t.Parallel(): the vars are
// process-global state. The same constraint applies to captureLog (declared
// in phase2b_test.go), so all phase2c_derprogram_test.go tests run serially.
// That's the cost of testing log-string contracts and package-level cadence
// at once.
func shrinkPhase2cDERProgramPoll(t *testing.T, floor, def time.Duration) {
	t.Helper()
	prevMin := phase2cDERProgramPollMin
	prevDef := phase2cDERProgramPollDefault
	phase2cDERProgramPollMin = floor
	phase2cDERProgramPollDefault = def
	t.Cleanup(func() {
		phase2cDERProgramPollMin = prevMin
		phase2cDERProgramPollDefault = prevDef
	})
}

// phase2cDERProgramDefaultDcap returns a DeviceCapability with PollRate=0 so
// the empty-aggregate idle uses phase2cDERProgramPollDefault, which tests
// shrink to a tight value via shrinkPhase2cDERProgramPoll. Non-zero PollRate
// values would bypass the default branch and produce slow tests at the 1+
// second cadence the raw seconds-per-tick conversion implies. Mirrors
// phase2cDefaultDcap and phase2bDefaultDcap.
func phase2cDERProgramDefaultDcap() sep2.DeviceCapability {
	return sep2.DeviceCapability{PollRate: 0}
}

// TestRunPhase2cDERProgramWalk_HappyPathReturnsCache : baseline.
//
// Single FSA with a non-empty DERProgramList. runPhase2cDERProgramWalk must
// return the populated cache after exactly one walk pass, no idle, no fatal.
// Verifies the extracted function preserves the cache-and-return shape of
// the previous inline form.
func TestRunPhase2cDERProgramWalk_HappyPathReturnsCache(t *testing.T) {
	buf := captureLog(t)

	fsas := []fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
			programs: []derProgramFixture{
				{mRID: "PROG-A1"},
				{mRID: "PROG-A2"},
			},
		},
	}
	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	for _, fsa := range fsas {
		mountFSAFixture(t, mux, fsa, hitsByPath)
	}

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true}
	got, err := runPhase2cDERProgramWalk(ctx, client, buildFSAList(fsas), cfg, phase2cDERProgramDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cDERProgramWalk: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("cache size = %d, want 2", len(got))
	}
	for _, mrid := range []string{"PROG-A1", "PROG-A2"} {
		if _, ok := got[mrid]; !ok {
			t.Errorf("cache missing mRID %q; got keys = %v", mrid, mridSet(got))
		}
	}
	if h := hitsByPath["/edev/1/fsa/0/derp"].Load(); h != 1 {
		t.Errorf("DERProgramList GET hits = %d, want exactly 1", h)
	}
	if !strings.Contains(buf.String(), "=== Phase 2c: DERProgram Tree Walk ===") {
		t.Errorf("log %q missing walk banner", buf.String())
	}
	if !strings.Contains(buf.String(), "cached 2 DERProgram(s) across 1 FSA(s)") {
		t.Errorf("log %q missing cache-summary line", buf.String())
	}
}

// TestRunPhase2cDERProgramWalk_EmptyAggregateIdleThenAppears : case 4.
//
// All FSAs return an empty DERProgramList on the first walk pass; on the
// second pass the FSA returns a non-empty list. runPhase2cDERProgramWalk
// must idle on phase2cDERProgramPollInterval (shrunk to 10ms) between the
// two passes, then return the cache once non-empty. Tests both the
// empty-aggregate idle path AND that the loop actually retries on PollRate.
//
// The test seam swaps the FSA's DERProgramList handler atomically once the
// first walk pass has completed so the test is deterministic: walk 1 sees an
// empty list, idle fires, walk 2 sees the non-empty list.
func TestRunPhase2cDERProgramWalk_EmptyAggregateIdleThenAppears(t *testing.T) {
	buf := captureLog(t)
	shrinkPhase2cDERProgramPoll(t, 10*time.Millisecond, 10*time.Millisecond)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/fsa/0/derp", func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			// First pass: empty list : triggers idle.
			empty := sep2.DERProgramList{}
			writeSepXML(t, w, &empty)
			return
		}
		// Second+ pass: non-empty list : triggers return.
		list := sep2.DERProgramList{
			ListResource: sep2.ListResource{All: 1, Results: 1},
			DERProgram: []sep2.DERProgram{
				{MRID: "PROG-LATE", Primacy: 1},
			},
		}
		writeSepXML(t, w, &list)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	fsaList := buildFSAList([]fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true}
	got, err := runPhase2cDERProgramWalk(ctx, client, fsaList, cfg, phase2cDERProgramDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cDERProgramWalk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("cache size = %d, want 1", len(got))
	}
	if _, ok := got["PROG-LATE"]; !ok {
		t.Errorf("cache missing mRID %q; got keys = %v", "PROG-LATE", mridSet(got))
	}
	if h := hits.Load(); h < 2 {
		t.Errorf("DERProgramList GET hits = %d, want at least 2 (idle then non-empty)", h)
	}
	if !strings.Contains(buf.String(), "No DERPrograms enumerated across any FSA; re-polling every") {
		t.Errorf("log %q missing empty-idle marker", buf.String())
	}
	if !strings.Contains(buf.String(), "CSIP V1.2 CORE-012 step 2") {
		t.Errorf("log %q missing CORE-012 step 2 citation in idle path", buf.String())
	}
	if !strings.Contains(buf.String(), "cached 1 DERProgram(s) across 1 FSA(s)") {
		t.Errorf("log %q missing cache-summary line after list appears", buf.String())
	}
}

// TestRunPhase2cDERProgramWalk_PollRateThrottlesEmptyAggregateLoop : case 6.
//
// All FSAs keep returning empty DERProgramLists. runPhase2cDERProgramWalk is
// parked in the empty-aggregate idle loop on a 25ms cadence (test seam).
// After observing >=2 idle ticks the test cancels ctx and asserts:
//
//   - The function returns ctx.Err() in the chain (errors.Is(err,
//     context.Canceled)).
//   - The interval between successive DERProgramList GETs is at least
//     phase2cDERProgramPollMin x tolerance, proving the time.After(pollEvery)
//     gate is honored and not bypassed by a busy loop.
//   - hits >= 2: we did observe at least two passes.
//
// The pollRate-throttling assertion uses a 0.5x tolerance window mirroring
// the "real-time assertion with a tolerance window
// (>=50% of pollRate between successive GETs)"). Scheduler jitter on
// race-detector runs occasionally compresses individual gaps; a 50% floor on
// the median (or, equivalently, on the per-gap minimum after a small warmup)
// is the documented bar.
//
// Race-clean: ctx-cancel + the per-gap timestamp slice are guarded by an
// atomic.Int32 + mutex pattern.
func TestRunPhase2cDERProgramWalk_PollRateThrottlesEmptyAggregateLoop(t *testing.T) {
	_ = captureLog(t)
	shrinkPhase2cDERProgramPoll(t, 25*time.Millisecond, 25*time.Millisecond)

	var (
		hits    atomic.Int32
		gapsMu  = make(chan struct{}, 1)
		gaps    []time.Duration
		lastHit time.Time
	)
	gapsMu <- struct{}{} // initial token
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/fsa/0/derp", func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		hits.Add(1)
		<-gapsMu
		if !lastHit.IsZero() {
			gaps = append(gaps, now.Sub(lastHit))
		}
		lastHit = now
		gapsMu <- struct{}{}
		empty := sep2.DERProgramList{}
		writeSepXML(t, w, &empty)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	fsaList := buildFSAList([]fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
		},
	})

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		cfg := inverter.SimConfig{CSIP: true}
		_, err := runPhase2cDERProgramWalk(ctx, client, fsaList, cfg, phase2cDERProgramDefaultDcap())
		errCh <- err
	}()

	// Wait for ~4 idle ticks at 25ms apart, then cancel. 150ms gives us
	// plenty of margin even on a slow race-detector run while still bounding
	// the test wall-time well under the 2s safety bound below.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runPhase2cDERProgramWalk returned nil error after ctx cancel; want context.Canceled in the chain")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; context.Canceled must be in the chain", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runPhase2cDERProgramWalk did not return within 2s of ctx cancel; idle loop is not honoring ctx.Done()")
	}

	if h := hits.Load(); h < 2 {
		t.Errorf("DERProgramList GET hits = %d, want at least 2 (loop must enter the idle path at least once)", h)
	}

	// Drain any in-flight handler write that beat ctx-cancel; safe to read
	// gaps now that the goroutine has returned.
	<-gapsMu
	gapsSnap := append([]time.Duration(nil), gaps...)
	gapsMu <- struct{}{}

	// Throttling assertion: the minimum gap between successive GETs must be
	// at least 50% of phase2cDERProgramPollMin. If the time.After gate were
	// bypassed (busy loop) the gaps would collapse to sub-millisecond. 50%
	// tolerates scheduler jitter and the small but non-zero handler
	// turnaround under -race.
	if len(gapsSnap) < 1 {
		t.Skip("only one GET observed before cancel; cannot assert throttling on a single sample")
	}
	const tolerance = time.Duration(float64(25*time.Millisecond) * 0.5)
	for i, g := range gapsSnap {
		if g < tolerance {
			t.Errorf("gap[%d] = %s; want >= %s (pollRate throttling collapsed : time.After gate may have been bypassed)", i, g, tolerance)
		}
	}
}

// TestRunPhase2cDERProgramWalk_WalkErrorReturnsFatal : bonus.
//
// One FSA's DERProgramList GET fails with HTTP 500. walkDERProgramTree wraps
// that as `FSA mRID=%s DERProgramList: %w`. runPhase2cDERProgramWalk wraps
// THAT as `*derProgramWalkFatal{reason: "walk DERProgram tree: <inner>"}`
// matching the prior inline `log.Fatalf("walk DERProgram tree: %v", err)`
// format byte-for-byte. main() unwraps via errors.As and log.Fatalf's the
// reason.
//
// Asserts the *derProgramWalkFatal contract: Error() includes the literal
// "walk DERProgram tree:" prefix and the inner error message; errors.As
// succeeds; the chain preserves the wrapped transport error so callers can
// errors.Is downstream. Exercises the Error() and Unwrap() methods for
// coverage parity with phase2b.go / phase2c_fsalist.go siblings.
func TestRunPhase2cDERProgramWalk_WalkErrorReturnsFatal(t *testing.T) {
	_ = captureLog(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/fsa/0/derp", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	fsaList := buildFSAList([]fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true}
	got, err := runPhase2cDERProgramWalk(ctx, client, fsaList, cfg, phase2cDERProgramDefaultDcap())
	if err == nil {
		t.Fatal("runPhase2cDERProgramWalk with 500-handler returned nil error; want *derProgramWalkFatal")
	}
	if len(got) != 0 {
		t.Errorf("cache size = %d, want 0 on fatal path", len(got))
	}
	var fe *derProgramWalkFatal
	if !errors.As(err, &fe) {
		t.Fatalf("err = %T %q; want *derProgramWalkFatal", err, err)
	}
	if !strings.HasPrefix(fe.Error(), "walk DERProgram tree: ") {
		t.Errorf("fatal reason %q missing %q prefix (byte-for-byte parity with prior inline log.Fatalf)", fe.Error(), "walk DERProgram tree: ")
	}
	if fe.Unwrap() == nil {
		t.Errorf("fatal Unwrap() returned nil; want the wrapped walkDERProgramTree error so errors.Is downstream still works")
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("DERProgramList GET hits = %d, want exactly 1 (fatal path must not retry)", h)
	}
}

// TestRunPhase2cDERProgramWalk_EmptyAggregateAllowUnregisteredProceeds : bonus.
//
// cfg.AllowUnregistered=true + all FSAs return empty DERProgramLists.
// runPhase2cDERProgramWalk must log the proceed marker, return the empty
// cache, nil error. No idle, no retry. Symmetric to the
// empty-list-proceeds path at the FSAList layer.
func TestRunPhase2cDERProgramWalk_EmptyAggregateAllowUnregisteredProceeds(t *testing.T) {
	buf := captureLog(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/fsa/0/derp", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		empty := sep2.DERProgramList{}
		writeSepXML(t, w, &empty)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	fsaList := buildFSAList([]fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, AllowUnregistered: true}
	got, err := runPhase2cDERProgramWalk(ctx, client, fsaList, cfg, phase2cDERProgramDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cDERProgramWalk: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cache size = %d, want 0 (proceed-with-empty path)", len(got))
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("DERProgramList GET hits = %d, want exactly 1 (no idle)", h)
	}
	if !strings.Contains(buf.String(), "No DERPrograms enumerated; proceeding with empty cache") {
		t.Errorf("log %q missing empty-proceed marker", buf.String())
	}
}

// mridSet collects the mRID keys of a DERProgram cache into a sorted-ish
// slice purely for failure messages. Order is not asserted; the test cares
// about set-membership.
func mridSet(m map[string]sep2.DERProgram) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
