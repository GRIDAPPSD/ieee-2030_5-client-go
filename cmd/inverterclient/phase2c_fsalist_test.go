package main

// Backfill of the 5 deferred Phase 2c FSAList integration cases from IEEE-072
// PR #95. IEEE-075 extracted the Phase 2c FSAList discovery block out of
// main() into runPhase2cFSAList (cmd/inverterclient/phase2c_fsalist.go) so
// these cases — previously blocked by inline-in-main + log.Fatalf — can be
// driven directly. Same structural template as IEEE-074's Phase 2b backfill
// (phase2b_test.go).
//
// Reuses the main-package test bedrock established by IEEE-073:
//   - derWalkTestEnv / newDERWalkTestEnv      (TLS fixture + device cert)
//   - startDERWalkListener                    (gotls HTTPS server)
//   - newDERWalkClient                        (production SEP2Client)
//   - writeSepXML                             (sep+xml encoder)
// All declared in derprogram_phase_test.go and shared across this package's
// test files. captureLog is shared with phase2b_test.go (also declared there;
// avoiding the duplication is HARD RULE 2 territory but a `var captureLog`
// across two files would invent more complexity than it removes — the symbol
// already exists at package scope).
//
// Cases shipped (from IEEE-072 PR #95 deferred block + the IEEE-075 backlog
// spec):
//
//   1. TestRunPhase2cFSAList_HappyPathReturnsList
//        IEEE-072 case (5th deferred, structural baseline): non-empty
//        FSAList GET → return list immediately, no idle, no fatal. Verifies
//        the happy path that the extracted function preserves the original
//        cache-and-break behavior.
//   2. TestRunPhase2cFSAList_EmptyListIdleThenAppears
//        IEEE-072 #5: empty list under --csip strict idles on
//        phase2cFSAListPollInterval (shrunk to 10ms), then a non-empty list
//        appears on the next GET and the function returns it.
//   3. TestRunPhase2cFSAList_EmptyListAllowUnregisteredProceeds
//        IEEE-072 #6: empty list under cfg.AllowUnregistered → log + return
//        empty list, no idle, no fatal.
//   4. TestRunPhase2cFSAList_MissingLinkCSIPStrictReturnsFatal
//        IEEE-072 #7: edev.FunctionSetAssignmentsListLink == nil under
//        --csip strict → *fsaListFatal whose Error() contains "CSIP V1.2
//        CORE-012 step 1". No GETs.
//   5. TestRunPhase2cFSAList_MissingLinkAllowUnregisteredBypasses
//        IEEE-072 #8: edev.FunctionSetAssignmentsListLink == nil under
//        cfg.AllowUnregistered → log + return empty list, nil error. No GETs.
//   6. TestRunPhase2cFSAList_CtxCancelDuringEmptyListIdle
//        IEEE-072 #9: server keeps returning an empty list; ctx-cancel during
//        the idle loop → return ctx.Err() (context.Canceled in chain). Race-
//        clean.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// shrinkPhase2cFSAListPoll compresses the package-level poll floor / default
// for the duration of the test so idle-loop tests run at sub-second cadence
// without faking time. Mirrors shrinkPhase2bPoll (IEEE-074) and
// SetMinTimeSyncPollRateForTesting (IEEE-070).
//
// Tests that shrink the floor MUST NOT use t.Parallel(): the vars are
// process-global state. The same constraint applies to captureLog (declared
// in phase2b_test.go), so all phase2c_fsalist_test.go tests run serially.
// That's the cost of testing log-string contracts and package-level cadence
// at once.
func shrinkPhase2cFSAListPoll(t *testing.T, floor, def time.Duration) {
	t.Helper()
	prevMin := phase2cFSAListPollMin
	prevDef := phase2cFSAListPollDefault
	phase2cFSAListPollMin = floor
	phase2cFSAListPollDefault = def
	t.Cleanup(func() {
		phase2cFSAListPollMin = prevMin
		phase2cFSAListPollDefault = prevDef
	})
}

