package main

// Backfill of the 8 deferred Phase 2b integration cases from IEEE-071 PR #91.
// IEEE-074 extracted Phase 2b out of main() into runPhase2bRegistration so
// these cases : previously blocked by inline-in-main + log.Fatalf : can be
// driven directly.
//
// Reuses the main-package test bedrock established by IEEE-073:
//   - derWalkTestEnv / newDERWalkTestEnv      (TLS fixture + device cert)
//   - startDERWalkListener                    (gotls HTTPS server)
//   - newDERWalkClient                        (production SEP2Client)
//   - writeSepXML                             (sep+xml encoder)
// All declared in derprogram_phase_test.go and shared across this package's
// test files. Renaming them here would invent test-scaffolding duplication
// Pike's HARD RULE 2 forbids; the existing helpers are perfectly serviceable.
//
// Cases shipped (from IEEE-071 PR #91 "Cases deferred (8)" table):
//
//   1. TestRunPhase2b_ExpectedPINZero_SkipsMatchCheck
//        IEEE-033 #1: cfg.ExpectedPIN == 0 → log "skipping match check",
//        no idle, return nil.
//   2. TestRunPhase2b_PINMatchesProceed
//        IEEE-034 #3: cfg.ExpectedPIN == rg.PIN → log "matches expected;
//        proceeding", return nil.
//   3. TestRunPhase2b_GetRegistration500ReturnsFatal
//        IEEE-033 #5: GET Registration 500 → *phase2bFatal containing
//        "GET Registration".
//   4. TestRunPhase2b_NonZeroMismatchReturnsFatal
//        IEEE-034 #2: rg.PIN != 0 && rg.PIN != cfg.ExpectedPIN →
//        *phase2bFatal containing "PIN mismatch" + "CSIP V1.2 BASIC-001
//        step 5" + redacted-PIN regex.
//   5. TestRunPhase2b_ServerPINZeroIdleThenMatch
//        IEEE-034 #1: rg.PIN == 0 idle, then rg.PIN provisions to
//        ExpectedPIN → match + proceed.
//   6. TestRunPhase2b_MissingRegistrationLinkIdleThenAppears
//        IEEE-034 #4: edev.RegistrationLink == nil + CSIP-strict → idle on
//        LookupOwnEndDevice, then RegistrationLink appears, GET fires once,
//        return nil.
//   7. TestRunPhase2b_AllowUnregisteredBypassesMissingLink
//        IEEE-034 #5: edev.RegistrationLink == nil + cfg.AllowUnregistered
//        → log skip, return immediately, zero GETs.
//   8. TestRunPhase2b_CtxCancelDuringServerPINZeroIdle
//        IEEE-033 #8 / IEEE-034 #8: ctx-cancel during the rg.PIN==0 idle
//        loop → return ctx.Err(), clean shutdown under -race.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// captureLogMu serializes captureLog callers so concurrent goroutines
// inside one test don't race on the global-writer swap. The bigger
// constraint : callers MUST NOT be t.Parallel() because any sibling
// parallel test emitting log.Printf lines will land in the captured
// buffer: is documented on captureLog below.
var captureLogMu sync.Mutex

// captureLog redirects the default logger's output to a bytes.Buffer for
// the duration of the test. The pre-test prefix + flags are restored in
// t.Cleanup; callers may call .String() on the returned buffer to assert
// on emitted lines.
//
// IEEE-081 contract: Tests that capture the log MUST NOT use t.Parallel().
// log.Default() is process-global state and any t.Parallel() sibling test
// that emits log.Printf lines will write into the captured buffer AND
// race buf.String(). Go's testing runtime runs serial tests in a single
// goroutine BEFORE resuming queued parallel tests, so serial captureLog
// callers complete with no concurrent log writers. The captureLogMu
// mutex below is a secondary guard against future
// captureLog-from-multiple-goroutines misuse within one test; it is NOT
// sufficient on its own.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	captureLogMu.Lock()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0) // drop timestamps so substring matches are deterministic
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
		captureLogMu.Unlock()
	})
	return &buf
}

