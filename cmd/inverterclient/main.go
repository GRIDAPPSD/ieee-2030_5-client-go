package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter/device"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter/dispatch"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// defaultServerURL matches the Makefile's `run-inverter` SERVER_URL default
// so invoking the binary directly behaves the same as `make run-inverter`.
// See IEEE-026.
const defaultServerURL = "https://localhost:8443"

// HMI server timeout defaults. The HMI is a local dashboard, but zero
// timeouts still allow Slowloris exhaustion against an unprotected port.
const (
	hmiReadHeaderTimeout = 10 * time.Second
	hmiReadTimeout       = 30 * time.Second
	hmiWriteTimeout      = 30 * time.Second
	hmiIdleTimeout       = 120 * time.Second
)

// newHMIServer constructs the HMI http.Server with bounded timeouts.
// port is the TCP port to bind. The handler is set by the caller after HMI
// initialisation (see hmi.Handler()).
func newHMIServer(port int) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		ReadHeaderTimeout: hmiReadHeaderTimeout,
		ReadTimeout:       hmiReadTimeout,
		WriteTimeout:      hmiWriteTimeout,
		IdleTimeout:       hmiIdleTimeout,
	}
}

// redactPIN masks all but the last 2 digits of a PIN. Per IEEE 2030.5 §8.2.1
// the last digit is a check digit; the trailing 2 digits are conventional
// in security UIs for redaction that preserves the check-digit signature
// without exposing the full secret. See IEEE-034.
func redactPIN(p uint) string {
	if p < 100 {
		return fmt.Sprintf("***%02d", p)
	}
	return fmt.Sprintf("***%02d", p%100)
}