// phase2cDefaultDcap returns a DeviceCapability with PollRate=0 so the
// empty-list idle uses phase2cFSAListPollDefault, which tests shrink to a
// tight value via shrinkPhase2cFSAListPoll. Non-zero PollRate values would
// bypass the default branch and produce slow tests at the 1+ second cadence
// the raw seconds-per-tick conversion implies. Mirrors phase2bDefaultDcap.
func phase2cDefaultDcap() sep2.DeviceCapability {
	return sep2.DeviceCapability{PollRate: 0}
}

// fsaListFixturePath is the canonical FSAList href used by these tests.
// Matches the convention in internal/inverter/fsalist_test.go.
const fsaListFixturePath = "/edev/1/fsa"

// buildPhase2cFSAList emits a FunctionSetAssignmentsList with n entries, each
// with a distinct mRID. Used by the happy-path and idle-then-appears tests.
// Named with the phase2c prefix to avoid collision with derprogram_phase_test.go's
// buildFSAList (which takes []fsaFixture and is wired into the walkDERProgramTree
// tests). Keeping a small purpose-specific builder here is simpler than reusing
// the fixture-slice form for these tests' single-mRID needs.
func buildPhase2cFSAList(n int) sep2.FunctionSetAssignmentsList {
	fsas := make([]sep2.FunctionSetAssignments, 0, n)
	for i := 0; i < n; i++ {
		fsas = append(fsas, sep2.FunctionSetAssignments{
			MRID: "FSA-" + string(rune('A'+i)),
		})
	}
	return sep2.FunctionSetAssignmentsList{
		ListResource: sep2.ListResource{
			All:     uint32(n),
			Results: uint32(n),
		},
		FunctionSetAssignments: fsas,
	}
}