// shrinkPhase2bPoll compresses the package-level poll floor/default for the
// duration of the test so idle-loop tests run at sub-second cadence without
// faking time. Mirrors SetMinTimeSyncPollRateForTesting.
//
// Tests that shrink the floor MUST NOT use t.Parallel(): the vars are
// process-global state. The same constraint applies to captureLog above,
// so most phase2b_test.go tests run serially; that's the cost of testing
// log-string contracts and package-level cadence at once.
func shrinkPhase2bPoll(t *testing.T, floor, def time.Duration) {
	t.Helper()
	prevMin := phase2bPollMin
	prevDef := phase2bPollDefault
	phase2bPollMin = floor
	phase2bPollDefault = def
	t.Cleanup(func() {
		phase2bPollMin = prevMin
		phase2bPollDefault = prevDef
	})
}

// phase2bDefaultDcap returns a DeviceCapability with PollRate=0 so the
// missing-link idle uses phase2bPollDefault, which tests shrink to a tight
// value via shrinkPhase2bPoll. Non-zero PollRate values would bypass the
// default branch and produce slow tests at the 1+ second cadence the
// raw seconds-per-tick conversion implies.
func phase2bDefaultDcap() sep2.DeviceCapability {
	return sep2.DeviceCapability{PollRate: 0}
}

// registrationFixturePath is the canonical Registration href used by these
// tests. Mirrors the convention in internal/inverter/registration_test.go.
const registrationFixturePath = "/edev/1/rg"

// edevListFixturePath is the canonical EndDeviceList href used by the
// re-lookup idle tests.
const edevListFixturePath = "/edev"

// buildEdevListWithLFDI emits an EndDeviceList containing a single EndDevice
// with the supplied LFDI and optional RegistrationLink. LookupOwnEndDevice
// matches on LFDI string equality (case-sensitive upper-hex 40-char).
func buildEdevListWithLFDI(lfdi string, regLink *sep2.Link) sep2.EndDeviceList {
	ed := sep2.EndDevice{LFDI: lfdi}
	if regLink != nil {
		ed.RegistrationLink = regLink
	}
	return sep2.EndDeviceList{
		ListResource: sep2.ListResource{
			All:     1,
			Results: 1,
		},
		EndDevice: []sep2.EndDevice{ed},
	}
}

// TestRunPhase2b_ExpectedPINZero_SkipsMatchCheck : IEEE-033 #1.
//
// cfg.ExpectedPIN == 0 means the operator opted out of the match check.
// Phase 2b reads the Registration resource once, logs the redacted
// server-presented PIN, and returns nil. No idle, no fatal.
func TestRunPhase2b_ExpectedPINZero_SkipsMatchCheck(t *testing.T) {
	buf := captureLog(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeSepXML(t, w, &sep2.Registration{PIN: 111115})
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:             client.LFDI(),
		RegistrationLink: &sep2.Link{Href: registrationFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: 0}
	got, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2bRegistration: %v", err)
	}
	if got.LFDI != edev.LFDI {
		t.Errorf("returned edev.LFDI = %q, want %q", got.LFDI, edev.LFDI)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("Registration GET hits = %d, want exactly 1", n)
	}
	if !strings.Contains(buf.String(), "--pin not set; skipping match check") {
		t.Errorf("log %q missing skip marker", buf.String())
	}
	// PIN must be redacted even on the skip path.
	if strings.Contains(buf.String(), "111115") {
		t.Errorf("log %q leaked unredacted PIN 111115", buf.String())
	}
	if !strings.Contains(buf.String(), "***15") {
		t.Errorf("log %q missing redacted PIN ***15", buf.String())
	}
}