// pinPollInterval converts an IEEE 2030.5 pollRate (seconds, uint32) to a
// time.Duration with the project's standard floor (60s) and default-on-zero
// (30min) policy. Used by the Phase 2b idle loops (server-PIN-not-provisioned
// and missing-RegistrationLink-in-CSIP-strict) and the Phase 2c FSAList /
// DERProgram-walk idle loops (IEEE-035 / IEEE-036). Pattern mirrors IEEE-031.
func pinPollInterval(rate uint32) time.Duration {
	d := time.Duration(rate) * time.Second
	if d <= 0 {
		d = 30 * time.Minute
	}
	if d < 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// diffSnapshots computes added / cancelled buckets between two cache
// snapshots for IEEE-040's state-machine tick. Added = mRIDs in curr not in
// prev. Cancelled = mRIDs in prev not in curr OR mRIDs whose
// EventStatus.CurrentStatus flipped to 2 (EventStatusCancelled) in curr.
//
// This is intentionally a TICK-LEVEL diff (prev-vs-curr) rather than the
// CACHE-LEVEL diff DERControlCache.Diff produces (which compares a polled
// list against the live cache state). The state machine needs per-tick
// deltas so it can drive transitions exactly once per change.
func diffSnapshots(prev, curr map[string]sep2.DERControl) (added, cancelled []sep2.DERControl) {
	for mrid, c := range curr {
		// Cancelled-in-curr regardless of prev : surfaces newly observed
		// cancellations whether the event was previously seen or not.
		if c.EventStatus != nil && c.EventStatus.CurrentStatus == sep2.EventStatusCancelled {
			cancelled = append(cancelled, c.Copy())
			continue
		}
		if _, existed := prev[mrid]; !existed {
			added = append(added, c.Copy())
		}
	}
	// mRIDs in prev but absent from curr : server removed them from the
	// list. Treat as a benign cancellation so the state machine can revert
	// the active event if it matches.
	for mrid, p := range prev {
		if _, stillThere := curr[mrid]; !stillThere {
			cancelled = append(cancelled, p.Copy())
		}
	}
	return added, cancelled
}

// walkDERProgramTree implements the IEEE-036 Phase 2c (continued) walk: for
// each FSA whose DERProgramListLink is non-nil, GET the DERProgramList and
// fetch each DERProgram's DefaultDERControl / DERControlList / DERCurveList
// subtrees (when those links are non-nil). Programs are cached into `out`
// keyed by mRID; if the same DERProgram is reachable from multiple FSAs the
// later GET wins (DERProgram identity is its mRID per IEEE 2030.5 §10.1.3).
//
// Per-FSA missing-DERProgramListLink and per-DERProgram missing-subtree-link
// are tolerated : the walker skips that level and continues. Transport / decode
// failures are returned wrapped with `%w` so callers can `errors.Is`/`As` on
// underlying causes. Context cancellation propagates through c.Get; this
// function spawns no goroutines.
//
// IEEE-036 tests deferred per Craig override 2026-05-12. Required coverage
// captured in the backlog ticket and the PR body.
func walkDERProgramTree(
	ctx context.Context,
	client *inverter.SEP2Client,
	fsaList sep2.FunctionSetAssignmentsList,
	out map[string]sep2.DERProgram,
) error {
	for _, fsa := range fsaList.FunctionSetAssignments {
		if fsa.DERProgramListLink == nil {
			log.Printf("  FSA mRID=%s has no DERProgramListLink; skipping", fsa.MRID)
			continue
		}
		// IEEE-047: GetDERProgramList / GetDefaultDERControl / GetDERControlList
		// follow once on 301. The new hrefs are discarded here because this
		// is a one-shot discovery walk (per-FSA, per-program); a stale
		// per-program href will pay one extra redirect on the next
		// rediscovery rather than per-tick. Acceptable scope.
		progList, _, err := client.GetDERProgramList(ctx, fsa.DERProgramListLink.Href)
		if err != nil {
			return fmt.Errorf("FSA mRID=%s DERProgramList: %w", fsa.MRID, err)
		}
		log.Printf("  FSA mRID=%s: %d DERProgram(s)", fsa.MRID, len(progList.DERProgram))
		for _, prog := range progList.DERProgram {
			if prog.DefaultDERControlLink != nil {
				if _, _, err := client.GetDefaultDERControl(ctx, prog.DefaultDERControlLink.Href); err != nil {
					return fmt.Errorf("DERProgram mRID=%s DefaultDERControl: %w", prog.MRID, err)
				}
			}
			if prog.DERControlListLink != nil {
				if _, _, err := client.GetDERControlList(ctx, prog.DERControlListLink.Href); err != nil {
					return fmt.Errorf("DERProgram mRID=%s DERControlList: %w", prog.MRID, err)
				}
			}
			if prog.DERCurveListLink != nil {
				if _, err := client.GetDERCurveList(ctx, prog.DERCurveListLink.Href); err != nil {
					return fmt.Errorf("DERProgram mRID=%s DERCurveList: %w", prog.MRID, err)
				}
			}
			out[prog.MRID] = prog
		}
	}
	return nil
}

func main() {
	cfg := inverter.SimConfig{}

	flag.StringVar(&cfg.ServerURL, "server", defaultServerURL, "IEEE 2030.5 server URL")
	flag.StringVar(&cfg.CertFile, "cert", "certs/device.crt", "Client certificate PEM")
	flag.StringVar(&cfg.KeyFile, "key", "certs/device.key", "Client private key PEM")
	flag.StringVar(&cfg.CAFile, "ca", "certs/ca.crt", "CA certificate PEM")
	flag.StringVar(&cfg.Scenario, "scenario", "normal", "Scenario name")
	flag.Float64Var(&cfg.TimeScale, "timescale", 60.0, "Simulation speed (60 = 1min real = 1hr sim)")
	flag.DurationVar(&cfg.TickInterval, "tick", 1*time.Second, "Simulation tick interval")
	flag.DurationVar(&cfg.ReportInterval, "report-interval", 10*time.Second, "Status report interval")

	hmiPort := flag.Int("hmi-port", 8080, "HMI web dashboard port (0 to disable)")
	listScenarios := flag.Bool("list-scenarios", false, "List available scenarios and exit")
	flag.BoolVar(&cfg.CSIPStrict, "csip-strict", false, "Strict CSIP TLS: drop GCM fallback, only offer TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8")
	flag.BoolVar(&cfg.CSIP, "csip", false, "CSIP mode: lookup own EndDevice in server's /edev list instead of POST-registering")
	flag.UintVar(&cfg.ExpectedPIN, "pin", 0, "expected Registration PIN (0 = skip match check; nonzero mismatch is fatal per IEEE-034)")
	flag.BoolVar(&cfg.AllowUnregistered, "allow-unregistered", false, "bypass missing-RegistrationLink check in CSIP mode (dev/test only)")

	// IEEE-053 Phase 9 entry: PEN (Private Enterprise Number) stamped into
	// every outbound LogEvent. Env var SEP2_PEN seeds the default; the CLI
	// flag still wins per stdlib flag.Parse() precedence. Default 0 means
	// "no manufacturer namespace" : fine for test / interop, but production
	// deployments MUST register their own PEN with IANA and pass it here
	// so server-side log archives can disambiguate codes across vendors.
	defaultPEN := uint64(0)
	if envPEN := os.Getenv("SEP2_PEN"); envPEN != "" {
		if v, perr := strconv.ParseUint(envPEN, 10, 32); perr == nil {
			defaultPEN = v
		} else {
			fmt.Fprintf(os.Stderr, "warning: SEP2_PEN=%q is not a valid uint32: %v\n", envPEN, perr)
		}
	}
	penFlag := flag.Uint64("pen", defaultPEN, "IANA Private Enterprise Number stamped into outbound LogEvents (env: SEP2_PEN; 0 = no manufacturer namespace)")

	// IEEE-049 Phase 8 entry: inbound HTTPS Notification listener. Default
	// 127.0.0.1:0 binds a random local port; the IEEE-050 subscription POST
	// will publish whatever we actually bound to. Empty string disables the
	// listener (no subscription/notification flow; fall back to polling).
	// Env var SEP2_NOTIFY_LISTEN seeds the default but the CLI flag still
	// wins per stdlib flag.Parse() precedence.
	defaultNotifyListen := os.Getenv("SEP2_NOTIFY_LISTEN")
	if defaultNotifyListen == "" {
		defaultNotifyListen = "127.0.0.1:0"
	}
	notifyListen := flag.String("notify-listen", defaultNotifyListen, "inbound HTTPS Notification listener address (env: SEP2_NOTIFY_LISTEN; empty disables)")

	// IEEESIM-003: backend selects the physical-state source for the tick loop.
	// Default "synthetic" preserves the existing scenario-harness behavior.
	flag.StringVar(&cfg.Backend, "backend", "synthetic", "device backend: synthetic|gridlabd|realdevice")

	// IEEESIM-004: role selects the consumer-policy role for notification
	// dispatch. Default "simulator" preserves the existing behavior. The role
	// is resolved once at construction into a concrete Dispatcher type per
	// ADR-003: NOT a runtime branch in the dispatch path.
	flag.StringVar(&cfg.Role, "role", "simulator", "consumer-policy role: simulator|production")

	flag.Parse()

	// IEEE-053: clamp PEN to uint32 range. flag.Uint64Var lets us catch
	// out-of-range input from CLI / env without silently truncating.
	if *penFlag > 0xFFFFFFFF {
		fmt.Fprintf(os.Stderr, "--pen=%d exceeds uint32 max (4294967295)\n", *penFlag)
		os.Exit(1)
	}
	cfg.LogEventPEN = uint32(*penFlag)

	if *listScenarios {
		scenarios := inverter.AllScenarios()
		names := make([]string, 0, len(scenarios))
		for k := range scenarios {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Println("Available scenarios:")
		for _, name := range names {
			s := scenarios[name]
			fmt.Printf("  %-15s %s (%v)\n", name, s.Description, s.Duration)
		}
		return
	}

	scenarios := inverter.AllScenarios()
	scenario, ok := scenarios[cfg.Scenario]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown scenario: %s (use --list-scenarios)\n", cfg.Scenario)
		os.Exit(1)
	}

	// IEEESIM-003: construct the DERDevice once. The tick loop talks only to
	// dev; it never branches on backend identity.
	dev, err := device.New(cfg, scenario)
	if err != nil {
		fmt.Fprintf(os.Stderr, "device.New: %v\n", err)
		os.Exit(1)
	}

	// IEEESIM-004: construct the notification Dispatcher once at the same call
	// site as device.New. The two axes (role, backend) are independent; any
	// combination is constructible. Coherence check: production role with a
	// synthetic or gridlabd backend is a permitted dry-run (Fork C decision)
	// but unusual enough to warn loudly at startup.
	notifyDispatcher, err := dispatch.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch.New: %v\n", err)
		os.Exit(1)
	}
	if warned, _ := dispatch.CheckRoleBackendCoherence(cfg.Role, cfg.Backend); warned {
		log.Printf("WARNING: role=%q paired with backend=%q (production role with a non-realdevice backend is a dry-run commissioning combination, not the normal production path)", cfg.Role, cfg.Backend)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start HMI
	var hmi *inverter.HMI
	if *hmiPort > 0 {
		hmi = inverter.NewHMI()
		hmiServer := newHMIServer(*hmiPort)
		hmiServer.Handler = hmi.Handler()
		go func() {
			log.Printf("HMI dashboard: http://localhost:%d", *hmiPort)
			if err := hmiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("HMI server error: %v", err)
			}
		}()
	}

	// IEEE-049 Phase 8 entry: inbound HTTPS Notification receiver. The
	// listener uses the same gotls (CCM-8) stack as the outbound client
	// and presents the device cert as its server cert.
	//
	// IEEESIM-004: notifyDispatcher is now a dispatch.Dispatcher constructed
	// above by dispatch.New(cfg). Phase 5 dependencies (client, cache,
	// DERControlListHref) are bound later via RegisterDERControlList once those
	// values are known. Until then the dispatcher logs and drops (IEEE-049
	// no-op semantics): the listener can come up before the DERControlList href
	// is discovered during Phase 4.
	//
	// Failure policy: graceful bypass (per IEEE-048 philosophy). The
	// receiver is optional: CSIP V1.2 CORE-018 recommends but does not
	// require it; the inverter has working polling for every function set
	// in scope. If cert load, TCP bind, or address resolution fails, log
	// the error and continue with polling-only rather than crash.
	notifyReceiver := startNotifyReceiver(cfg, *notifyListen, hmi, notifyDispatcher.Dispatch)
	if notifyReceiver != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := notifyReceiver.Stop(shutdownCtx); err != nil {
				log.Printf("notify receiver stop: %v", err)
			}
		}()
	}

	log.Printf("Inverter Simulator : scenario: %s (%s)", scenario.Name, scenario.Description)
	log.Printf("Server: %s | TimeScale: %.0fx | Tick: %v", cfg.ServerURL, cfg.TimeScale, cfg.TickInterval)

	// Create 2030.5 client
	client, err := inverter.NewSEP2Client(cfg)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	log.Printf("Device identity : SFDI: %s LFDI: %s", client.SFDI(), client.LFDI())

	// Phase 1: Discovery
	log.Println("=== Phase 1: Discovery ===")
	dcap, err := client.Discover(ctx)
	if err != nil {
		log.Fatalf("discover: %v", err)
	}
	log.Printf("DeviceCapability: href=%s pollRate=%d", dcap.Href, dcap.PollRate)
	if dcap.TimeLink != nil {
		log.Printf("  TimeLink: %s", dcap.TimeLink.Href)
	}
	if dcap.EndDeviceListLink != nil {
		log.Printf("  EndDeviceListLink: %s", dcap.EndDeviceListLink.Href)
	}

	// IEEE-028: per CSIP §6.6 / IEEE 2030.5 §10.3, when DeviceCapability
	// advertises no function-set links the device MUST idle-re-poll at the
	// advertised pollRate rather than crash forward into Phase 2 against
	// paths the server has not provisioned. WaitForAdvertisedLinks returns
	// immediately if at least one link is already present.
	dcap, err = client.WaitForAdvertisedLinks(ctx, dcap)
	if err != nil {
		log.Fatalf("wait for advertised links: %v", err)
	}

	// Phase 1b: Server-time sync (IEEE-031). Per IEEE 2030.5 §10 / CSIP,
	// devices source time from the server's Time resource advertised by
	// DeviceCapability.TimeLink and use it : not local wall-clock : for
	// every server-consumed timestamp. We do a synchronous initial sync
	// so the offset is populated before Phase 2 starts; the goroutine
	// then refreshes the offset at DefaultTimeSyncPollRate. The goroutine
	// exits cleanly when the inverter's root context cancels (Ctrl-C
	// handler already wired). If TimeLink is absent the inverter
	// degrades to local clock : log it and proceed.
	//
	// IEEE-048: extracted into runPhase1bTimeSync so the previously-fatal
	// log.Fatalf on Time-resource fetch failure is replaced with graceful
	// bypass (Phase 7 exit criterion 1). See phase1b_timesync.go.
	log.Println("=== Phase 1b: Time Sync ===")
	if err := runPhase1bTimeSync(ctx, client, dcap); err != nil {
		// Only context.Canceled / context.DeadlineExceeded reach here under
		// the Phase 7 graceful-bypass policy; treat them the same as every
		// other ctx-cancel exit in main().
		return
	}

	// Phase 2: EndDevice acquisition.
	//
	// IEEE-029: CSIP mode (--csip) GETs the server's EndDeviceList and finds
	// our own EndDevice by LFDI match : CSIP devices are pre-allowlisted
	// out-of-band, so the device discovers a pre-provisioned EndDevice
	// rather than POSTing /edev. If our LFDI is not in the list yet, idle
	// and re-poll at dcap.PollRate (default 30s). IEEE 2030.5 mode (--csip
	// off, the default) keeps the self-registration POST /edev path.
	// Phase 2 / 3 / 4 walk the link graph reachable from /dcap rather than
	// hardcoding URLs. Per IEEE 2030.5 §10.3 / CSIP §6.6 the client MUST
	// derive every endpoint from advertised links : the server is free to
	// host resources at any path. Each phase skips with a log line if the
	// upstream link is absent (server did not advertise that function set).
	// See IEEE-030.
	edevListHref := ""
	if dcap.EndDeviceListLink != nil {
		edevListHref = dcap.EndDeviceListLink.Href
	}

	var edev sep2.EndDevice
	if cfg.CSIP {
		log.Println("=== Phase 2: EndDevice Lookup (CSIP) ===")
		if edevListHref == "" {
			log.Fatalf("--csip set but DeviceCapability has no EndDeviceListLink")
		}
		for {
			// IEEE-047: on 301 LookupOwnEndDevice surfaces the new edev-list
			// base href; store it locally so the next idle poll iteration
			// (and the Phase 2b re-lookup) hits the new URL directly.
			var newEdevListHref string
			edev, newEdevListHref, err = client.LookupOwnEndDevice(ctx, edevListHref)
			if newEdevListHref != "" {
				log.Printf("Phase 2 lookup: 301 follow : cached edev-list href %s → %s",
					edevListHref, newEdevListHref)
				edevListHref = newEdevListHref
			}
			if err == nil {
				break
			}
			if !errors.Is(err, inverter.ErrEndDeviceNotFound) {
				log.Fatalf("lookup own EndDevice: %v", err)
			}
			pollEvery := time.Duration(dcap.PollRate) * time.Second
			if pollEvery <= 0 {
				pollEvery = 30 * time.Second
			}
			log.Printf("Own EndDevice not in server list; re-polling every %s", pollEvery)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollEvery):
			}
		}
		log.Printf("Found own EndDevice: href=%s SFDI=%s", edev.Href, edev.SFDI)
	} else {
		log.Println("=== Phase 2: Registration ===")
		if edevListHref == "" {
			log.Fatalf("DeviceCapability has no EndDeviceListLink; registration impossible")
		}
		// IEEE-047: on 301 Register surfaces the new edev-list base href;
		// store it locally so any downstream phase that re-uses edevListHref
		// (Phase 2b re-lookup idle loop) hits the new URL directly.
		var newEdevListHref string
		edev, newEdevListHref, err = client.Register(ctx, edevListHref)
		if err != nil {
			log.Fatalf("register: %v", err)
		}
		if newEdevListHref != "" {
			log.Printf("Phase 2 register: 301 follow : cached edev-list href %s → %s",
				edevListHref, newEdevListHref)
			edevListHref = newEdevListHref
		}
		log.Printf("Registered: href=%s SFDI=%s", edev.Href, edev.SFDI)
	}

	// Phase 2b: Registration resource read + PIN match check + commissioning
	// gate. Extracted by IEEE-074 into runPhase2bRegistration so the deferred
	// IEEE-071 integration cases have a function seam to test against. The
	// full behavior contract (missing-RegistrationLink branches, PIN-match
	// branches, idle-loop policy, PIN redaction) lives in
	// cmd/inverterclient/phase2b.go.
	//
	// log.Fatalf stays HERE : main() is the exit-code owner. The extracted
	// function returns *phase2bFatal in place of every previous inline
	// log.Fatalf, which main() unwraps via errors.As. ctx-cancel inside the
	// function returns ctx.Err() (context.Canceled / DeadlineExceeded);
	// main() treats that the same as the previous inline `return` on
	// `<-ctx.Done()`.
	{
		newEdev, err := runPhase2bRegistration(ctx, client, edev, edevListHref, cfg, dcap)
		if err != nil {
			var fe *phase2bFatal
			if errors.As(err, &fe) {
				log.Fatalf("%s", fe.Error())
			}
			return
		}
		edev = newEdev
	}

	// IEEE-050 Phase 8 ticket 2 of 4: register Subscriptions with the SEP2
	// server so it can POST Notifications to the IEEE-049 /notify listener.
	// Runs after Phase 2b (we now know our EndDevice and its links) and
	// before Phase 2c (the rest of the link-graph walk doesn't depend on
	// subscription state). All failure modes degrade to polling : never
	// fatal : so CSIP V1.2's "recommended but not required" subscription
	// flow stays optional. The returned map keys are the subscribed
	// resource hrefs; values are the server-assigned subscription hrefs
	// (consumed by IEEE-052 for cancellation handling).
	subscriptionsByResource := registerSubscriptions(
		ctx,
		client,
		edev,
		notifyURLForReceiver(notifyReceiver),
	)
	// IEEE-052 Phase 8 ticket 4 of 4 (final): wire the registry's Cancel
	// method as the dispatcher's CancelHook so status=1 notifications
	// (CSIP V1.2 CORE-019 step 13: "subscription cancelled by server")
	// free the inverter-side subscription entry. Polling for the affected
	// resource continues unaffected : the hook only cleans local state
	// so a future re-subscription attempt can fire again. Safe to wire
	// even when the registry is empty (no entries → cancel is a no-op).
	notifyDispatcher.RegisterCancelHook(subscriptionsByResource.CancelHookFunc())

	// Phase 2c: FunctionSetAssignmentsList discovery (IEEE-035 : plan-1
	// phase 4 entry). Extracted by IEEE-075 into runPhase2cFSAList so the
	// deferred IEEE-072 integration cases have a function seam to test
	// against. The full behavior contract (missing-link branches, empty-
	// list idle policy, CSIP-strict gating) lives in
	// cmd/inverterclient/phase2c_fsalist.go.
	//
	// log.Fatalf stays HERE : main() is the exit-code owner. The extracted
	// function returns *fsaListFatal in place of every previous inline
	// log.Fatalf, which main() unwraps via errors.As. ctx-cancel inside the
	// function returns ctx.Err() (context.Canceled / DeadlineExceeded);
	// main() treats that the same as the previous inline `return` on
	// `<-ctx.Done()`.
	fsaList, err := runPhase2cFSAList(ctx, client, edev, cfg, dcap)
	if err != nil {
		var fe *fsaListFatal
		if errors.As(err, &fe) {
			log.Fatalf("%s", fe.Error())
		}
		return
	}
	// fsaList is the Phase 2c cache seam consumed by IEEE-036 (FSA -> DERProgram
	// tree walk, below) and IEEE-037 (Primacy + mRID selection).
	log.Printf("Phase 2c (FSAList): cached %d FSA(s)", len(fsaList.FunctionSetAssignments))

	// Phase 2c (continued, IEEE-036): walk each FSA's DERProgramListLink and
	// fetch each DERProgram's DefaultDERControl + DERControlList + DERCurveList
	// subtrees. CSIP V1.2 CORE-012 step 2 : list discovery, NOT selection.
	// Primacy + mRID selection of the highest-priority DERProgram is IEEE-037,
	// the next ticket. Control application (consuming the cache) is Phase 5.
	//
	// Extracted by IEEE-076 into runPhase2cDERProgramWalk so the 2 deferred
	// IEEE-073 integration cases (#4 empty-aggregate idle-loop, #6 pollRate
	// throttling) have a function seam to test against. The per-FSA /
	// per-program tolerated missing-link policy lives in walkDERProgramTree;
	// the outer-loop empty-aggregate idle-vs-proceed policy and the
	// walk-error fatal path live in cmd/inverterclient/phase2c_derprogram.go.
	//
	// The cache `derProgramsByMRID` is the seam IEEE-037 consumes. Keying on
	// mRID matches the IEEE 2030.5 §10.1.3 list-ordering tie-break field; if
	// the same DERProgram is reachable from multiple FSAs the later GET wins
	// (acceptable per the spec : DERProgram resources are identified by mRID,
	// not by FSA path).
	//
	// log.Fatalf stays HERE : main() is the exit-code owner. The extracted
	// function returns *derProgramWalkFatal in place of the previous inline
	// log.Fatalf, which main() unwraps via errors.As. ctx-cancel inside the
	// function returns ctx.Err() (context.Canceled / DeadlineExceeded);
	// main() treats that the same as the previous inline `return` on
	// `<-ctx.Done()`.
	//
	// Empty FSAList short-circuits without entering the walk (preserves the
	// prior inline `if len(fsaList.FunctionSetAssignments) > 0` guard).
	derProgramsByMRID := make(map[string]sep2.DERProgram)
	if len(fsaList.FunctionSetAssignments) > 0 {
		cache, err := runPhase2cDERProgramWalk(ctx, client, fsaList, cfg, dcap)
		if err != nil {
			var fe *derProgramWalkFatal
			if errors.As(err, &fe) {
				log.Fatalf("%s", fe.Error())
			}
			return
		}
		derProgramsByMRID = cache
	}
	// Phase 2c (continued, IEEE-037): apply IEEE 2030.5 §10.1.3 list-ordering
	// + CSIP V1.2 CORE-012 step 2 selection over the IEEE-036 cache. Lowest
	// Primacy wins; ties on Primacy broken by MRID lex-min. Empty cache →
	// no selection; the simulation tick loop's ApplyControls call falls back
	// to nil base via inverter.ActiveControlBase's rule-3 (no-op semantics).
	//
	// selectedDERProgram + selectedDefaultControlHref feed IEEE-041 below:
	// the href drives a one-shot GET of DefaultDERControl, whose base is the
	// fallback applied whenever the IEEE-040 state machine is not in
	// EVENT_STARTED.
	selectedDERProgram, selected := inverter.SelectHighestPriority(derProgramsByMRID)
	var selectedDefaultControlHref string
	if selected {
		if selectedDERProgram.DefaultDERControlLink != nil {
			selectedDefaultControlHref = selectedDERProgram.DefaultDERControlLink.Href
		}
		log.Printf("Phase 2c (Primacy selection): winner mRID=%s primacy=%d DefaultDERControlLink=%q",
			selectedDERProgram.MRID, selectedDERProgram.Primacy, selectedDefaultControlHref)
	} else {
		log.Println("Phase 2c (Primacy selection): no DERProgram cached; ApplyControls will fall back to nil base via ActiveControlBase rule-3")
	}
	// IEEE-041: GET the DefaultDERControl resource so the tick loop can fall
	// back to it whenever the state machine reports anything other than
	// EVENT_STARTED. walkDERProgramTree already validated the link (and
	// discarded the value); we re-GET it here once selection is complete so
	// the chosen program's default base is in scope at the consumption seam.
	//
	// Error handling: tolerant : log + leave defaultCtl nil. Mirrors the
	// IEEE-035 / IEEE-036 missing-link policy: a server that advertises a
	// default but fails to serve it should not crash the inverter; the
	// helper's rule-3 fall-through gives the existing no-op semantics.
	var defaultCtl *sep2.DefaultDERControl
	if selectedDefaultControlHref != "" {
		// IEEE-047: on 301 GetDefaultDERControl surfaces the new href; log
		// the follow for observability. The local selectedDefaultControlHref
		// has no further reads in this code path (the DefaultDERControl is
		// one-shot per startup), so we do not re-assign it : the follow has
		// already happened inside GetDefaultDERControl.
		ddc, newDefaultDERControlHref, err := client.GetDefaultDERControl(ctx, selectedDefaultControlHref)
		if err != nil {
			log.Printf("Phase 2c (IEEE-041): GET DefaultDERControl %s: %v (continuing with no default base)",
				selectedDefaultControlHref, err)
		} else {
			if newDefaultDERControlHref != "" {
				log.Printf("Phase 2c (IEEE-041): 301 follow : DefaultDERControl href %s → %s (one-shot; not re-cached)",
					selectedDefaultControlHref, newDefaultDERControlHref)
			}
			ddcCopy := ddc.Copy()
			defaultCtl = &ddcCopy
			hasBase := defaultCtl.DERControlBase != nil
			log.Printf("Phase 2c (IEEE-041): resolved DefaultDERControl mRID=%s DERControlBase-present=%t",
				defaultCtl.MRID, hasBase)
		}
	}

	// Phase 5 entry (IEEE-038): start the DERControlList polling goroutine on
	// the active DERProgram's DERControlListLink. The cache surfaces added /
	// updated / cancelled events for the IEEE-039 scheduler and IEEE-040 state
	// machine that follow.
	//
	// pollRate source: dcap.PollRate. The advertised list-level pollRate lives
	// on the ListResource returned by the GET : not on the *ListLink : so we
	// seed the loop with the device's top-level pollRate the same way IEEE-029
	// / IEEE-034 / IEEE-035 reuse it. IEEE-039+ may switch to the list-level
	// pollRate once one tick has populated the cache.
	//
	// Missing-DERControlListLink: log + skip. CORE-012 step 6 polling is
	// conditional on the link existing; a DERProgram without DERControls
	// (DefaultDERControl-only) is a valid CSIP shape.
	derControlCache := inverter.NewDERControlCache()
	if selected && selectedDERProgram.DERControlListLink != nil {
		dercListHref := selectedDERProgram.DERControlListLink.Href
		log.Printf("Phase 5 (IEEE-038): starting DERControlList poll href=%s pollRate=%ds",
			dercListHref, dcap.PollRate)
		go func() {
			if err := client.PollDERControlList(ctx, dercListHref, dcap.PollRate, derControlCache); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("DERControlList poll loop exited: %v", err)
			}
		}()
		// IEEE-051: now that we know the DERControlListHref + cache + client,
		// bind them into the dispatcher so the IEEE-049 /notify listener stops
		// being a no-op. Polling stays active above; notifications additively
		// cut the latency floor from pollRate to "as soon as server POSTs."
		// Bypass when the receiver never came up (notifyReceiver == nil) :
		// no listener means no inbound POSTs, so the dispatcher would never
		// fire anyway. The Register call is still safe (idempotent), but the
		// log line would be misleading without a corresponding listener.
		if notifyReceiver != nil {
			if err := notifyDispatcher.RegisterDERControlList(client, derControlCache, dercListHref); err != nil {
				log.Printf("Phase 8 (IEEE-051): dispatcher Register failed (%v); notifications will log+drop", err)
			} else {
				log.Printf("Phase 8 (IEEE-051): notification dispatcher registered for DERControlList=%s", dercListHref)
			}
		}
	} else {
		log.Println("Phase 5 (IEEE-038): no DERControlListLink on selected program; polling skipped")
	}
	// Phase 5 entry (IEEE-040): state machine that ties IEEE-038's cache and
	// IEEE-039's scheduler together. Drives DEFAULT ↔ EVENT_RECEIVED ↔
	// EVENT_STARTED ↔ (EVENT_COMPLETED | EVENT_CANCELLED) → DEFAULT.
	//
	// The state-machine tick goroutine runs at the same cadence as the cache
	// poll (dcap.PollRate, floored at 60s) so each tick can observe the
	// freshest cache state. We hold a closure-local previous-snapshot map so
	// cache.Diff yields added/cancelled buckets for THIS tick.
	//
	// pollDuration is duplicated rather than refactored : pinPollInterval
	// lives in cmd/inverterclient and is out of scope to extract. The
	// duplication is acknowledged in IEEE-040's ticket; the helper is the
	// same internal/inverter.derControlPollDuration policy (60s floor,
	// 30min default-on-zero).
	//
	// Hook: IEEE-042 installs a curve-refresh hook here that fetches
	// the active DERProgram's DERCurveList on every EVENT_RECEIVED →
	// EVENT_STARTED transition (CSIP V1.2 CORE-012 step 6 : curves are
	// part of "apply," not "discover"). Phase 6 (IEEE-043+) layers the
	// Response Function Set emitter on top of this same hook surface.
	sched := inverter.NewScheduler(client.Now, mathrand.New(mathrand.NewPCG(uint64(time.Now().UnixNano()), 0xCAFEBABE)))
	stateMachine := inverter.NewStateMachine()

	// IEEE-042: curve cache populated by the state-machine hook on
	// EVENT_RECEIVED → EVENT_STARTED. ApplyControlsWithCurves reads from
	// it on every sim tick; cache miss falls back to IEEE 1547 default
	// curves (see internal/inverter/controller.go).
	//
	// IEEE-044 switched the curve-refresh registration from OnTransition
	// (replace-only) to AddTransitionHook (append). The Response POST
	// hook below is the second consumer of the state machine's hook
	// surface and must compose with this one. See response_hook.go.
	curveCache := inverter.NewDERCurveCache()
	if selected && selectedDERProgram.DERCurveListLink != nil {
		curveListHref := selectedDERProgram.DERCurveListLink.Href
		stateMachine.AddTransitionHook(func(prev, next inverter.EventState, _ *sep2.DERControl) {
			// Fire only on the start-of-event edge. Cancellation before
			// start never reaches EVENT_STARTED, so no fetch fires for a
			// cancelled event. State-machine hooks run OUTSIDE its mutex
			// (IEEE-040 contract), so we spawn a ctx-bound goroutine to
			// keep the Tick path non-blocking even if the server is slow.
			if prev == inverter.StateEventStarted || next != inverter.StateEventStarted {
				return
			}
			go func() {
				if err := inverter.FetchProgramCurves(ctx, client, curveListHref, curveCache); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					log.Printf("Phase 5 (IEEE-042): curve refresh failed (continuing with cached/default curves): %v", err)
					return
				}
				log.Printf("Phase 5 (IEEE-042): refreshed %d curve type(s) from %s", curveCache.Len(), curveListHref)
			}()
		})
	} else if selected {
		log.Println("Phase 5 (IEEE-042): no DERCurveListLink on selected program; curve refresh disabled (controller falls back to IEEE 1547 defaults)")
	}

	// IEEE-044: Wire state-machine transitions to PostResponse. The hook
	// filters on evt.ReplyTo + evt.ResponseRequired + Table 31 status
	// mapping internally; here we just register it. lfdi is the inverter's
	// LFDI hex (set in Phase 2 via SEP2Client.LFDI()).
	if selected {
		stateMachine.AddTransitionHook(responsePOSTHook(client, client.LFDI(), client.Now))
		log.Println("Phase 6 (IEEE-044): response POST hook installed")
	}

	if selected && selectedDERProgram.DERControlListLink != nil {
		tickInterval := pinPollInterval(dcap.PollRate)
		log.Printf("Phase 5 (IEEE-040): starting state-machine tick interval=%s", tickInterval)
		go func() {
			t := time.NewTicker(tickInterval)
			defer t.Stop()
			prev := derControlCache.Snapshot()
			tick := func() {
				curr := derControlCache.Snapshot()
				// Translate the cache to a fresh DERControl slice for Diff :
				// Diff already returns Copy() values for added/cancelled.
				next := make([]sep2.DERControl, 0, len(curr))
				for _, v := range curr {
					next = append(next, v)
				}
				// cache.Diff is computed against the live cache state, not
				// against `prev`. We track `prev` so on the next iteration we
				// see freshly-added events; the Diff signature consults the
				// cache's internal map directly. For the state machine we want
				// "deltas since LAST TICK", so use a manual diff against prev.
				added, cancelled := diffSnapshots(prev, curr)
				stateMachine.Tick(client.Now(), added, cancelled, sched)
				prev = curr
			}
			tick() // initial tick so a fresh poll seeds the state machine.
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				tick()
			}
		}()
	} else {
		log.Println("Phase 5 (IEEE-040): no DERControlListLink on selected program; state-machine tick skipped")
	}
	// stateMachine + defaultCtl are now live consumers : see the simulation
	// tick loop below, which feeds inverter.ActiveControlBase(stateMachine.
	// Current(), defaultCtl) into ApplyControls. IEEE-041 closes the
	// long-standing ApplyControls(nil, ...) defect at this seam.

	// Phase 3: DER Setup : follow EndDevice.DERListLink to find the first
	// DER, then PUT to its DERCapabilityLink / DERSettingsLink. DERStatus
	// goes through the reporter loop in Phase 5 against DERStatusLink.
	log.Println("=== Phase 3: DER Setup ===")
	var derStatusHref string
	if edev.DERListLink == nil {
		log.Println("EndDevice has no DERListLink; skipping Phase 3 DER setup")
	} else {
		// IEEE-047: on 301 client.Get surfaces the new DERList URL; one-shot
		// Phase 3 setup so we log it for diagnostics rather than threading
		// it onward (DER setup PUTs that follow are link-derived from
		// derList.DER entries : no DERList href reuse downstream).
		// IEEE-048: extracted into fetchDERListForSetup so the previously-
		// fatal log.Fatalf on DER-list fetch failure is replaced with
		// graceful bypass (Phase 7 exit criterion 1). See phase3_derlist.go.
		derList, ok, newDERListHref, err := fetchDERListForSetup(ctx, client, edev.DERListLink.Href)
		switch {
		case err != nil:
			// Only context.Canceled / context.DeadlineExceeded reach here
			// under the Phase 7 graceful-bypass policy; treat them the same
			// as every other ctx-cancel exit in main().
			return
		case !ok:
			// fetchDERListForSetup already logged the bypass cause. Fall
			// through to the rest of main() with Phase 3 skipped.
		default:
			if newDERListHref != "" {
				log.Printf("Phase 3 DER list: 301 follow : original %s → %s (one-shot; not cached)",
					edev.DERListLink.Href, newDERListHref)
			}
			if len(derList.DER) == 0 {
				log.Println("DER list empty; skipping Phase 3 DER setup")
			} else {
				// First DER only : multi-DER inverters are a follow-up.
				der := derList.DER[0]

				maxW := sep2.ActivePower{Value: int64(inverter.Rating.RatedW)}
				maxVAr := sep2.ReactivePower{Value: int64(inverter.Rating.RatedVAr)}
				modesSupported := uint32(0xFF) // all modes
				derType := uint8(4)            // PV inverter

				if der.DERCapabilityLink != nil {
					if err := client.PutDERCapability(ctx, der.DERCapabilityLink.Href, sep2.DERCapability{
						RTGMaxW:        &maxW,
						RTGMaxVar:      &maxVAr,
						ModesSupported: &modesSupported,
						Type:           &derType,
					}); err != nil {
						log.Printf("PUT DERCapability: %v (continuing)", err)
					}
				} else {
					log.Println("DER has no DERCapabilityLink; skipping DERCapability PUT")
				}

				setMaxW := sep2.ActivePower{Value: int64(inverter.Rating.RatedW)}
				if der.DERSettingsLink != nil {
					// IEEE-031: outbound timestamp : use the server-synced clock
					// rather than local wall-clock. Before any TimeLink sync runs
					// client.Now() degrades to time.Now(), so this is safe even
					// when no TimeLink was advertised.
					if err := client.PutDERSettings(ctx, der.DERSettingsLink.Href, sep2.DERSettings{
						SetMaxW:     &setMaxW,
						UpdatedTime: client.Now().Unix(),
					}); err != nil {
						log.Printf("PUT DERSettings: %v (continuing)", err)
					}
				} else {
					log.Println("DER has no DERSettingsLink; skipping DERSettings PUT")
				}

				if der.DERStatusLink != nil {
					derStatusHref = der.DERStatusLink.Href
				} else {
					log.Println("DER has no DERStatusLink; Phase 5 status reporting disabled")
				}
				log.Println("DER capability and settings reported")
			}
		}
	}

	// Phase 4: Metering Setup : POST a MirrorUsagePoint to the list href
	// advertised by DeviceCapability. The response Location is then GET to
	// read back the MirrorMeterReadingListLink for Phase 5 readings.
	log.Println("=== Phase 4: Metering Setup ===")
	var mmrHref string
	if dcap.MirrorUsagePointListLink == nil {
		log.Println("DeviceCapability has no MirrorUsagePointListLink; metering disabled")
	} else {
		mupLoc, err := client.CreateMirrorUsagePoint(ctx, dcap.MirrorUsagePointListLink.Href, sep2.MirrorUsagePoint{
			MRID:                "mup-" + client.SFDI()[:8],
			Description:         "PV Inverter Metering",
			ServiceCategoryKind: 0,
			Status:              1,
		})
		if err != nil {
			log.Printf("create MirrorUsagePoint: %v (metering disabled)", err)
		} else if mupLoc == "" {
			log.Println("MirrorUsagePoint POST returned empty Location; metering disabled")
		} else {
			log.Printf("MirrorUsagePoint: %s", mupLoc)
			// Read back the created resource to discover its
			// MirrorMeterReadingListLink : we do not assume the URL.
			// IEEE-047: on 301 client.Get surfaces the new MUP URL; update
			// mupLoc so any future reference (none in the current code,
			// but the var is the canonical hold-point) targets the new
			// URL.
			var mup sep2.MirrorUsagePoint
			newMupLoc, err := client.Get(ctx, mupLoc, &mup)
			if newMupLoc != "" {
				log.Printf("Phase 4 MUP read-back: 301 follow : cached mupLoc %s → %s",
					mupLoc, newMupLoc)
				mupLoc = newMupLoc
			}
			if err != nil {
				log.Printf("GET MirrorUsagePoint %s: %v (metering disabled)", mupLoc, err)
			} else if mup.MirrorMeterReadingListLink == nil {
				log.Println("MirrorUsagePoint has no MirrorMeterReadingListLink; metering disabled")
			} else {
				mmrHref = mup.MirrorMeterReadingListLink.Href
			}
		}
	}

	// Create reporter : empty hrefs cause the corresponding channel to be
	// a silent no-op (see reporter.go).
	reporter := inverter.NewReporter(client, derStatusHref, mmrHref)

	// IEEE-054: wire the LogEvent rate-limiter + alarm transition detector.
	//
	// Source the LogEventList href from EndDevice.LogEventListLink (set by
	// IEEE-030's registration/lookup). An empty/missing link means the
	// server doesn't advertise the OPTIONAL LogEvent function set :
	// NewAlarmDetector degrades to a no-op in that case (Evaluate becomes
	// free) so the simulator runs without complaint.
	var logEventListHref string
	if edev.LogEventListLink != nil {
		logEventListHref = edev.LogEventListLink.Href
	}
	logEventLimiter := inverter.NewPerCodeLogEventLimiter(inverter.DefaultLogEventWindow, time.Now)
	client.SetLogEventRateLimiter(logEventLimiter)
	alarmDetector := inverter.NewAlarmDetector(client, logEventListHref)
	if logEventListHref == "" {
		log.Println("IEEE-054: EndDevice has no LogEventListLink; alarm-LogEvent emit disabled (graceful bypass)")
	} else {
		log.Printf("IEEE-054: alarm-LogEvent emit enabled : href=%s window=%v",
			logEventListHref, logEventLimiter.Window())
	}

	// Phase 5: Simulation Loop
	log.Printf("=== Phase 5: Simulation: %s ===", scenario.Name)

	// IEEESIM-003: the tick loop now talks only to dev (device.DERDevice).
	// It never branches on backend identity; physical-state production and
	// setpoint application are fully behind the interface. The scenario
	// duration check uses an optional interface so the Synthetic backend can
	// signal completion without the loop knowing its internals.
	type scenarioEnder interface {
		ScenarioDone() bool
		SimTime() time.Time
	}

	lastReport := time.Time{}
	// IEEE-054: track contiguous abnormal-condition duration so the trip
	// curves in ridethrough.go have a duration argument. Reset on every
	// nominal-grid tick.
	abnormalSince := time.Time{}
	// IEEE-054: track pre-disturbance active power for the FreqDroop /
	// LE_ACTIVE_LIMIT classification. Captured on each nominal tick;
	// freeze during disturbance so the classification sees the original
	// setpoint, not the curtailed value.
	preDisturbancePW := 0.0

	ticker := time.NewTicker(cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			return
		case <-ticker.C:
			// ReadState: advance the device's internal clock, walk scenario
			// steps, and return the current grid conditions + power ceiling.
			// On error (hardware comms loss), apply the fail-safe and skip
			// the control cycle.
			reading, readErr := dev.ReadState(ctx)
			if readErr != nil {
				log.Printf("ReadState failed (fail-safe): %v", readErr)
				// Skip this tick; the next tick will retry.
				continue
			}

			// Check scenario completion (Synthetic backend only via optional
			// interface; hardware backends run until context cancellation).
			if se, ok := dev.(scenarioEnder); ok && se.ScenarioDone() {
				log.Printf("Scenario %s complete (duration %v)", scenario.Name, scenario.Duration)
				return
			}

			// Derive simTime for alarm-detection and logging. Hardware backends
			// do not implement scenarioEnder; use wall time as a fallback so
			// the alarm detector always gets a non-zero timestamp.
			var simTime time.Time
			if se, ok := dev.(scenarioEnder); ok {
				simTime = se.SimTime()
			} else {
				simTime = reading.Grid.Time
			}

			// IEEE-041: source the active control base from the Phase 5 state
			// machine. EVENT_STARTED -> the active event's DERControlBase;
			// otherwise the program's DefaultDERControl base; otherwise nil
			// (no CSIP server / no default provisioned -> preserves the
			// pre-IEEE-041 no-op semantics). See
			// internal/inverter/applycontrols_base.go for the decision tree.
			//
			// IEEE-042: ApplyControlsWithCurves consults curveCache for
			// server-supplied Volt/Var and Volt/Watt curves; misses fall
			// back to IEEE 1547 defaults inside the controller.
			base := inverter.ActiveControlBase(stateMachine.Current(), defaultCtl)
			controls := inverter.ApplyControlsWithCurves(base, reading.Grid, reading.MaxPowerW, curveCache)

			// ApplySetpoint: push the commanded control output to the device
			// and get back the achieved InverterState.
			state, applyErr := dev.ApplySetpoint(ctx, controls)
			if applyErr != nil {
				switch {
				case errors.Is(applyErr, device.ErrMalformedControl):
					log.Printf("WARN ApplySetpoint guard-reject (malformed control): %v", applyErr)
				case errors.Is(applyErr, device.ErrRateLimitExceeded):
					log.Printf("WARN ApplySetpoint guard-reject (rate limit): %v", applyErr)
				default:
					log.Printf("ApplySetpoint failed (fail-safe active): %v", applyErr)
				}
				// Skip reporting for this tick; next tick retries.
				continue
			}

			// IEEE-054: edge-triggered alarm-class detection. Track
			// abnormal-condition duration so the IEEE 1547 trip curves
			// see the right `dur` argument. Track the pre-disturbance
			// active-power setpoint so LE_ACTIVE_LIMIT classification
			// sees the unclipped value.
			isNormalGrid := reading.Grid.VoltsPU >= 0.88 && reading.Grid.VoltsPU <= 1.10 &&
				reading.Grid.FreqHz >= 59.0 && reading.Grid.FreqHz <= 60.5
			if isNormalGrid {
				abnormalSince = time.Time{}
				preDisturbancePW = state.ActivePowerW
			} else if abnormalSince.IsZero() {
				abnormalSince = simTime
			}
			abnormalDur := time.Duration(0)
			if !abnormalSince.IsZero() {
				abnormalDur = simTime.Sub(abnormalSince)
			}
			alarmDetector.Evaluate(ctx, inverter.AlarmInputs{
				Grid:             reading.Grid,
				Connected:        state.Connected,
				Energized:        state.Energized,
				ActivePowerW:     state.ActivePowerW,
				ReactivePowerVAr: state.ReactivePowerVAr,
				PreDisturbancePW: preDisturbancePW,
				RatedVAr:         inverter.Rating.RatedVA, // RatedVAr ~= RatedVA for unity-PF inverter
				AbnormalDuration: abnormalDur,
				Time:             simTime,
			})

			// Broadcast to HMI. reading.Irradiance carries the W/m^2 value
			// from the Synthetic backend; other backends emit 0.
			if hmi != nil {
				hmi.Broadcast(inverter.HMIDataPoint{
					Time:       time.Now().Format("15:04:05"),
					SimTime:    simTime.Format("15:04"),
					P:          state.ActivePowerW,
					Q:          state.ReactivePowerVAr,
					PF:         state.PowerFactor,
					V:          state.VoltsPU,
					F:          state.FreqHz,
					Mode:       state.Mode.String(),
					Connected:  state.Connected,
					Irradiance: reading.Irradiance,
				})
			}

			// Report periodically
			if time.Since(lastReport) >= cfg.ReportInterval {
				if err := reporter.ReportStatus(ctx, state); err != nil {
					log.Printf("ReportStatus failed: %v", err)
				}
				if err := reporter.ReportMetering(ctx, state); err != nil {
					log.Printf("ReportMetering failed: %v", err)
				}
				lastReport = time.Now()

				log.Printf("  sim=%s P=%.0fW Q=%.0fVAr PF=%.3f V=%.3fpu F=%.1fHz mode=%s irr=%.0f",
					simTime.Format("15:04"), state.ActivePowerW, state.ReactivePowerVAr,
					state.PowerFactor, state.VoltsPU, state.FreqHz, state.Mode, reading.Irradiance)
			}
		}
	}
}