// TestRunPhase2cFSAList_HappyPathReturnsList — happy-path baseline.
//
// FSAList GET returns 2 entries. runPhase2cFSAList must return the list and
// nil error after exactly one GET, no idle, no fatal. Verifies the extracted
// function preserves the cache-and-break shape of the previous inline form.
func TestRunPhase2cFSAList_HappyPathReturnsList(t *testing.T) {
	buf := captureLog(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(fsaListFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		list := buildPhase2cFSAList(2)
		writeSepXML(t, w, &list)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:                           client.LFDI(),
		FunctionSetAssignmentsListLink: &sep2.ListLink{Href: fsaListFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true}
	got, err := runPhase2cFSAList(ctx, client, edev, cfg, phase2cDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cFSAList: %v", err)
	}
	if len(got.FunctionSetAssignments) != 2 {
		t.Errorf("returned FSAList has %d entries, want 2", len(got.FunctionSetAssignments))
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("FSAList GET hits = %d, want exactly 1", n)
	}
	if !strings.Contains(buf.String(), "=== Phase 2c: FSAList Discovery ===") {
		t.Errorf("log %q missing FSAList Discovery banner", buf.String())
	}
	if !strings.Contains(buf.String(), "FSAList: 2 entries") {
		t.Errorf("log %q missing entry-count line", buf.String())
	}
}

// TestRunPhase2cFSAList_EmptyListIdleThenAppears — IEEE-072 #5.
//
// CSIP-strict + first GET returns empty FSAList. runPhase2cFSAList idles on
// phase2cFSAListPollInterval (shrunk to 10ms), then a second GET returns a
// non-empty list and the function returns it. Tests both the idle-on-empty
// path AND that the loop actually retries on PollRate.
func TestRunPhase2cFSAList_EmptyListIdleThenAppears(t *testing.T) {
	buf := captureLog(t)
	shrinkPhase2cFSAListPoll(t, 10*time.Millisecond, 10*time.Millisecond)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(fsaListFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			empty := sep2.FunctionSetAssignmentsList{}
			writeSepXML(t, w, &empty)
			return
		}
		list := buildPhase2cFSAList(1)
		writeSepXML(t, w, &list)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:                           client.LFDI(),
		FunctionSetAssignmentsListLink: &sep2.ListLink{Href: fsaListFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true}
	got, err := runPhase2cFSAList(ctx, client, edev, cfg, phase2cDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cFSAList: %v", err)
	}
	if len(got.FunctionSetAssignments) != 1 {
		t.Errorf("returned FSAList has %d entries, want 1", len(got.FunctionSetAssignments))
	}
	if n := hits.Load(); n < 2 {
		t.Errorf("FSAList GET hits = %d, want at least 2 (idle then non-empty)", n)
	}
	if !strings.Contains(buf.String(), "FSAList empty; re-polling every") {
		t.Errorf("log %q missing empty-idle marker", buf.String())
	}
	if !strings.Contains(buf.String(), "CSIP V1.2 CORE-012") {
		t.Errorf("log %q missing CORE-012 citation in idle path", buf.String())
	}
	if !strings.Contains(buf.String(), "FSAList: 1 entries") {
		t.Errorf("log %q missing entry-count line after list appears", buf.String())
	}
}

// TestRunPhase2cFSAList_EmptyListAllowUnregisteredProceeds — IEEE-072 #6.
//
// cfg.AllowUnregistered=true + GET returns empty list. runPhase2cFSAList
// must log the proceed marker, return the empty list, nil error. No idle,
// no fatal, no retry. Symmetric to the missing-link bypass for the
// empty-list case.
func TestRunPhase2cFSAList_EmptyListAllowUnregisteredProceeds(t *testing.T) {
	buf := captureLog(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(fsaListFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		empty := sep2.FunctionSetAssignmentsList{}
		writeSepXML(t, w, &empty)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:                           client.LFDI(),
		FunctionSetAssignmentsListLink: &sep2.ListLink{Href: fsaListFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, AllowUnregistered: true}
	got, err := runPhase2cFSAList(ctx, client, edev, cfg, phase2cDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cFSAList: %v", err)
	}
	if len(got.FunctionSetAssignments) != 0 {
		t.Errorf("returned FSAList has %d entries, want 0 (proceed-with-empty path)", len(got.FunctionSetAssignments))
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("FSAList GET hits = %d, want exactly 1 (no idle)", n)
	}
	if !strings.Contains(buf.String(), "FSAList empty; proceeding") {
		t.Errorf("log %q missing empty-proceed marker", buf.String())
	}
}

// TestRunPhase2cFSAList_MissingLinkCSIPStrictReturnsFatal — IEEE-072 #7.
//
// edev.FunctionSetAssignmentsListLink == nil under --csip strict.
// runPhase2cFSAList must return *fsaListFatal whose Error() contains literal
// "CSIP V1.2 CORE-012 step 1". No GETs (the listener exists but should never
// be hit). No log.Fatalf, no panic.
func TestRunPhase2cFSAList_MissingLinkCSIPStrictReturnsFatal(t *testing.T) {
	_ = captureLog(t)

	var totalHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		totalHits.Add(1)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{LFDI: client.LFDI()} // no FunctionSetAssignmentsListLink

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true}
	_, err := runPhase2cFSAList(ctx, client, edev, cfg, phase2cDefaultDcap())
	if err == nil {
		t.Fatal("runPhase2cFSAList with missing link in CSIP strict returned nil error; want *fsaListFatal")
	}
	var fe *fsaListFatal
	if !errors.As(err, &fe) {
		t.Fatalf("err = %T %q; want *fsaListFatal", err, err)
	}
	if !strings.Contains(fe.Error(), "CSIP V1.2 CORE-012 step 1") {
		t.Errorf("fatal reason %q missing %q", fe.Error(), "CSIP V1.2 CORE-012 step 1")
	}
	if !strings.Contains(fe.Error(), "FunctionSetAssignmentsListLink") {
		t.Errorf("fatal reason %q missing %q", fe.Error(), "FunctionSetAssignmentsListLink")
	}
	if n := totalHits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0 (missing-link fatal must not touch the network)", n)
	}
}

// TestRunPhase2cFSAList_MissingLinkAllowUnregisteredBypasses — IEEE-072 #8.
//
// edev.FunctionSetAssignmentsListLink == nil + cfg.AllowUnregistered=true:
// log the bypass and return an empty FSAList with nil error. No GETs. This
// is the dev/test escape hatch and the symmetric counterpart to IEEE-072 #7.
func TestRunPhase2cFSAList_MissingLinkAllowUnregisteredBypasses(t *testing.T) {
	buf := captureLog(t)

	var totalHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		totalHits.Add(1)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{LFDI: client.LFDI()} // no FunctionSetAssignmentsListLink

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, AllowUnregistered: true}
	got, err := runPhase2cFSAList(ctx, client, edev, cfg, phase2cDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2cFSAList: %v", err)
	}
	if len(got.FunctionSetAssignments) != 0 {
		t.Errorf("returned FSAList has %d entries, want 0 (bypass must not synthesize entries)", len(got.FunctionSetAssignments))
	}
	if n := totalHits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0 (bypass must not touch the network)", n)
	}
	if !strings.Contains(buf.String(), "skipping Phase 2c") {
		t.Errorf("log %q missing skip marker", buf.String())
	}
}