// TestRunPhase2b_PINMatchesProceed : IEEE-034 #3.
//
// cfg.ExpectedPIN == rg.PIN: log the CSIP commissioning signal
// ("matches expected; proceeding") and return nil. This exact substring is
// the gate that downstream Phase 2c+ phases grep for; the redactPIN guard
// keeps the cleartext out of the line.
func TestRunPhase2b_PINMatchesProceed(t *testing.T) {
	buf := captureLog(t)

	const pin uint32 = 111115
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeSepXML(t, w, &sep2.Registration{PIN: pin})
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:             client.LFDI(),
		RegistrationLink: &sep2.Link{Href: registrationFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: uint(pin)}
	if _, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap()); err != nil {
		t.Fatalf("runPhase2bRegistration: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("Registration GET hits = %d, want exactly 1", n)
	}
	if !strings.Contains(buf.String(), "matches expected; proceeding") {
		t.Errorf("log %q missing CSIP commissioning signal", buf.String())
	}
	if strings.Contains(buf.String(), "111115") {
		t.Errorf("log %q leaked unredacted PIN", buf.String())
	}
}

// TestRunPhase2b_GetRegistration500ReturnsFatal : IEEE-033 #5.
//
// GET Registration returns 500. runPhase2bRegistration must return
// *phase2bFatal wrapping "GET Registration"; no log.Fatalf, no panic.
func TestRunPhase2b_GetRegistration500ReturnsFatal(t *testing.T) {
	_ = captureLog(t) // swallow chatty log lines

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:             client.LFDI(),
		RegistrationLink: &sep2.Link{Href: registrationFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: 111115}
	_, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap())
	if err == nil {
		t.Fatal("runPhase2bRegistration on 500 returned nil error; want *phase2bFatal")
	}
	var fe *phase2bFatal
	if !errors.As(err, &fe) {
		t.Fatalf("err = %T %q; want *phase2bFatal", err, err)
	}
	if !strings.Contains(fe.Error(), "GET Registration") {
		t.Errorf("fatal reason %q missing %q", fe.Error(), "GET Registration")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("Registration GET hits = %d, want exactly 1", n)
	}
}

// TestRunPhase2b_NonZeroMismatchReturnsFatal : IEEE-034 #2.
//
// rg.PIN != 0 && rg.PIN != cfg.ExpectedPIN: wrong-device/server pair.
// runPhase2bRegistration returns *phase2bFatal whose Error() contains
// "PIN mismatch", "CSIP V1.2 BASIC-001 step 5", and BOTH PINs in the
// redacted ***NN form. The cleartext PINs must NOT appear anywhere in
// the message (IEEE 2030.5 §8.2.1 / IEEE-034 case 7 invariant).
func TestRunPhase2b_NonZeroMismatchReturnsFatal(t *testing.T) {
	_ = captureLog(t)

	const serverPIN uint32 = 222222
	const expectedPIN uint = 111115

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeSepXML(t, w, &sep2.Registration{PIN: serverPIN})
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:             client.LFDI(),
		RegistrationLink: &sep2.Link{Href: registrationFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: expectedPIN}
	_, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap())
	if err == nil {
		t.Fatal("runPhase2bRegistration on mismatch returned nil error; want *phase2bFatal")
	}
	var fe *phase2bFatal
	if !errors.As(err, &fe) {
		t.Fatalf("err = %T %q; want *phase2bFatal", err, err)
	}
	msg := fe.Error()
	if !strings.Contains(msg, "PIN mismatch") {
		t.Errorf("fatal %q missing %q", msg, "PIN mismatch")
	}
	if !strings.Contains(msg, "CSIP V1.2 BASIC-001 step 5") {
		t.Errorf("fatal %q missing CSIP step reference", msg)
	}
	// Both PINs must appear in redacted form.
	if !strings.Contains(msg, "***22") {
		t.Errorf("fatal %q missing redacted server PIN ***22", msg)
	}
	if !strings.Contains(msg, "***15") {
		t.Errorf("fatal %q missing redacted expected PIN ***15", msg)
	}
	// Strict guarantee: no cleartext PINs.
	if strings.Contains(msg, "222222") {
		t.Errorf("fatal %q leaked unredacted server PIN 222222", msg)
	}
	if strings.Contains(msg, "111115") {
		t.Errorf("fatal %q leaked unredacted expected PIN 111115", msg)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("Registration GET hits = %d, want exactly 1", n)
	}
}

// TestRunPhase2b_ServerPINZeroIdleThenMatch : IEEE-034 #1.
//
// Server hasn't provisioned us yet: first Registration GET returns PIN=0.
// runPhase2bRegistration idles on registration.PollRate (shrunk to 10ms),
// then a second GET returns the expected PIN and the function returns nil.
// Tests both the idle-on-zero path and that the loop actually retries.
func TestRunPhase2b_ServerPINZeroIdleThenMatch(t *testing.T) {
	buf := captureLog(t)
	shrinkPhase2bPoll(t, 10*time.Millisecond, 10*time.Millisecond)

	const expectedPIN uint = 111115
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			// First call: server hasn't provisioned us. PollRate=0
			// routes through phase2bPollDefault (shrunk to 10ms).
			writeSepXML(t, w, &sep2.Registration{PIN: 0, PollRate: 0})
			return
		}
		writeSepXML(t, w, &sep2.Registration{PIN: uint32(expectedPIN)})
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:             client.LFDI(),
		RegistrationLink: &sep2.Link{Href: registrationFixturePath},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: expectedPIN}
	if _, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap()); err != nil {
		t.Fatalf("runPhase2bRegistration: %v", err)
	}
	if n := hits.Load(); n < 2 {
		t.Errorf("Registration GET hits = %d, want at least 2 (idle then match)", n)
	}
	if !strings.Contains(buf.String(), "server PIN not yet provisioned") {
		t.Errorf("log %q missing PIN-zero idle marker", buf.String())
	}
	if !strings.Contains(buf.String(), "matches expected; proceeding") {
		t.Errorf("log %q missing CSIP commissioning signal", buf.String())
	}
}

// TestRunPhase2b_MissingRegistrationLinkIdleThenAppears : IEEE-034 #4.
//
// CSIP-strict + edev.RegistrationLink == nil: idle re-fetching the
// EndDevice via LookupOwnEndDevice on dcap.PollRate. First lookup returns
// edev WITHOUT RegistrationLink; second lookup returns edev WITH
// RegistrationLink; PIN GET then matches and returns nil.
func TestRunPhase2b_MissingRegistrationLinkIdleThenAppears(t *testing.T) {
	buf := captureLog(t)
	shrinkPhase2bPoll(t, 10*time.Millisecond, 10*time.Millisecond)

	const expectedPIN uint = 111115
	var lookupHits atomic.Int32
	var regHits atomic.Int32

	mux := http.NewServeMux()
	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)
	lfdi := client.LFDI()

	mux.HandleFunc(edevListFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		n := lookupHits.Add(1)
		var regLink *sep2.Link
		if n >= 2 {
			regLink = &sep2.Link{Href: registrationFixturePath}
		}
		list := buildEdevListWithLFDI(lfdi, regLink)
		writeSepXML(t, w, &list)
	})
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		regHits.Add(1)
		writeSepXML(t, w, &sep2.Registration{PIN: uint32(expectedPIN)})
	})

	// Start with a stale edev: RegistrationLink absent. main() arrived here
	// via the original LookupOwnEndDevice; runPhase2bRegistration owns the
	// re-poll loop.
	edev := sep2.EndDevice{LFDI: lfdi}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: expectedPIN}
	got, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2bRegistration: %v", err)
	}
	if got.RegistrationLink == nil {
		t.Fatalf("returned edev still has nil RegistrationLink; re-lookup loop did not advance state")
	}
	if got.RegistrationLink.Href != registrationFixturePath {
		t.Errorf("returned edev.RegistrationLink.Href = %q, want %q", got.RegistrationLink.Href, registrationFixturePath)
	}
	if n := lookupHits.Load(); n < 2 {
		t.Errorf("EndDeviceList GET hits = %d, want at least 2", n)
	}
	if n := regHits.Load(); n != 1 {
		t.Errorf("Registration GET hits = %d, want exactly 1 after link appears", n)
	}
	if !strings.Contains(buf.String(), "Awaiting RegistrationLink") {
		t.Errorf("log %q missing awaiting marker", buf.String())
	}
	if !strings.Contains(buf.String(), "RegistrationLink appeared") {
		t.Errorf("log %q missing appeared marker", buf.String())
	}
}