// TestRunPhase2cFSAList_CtxCancelDuringEmptyListIdle — IEEE-072 #9.
//
// Server keeps returning an empty FSAList (never provisions). runPhase2cFSAList
// is parked in the empty-list idle loop on a 10ms cadence. Cancelling ctx must
// unwind the function promptly. Two acceptable outcomes (mirror IEEE-074's
// ctx-cancel case):
//
//   - Cancel fires while the loop is blocked in the idle select →
//     ctx.Err() returned directly. Cleanest path.
//   - Cancel fires while a GetFSAList is in flight → SEP2Client wraps the
//     ctx-cancel error from net/http; runPhase2cFSAList wraps THAT in
//     *fsaListFatal (production behavior calls log.Fatalf unconditionally on
//     GET errors). errors.Is(err, context.Canceled) is still true through
//     the chain.
//
// What matters for the deferred contract is "ctx-cancel produces a prompt
// exit, the cancel reason is in the error chain, no race-detector hits".
// Both forms satisfy that. Tight 2s bound ensures we don't accidentally wait
// for the next time.After tick or, worse, the production 60s floor.
func TestRunPhase2cFSAList_CtxCancelDuringEmptyListIdle(t *testing.T) {
	_ = captureLog(t)
	shrinkPhase2cFSAListPoll(t, 10*time.Millisecond, 10*time.Millisecond)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(fsaListFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		empty := sep2.FunctionSetAssignmentsList{}
		writeSepXML(t, w, &empty)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:                           client.LFDI(),
		FunctionSetAssignmentsListLink: &sep2.ListLink{Href: fsaListFixturePath},
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		cfg := inverter.SimConfig{CSIP: true}
		_, err := runPhase2cFSAList(ctx, client, edev, cfg, phase2cDefaultDcap())
		errCh <- err
	}()

	// Let the first GET land + at least one idle-loop print so we know we're
	// at least once around the loop. 100ms ≫ 10ms cadence so the loop has
	// cycled many times.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runPhase2cFSAList returned nil error after ctx cancel; want context.Canceled in the chain")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; context.Canceled must be in the chain", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runPhase2cFSAList did not return within 2s of ctx cancel; idle loop is not honoring ctx.Done()")
	}
	if n := hits.Load(); n < 1 {
		t.Errorf("FSAList GET hits = %d, want at least 1 (loop must enter the empty-idle path)", n)
	}
}