// TestRunPhase2b_AllowUnregisteredBypassesMissingLink : IEEE-034 #5.
//
// edev.RegistrationLink == nil + cfg.AllowUnregistered=true: log the
// bypass and return nil immediately. No EndDeviceList GET, no Registration
// GET. This is the dev/test escape hatch.
func TestRunPhase2b_AllowUnregisteredBypassesMissingLink(t *testing.T) {
	buf := captureLog(t)

	var totalHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		totalHits.Add(1)
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{LFDI: client.LFDI()} // no RegistrationLink

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := inverter.SimConfig{CSIP: true, AllowUnregistered: true, ExpectedPIN: 111115}
	got, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap())
	if err != nil {
		t.Fatalf("runPhase2bRegistration: %v", err)
	}
	if got.RegistrationLink != nil {
		t.Errorf("returned edev.RegistrationLink = %+v, want nil (bypass must not synthesize a link)", got.RegistrationLink)
	}
	if n := totalHits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0 (bypass must not touch the network)", n)
	}
	if !strings.Contains(buf.String(), "skipping Phase 2b") {
		t.Errorf("log %q missing skip marker", buf.String())
	}
}

// TestRunPhase2b_CtxCancelDuringServerPINZeroIdle : IEEE-033 #8 / IEEE-034 #8.
//
// Server keeps returning rg.PIN=0 (never provisions). runPhase2bRegistration
// is parked in the PIN-zero idle loop on a 10ms cadence. Cancelling ctx
// must unwind the function promptly. Two acceptable outcomes:
//
//   - Cancel fires while the loop is blocked in the idle select →
//     ctx.Err() returned directly. Cleanest path.
//   - Cancel fires while a Registration GET is in flight → SEP2Client
//     wraps the ctx-cancel error from net/http; runPhase2bRegistration
//     wraps THAT in *phase2bFatal (because the production behavior frozen
//     at IEEE-034 calls log.Fatalf unconditionally on GetRegistration
//     errors, including ctx-cancel). errors.Is(err, context.Canceled) is
//     still true through the chain.
//
// What matters for the deferred contract is "ctx-cancel produces a prompt
// exit, the cancel reason is in the error chain, no race-detector hits".
// Both forms satisfy that. Tight 2s bound ensures we don't accidentally
// wait for the next time.After tick or, worse, the production 60s floor.
func TestRunPhase2b_CtxCancelDuringServerPINZeroIdle(t *testing.T) {
	_ = captureLog(t)
	shrinkPhase2bPoll(t, 10*time.Millisecond, 10*time.Millisecond)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationFixturePath, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeSepXML(t, w, &sep2.Registration{PIN: 0, PollRate: 0})
	})

	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	edev := sep2.EndDevice{
		LFDI:             client.LFDI(),
		RegistrationLink: &sep2.Link{Href: registrationFixturePath},
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		cfg := inverter.SimConfig{CSIP: true, ExpectedPIN: 111115}
		_, err := runPhase2bRegistration(ctx, client, edev, edevListFixturePath, cfg, phase2bDefaultDcap())
		errCh <- err
	}()

	// Let the first GET land + the idle-loop print so we know we're at
	// least once around the loop. 100ms ≫ 10ms cadence so the loop has
	// cycled many times.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runPhase2bRegistration returned nil error after ctx cancel; want context.Canceled in the chain")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; context.Canceled must be in the chain", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runPhase2bRegistration did not return within 2s of ctx cancel; idle loop is not honoring ctx.Done()")
	}
	if n := hits.Load(); n < 1 {
		t.Errorf("Registration GET hits = %d, want at least 1 (loop must enter the idle path)", n)
	}
}
