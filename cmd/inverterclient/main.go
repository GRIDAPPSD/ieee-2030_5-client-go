package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// defaultServerURL matches the Makefile's `run-inverter` SERVER_URL default
// so invoking the binary directly behaves the same as `make run-inverter`.
// See IEEE-026.
const defaultServerURL = "https://localhost:8443"

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

// walkDERProgramTree implements the IEEE-036 Phase 2c (continued) walk: for
// each FSA whose DERProgramListLink is non-nil, GET the DERProgramList and
// fetch each DERProgram's DefaultDERControl / DERControlList / DERCurveList
// subtrees (when those links are non-nil). Programs are cached into `out`
// keyed by mRID; if the same DERProgram is reachable from multiple FSAs the
// later GET wins (DERProgram identity is its mRID per IEEE 2030.5 §10.1.3).
//
// Per-FSA missing-DERProgramListLink and per-DERProgram missing-subtree-link
// are tolerated — the walker skips that level and continues. Transport / decode
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
		progList, err := client.GetDERProgramList(ctx, fsa.DERProgramListLink.Href)
		if err != nil {
			return fmt.Errorf("FSA mRID=%s DERProgramList: %w", fsa.MRID, err)
		}
		log.Printf("  FSA mRID=%s: %d DERProgram(s)", fsa.MRID, len(progList.DERProgram))
		for _, prog := range progList.DERProgram {
			if prog.DefaultDERControlLink != nil {
				if _, err := client.GetDefaultDERControl(ctx, prog.DefaultDERControlLink.Href); err != nil {
					return fmt.Errorf("DERProgram mRID=%s DefaultDERControl: %w", prog.MRID, err)
				}
			}
			if prog.DERControlListLink != nil {
				if _, err := client.GetDERControlList(ctx, prog.DERControlListLink.Href); err != nil {
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
	flag.Parse()

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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start HMI
	var hmi *inverter.HMI
	if *hmiPort > 0 {
		hmi = inverter.NewHMI()
		hmiServer := &http.Server{
			Addr:    fmt.Sprintf(":%d", *hmiPort),
			Handler: hmi.Handler(),
		}
		go func() {
			log.Printf("HMI dashboard: http://localhost:%d", *hmiPort)
			if err := hmiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("HMI server error: %v", err)
			}
		}()
	}

	log.Printf("Inverter Simulator — scenario: %s (%s)", scenario.Name, scenario.Description)
	log.Printf("Server: %s | TimeScale: %.0fx | Tick: %v", cfg.ServerURL, cfg.TimeScale, cfg.TickInterval)

	// Create 2030.5 client
	client, err := inverter.NewSEP2Client(cfg)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	log.Printf("Device identity — SFDI: %s LFDI: %s", client.SFDI(), client.LFDI())

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
	// DeviceCapability.TimeLink and use it — not local wall-clock — for
	// every server-consumed timestamp. We do a synchronous initial sync
	// so the offset is populated before Phase 2 starts; the goroutine
	// then refreshes the offset at DefaultTimeSyncPollRate. The goroutine
	// exits cleanly when the inverter's root context cancels (Ctrl-C
	// handler already wired). If TimeLink is absent the inverter
	// degrades to local clock — log it and proceed.
	log.Println("=== Phase 1b: Time Sync ===")
	if dcap.TimeLink != nil {
		serverTime, err := client.SyncServerTime(ctx, dcap.TimeLink.Href)
		if err != nil {
			log.Fatalf("initial server time sync %s: %v", dcap.TimeLink.Href, err)
		}
		offset := client.Now().Sub(time.Now())
		log.Printf("Server time: %s (offset from local: %s)",
			time.Unix(serverTime.CurrentTime, 0).UTC().Format(time.RFC3339),
			offset)
		go client.RunTimeSync(ctx, dcap.TimeLink.Href, inverter.DefaultTimeSyncPollRate)
	} else {
		log.Println("DeviceCapability has no TimeLink; using local clock for outbound timestamps")
	}

	// Phase 2: EndDevice acquisition.
	//
	// IEEE-029: CSIP mode (--csip) GETs the server's EndDeviceList and finds
	// our own EndDevice by LFDI match — CSIP devices are pre-allowlisted
	// out-of-band, so the device discovers a pre-provisioned EndDevice
	// rather than POSTing /edev. If our LFDI is not in the list yet, idle
	// and re-poll at dcap.PollRate (default 30s). IEEE 2030.5 mode (--csip
	// off, the default) keeps the self-registration POST /edev path.
	// Phase 2 / 3 / 4 walk the link graph reachable from /dcap rather than
	// hardcoding URLs. Per IEEE 2030.5 §10.3 / CSIP §6.6 the client MUST
	// derive every endpoint from advertised links — the server is free to
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
			edev, err = client.LookupOwnEndDevice(ctx, edevListHref)
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
		edev, err = client.Register(ctx, edevListHref)
		if err != nil {
			log.Fatalf("register: %v", err)
		}
		log.Printf("Registered: href=%s SFDI=%s", edev.Href, edev.SFDI)
	}

	// Phase 2b: Registration resource read + PIN match check + commissioning gate
	// (IEEE-032 landed the GET; IEEE-033 added enforcement; IEEE-034 closes the
	// two deviations from Noor's plan and adds missing-RegistrationLink handling).
	// CSIP V1.2 BASIC-001 step 5 / IEEE 2030.5 §10 require the device to walk
	// EndDevice.RegistrationLink, parse the Registration resource, and refuse
	// to proceed unless the server-presented pIN matches the out-of-band-
	// provisioned PIN (--pin / cfg.ExpectedPIN).
	//
	// Missing-RegistrationLink behavior:
	//   - --csip=false OR --allow-unregistered=true: log bypass, skip Phase 2b.
	//     Operator-acknowledged dev/test bypass.
	//   - --csip strict (default in CSIP mode): idle-loop re-fetching the
	//     EndDevice via LookupOwnEndDevice on dcap.PollRate until the server
	//     publishes RegistrationLink. EndDevice itself carries no pollRate
	//     (SubscribableResource only; PollRate lives on ListResource), so we
	//     reuse dcap.PollRate the same way IEEE-029 does for the EndDevice
	//     lookup idle. ctx-cancel exits the loop cleanly.
	//
	// PIN-match branches (after RegistrationLink resolves):
	//   - GET failure is FATAL — spec-mandated step, can't be skipped.
	//   - ExpectedPIN == 0: operator opted out of match check; log redacted
	//     server pIN and break.
	//   - Match: log "matches expected; proceeding" — this exact string is
	//     the CSIP "we are commissioned" signal. Downstream phases (FSA /
	//     DER control / Response Function Set) treat it as the gate. Do not
	//     reword without updating those consumers.
	//   - rg.PIN == 0 (server hasn't provisioned us yet — transient): idle
	//     and re-poll on registration.PollRate (floor 60s / default 30min
	//     when zero — IEEE-031 pattern). Honors ctx cancel.
	//   - rg.PIN != 0 && rg.PIN != cfg.ExpectedPIN (wrong device/server
	//     pair — fatal): log.Fatalf with both values redacted. Operator
	//     must intervene; idling here masks misconfiguration indefinitely.
	//
	// PIN redaction: every PIN log line goes through redactPIN, which
	// masks all but the last 2 digits (***NN). IEEE 2030.5 §8.2.1 makes the
	// last digit a check digit; trailing 2 is conventional in security UIs.
	//
	// Tests are deferred per Craig override 2026-05-12. Required coverage
	// captured in backlog (IEEE-034) and the PR body; the IEEE-028
	// pollDuration test seam was deliberately NOT extended here.
	switch {
	case edev.RegistrationLink == nil && (!cfg.CSIP || cfg.AllowUnregistered):
		log.Println("EndDevice has no RegistrationLink; skipping Phase 2b (--csip off or --allow-unregistered)")
	case edev.RegistrationLink == nil:
		// CSIP-strict: idle until the server publishes RegistrationLink.
		log.Println("=== Phase 2b: Awaiting RegistrationLink ===")
		for edev.RegistrationLink == nil {
			pollEvery := pinPollInterval(dcap.PollRate)
			log.Printf("EndDevice has no RegistrationLink (CSIP V1.2 BASIC-001 step 5 requires it); re-polling EndDevice every %s. Pass --allow-unregistered to bypass.", pollEvery)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollEvery):
			}
			if edevListHref == "" {
				log.Fatalf("EndDeviceListLink lost between polls; cannot re-lookup own EndDevice")
			}
			newEdev, err := client.LookupOwnEndDevice(ctx, edevListHref)
			if err != nil {
				log.Fatalf("re-lookup own EndDevice: %v", err)
			}
			edev = newEdev
		}
		log.Printf("RegistrationLink appeared: %s", edev.RegistrationLink.Href)
		fallthrough
	default:
		log.Println("=== Phase 2b: Registration ===")
		for {
			rg, err := client.GetRegistration(ctx, edev.RegistrationLink.Href)
			if err != nil {
				log.Fatalf("GET Registration: %v", err)
			}
			if cfg.ExpectedPIN == 0 {
				log.Printf("Registration: href=%s pIN=%s (--pin not set; skipping match check)", edev.RegistrationLink.Href, redactPIN(uint(rg.PIN)))
				break
			}
			if uint(rg.PIN) == cfg.ExpectedPIN {
				log.Printf("Registration: pIN=%s matches expected; proceeding", redactPIN(uint(rg.PIN)))
				break
			}
			if rg.PIN == 0 {
				// Server hasn't provisioned us yet; transient. Idle on registration.PollRate.
				pollEvery := pinPollInterval(rg.PollRate)
				log.Printf("Registration: server PIN not yet provisioned (pIN=%s); re-polling Registration every %s", redactPIN(uint(rg.PIN)), pollEvery)
				select {
				case <-ctx.Done():
					return
				case <-time.After(pollEvery):
				}
				continue
			}
			// Non-zero mismatch — wrong device/server pair. Operator must intervene.
			log.Fatalf("Registration: PIN mismatch — server=%s expected=%s (CSIP V1.2 BASIC-001 step 5; check --pin or device provisioning)",
				redactPIN(uint(rg.PIN)), redactPIN(cfg.ExpectedPIN))
		}
	}

	// Phase 2c: FunctionSetAssignmentsList discovery (IEEE-035 — plan-1
	// phase 4 entry). CSIP V1.2 CORE-012 step 1: after the device confirms
	// it is commissioned (Phase 2b PIN match), walk
	// EndDevice.FunctionSetAssignmentsListLink to enumerate the FSAs the
	// server has bound to it.
	//
	// IEEE-035 lands ONLY the GET + this logging-and-cache block. The walk
	// into each FSA's DERProgramListLink is IEEE-036; Primacy + mRID
	// selection is IEEE-037. The `fsaList` variable below is the seam
	// IEEE-036 consumes during its next-PR fold-in.
	//
	// Missing-FunctionSetAssignmentsListLink behavior mirrors IEEE-034's
	// missing-RegistrationLink branch:
	//   - --csip strict (default): fatal log citing CORE-012 step 1.
	//     CSIP-conformant servers MUST publish this link on a provisioned
	//     EndDevice; idling here would mask a server-side configuration
	//     bug. (IEEE-036 may revisit this once it understands aggregator
	//     topologies where the link could legitimately appear late.)
	//   - --csip=false OR --allow-unregistered=true: log bypass, proceed.
	//
	// Empty-FSAList behavior:
	//   - --csip strict: idle-loop on dcap.PollRate until at least one FSA
	//     appears. EndDevice itself carries no pollRate (SubscribableResource
	//     only), so we reuse dcap.PollRate the same way IEEE-029 and
	//     IEEE-034 do. ctx-cancel exits the loop cleanly.
	//   - --csip=false OR --allow-unregistered=true: accept empty list,
	//     proceed.
	//
	// Tests deferred per Craig override 2026-05-12. Required coverage
	// captured in backlog (IEEE-035) and the PR body.
	var fsaList sep2.FunctionSetAssignmentsList
	switch {
	case edev.FunctionSetAssignmentsListLink == nil && (!cfg.CSIP || cfg.AllowUnregistered):
		log.Println("EndDevice has no FunctionSetAssignmentsListLink; skipping Phase 2c (--csip off or --allow-unregistered)")
	case edev.FunctionSetAssignmentsListLink == nil:
		log.Fatalf("EndDevice has no FunctionSetAssignmentsListLink (CSIP V1.2 CORE-012 step 1 requires it); pass --allow-unregistered to bypass")
	default:
		log.Println("=== Phase 2c: FSAList Discovery ===")
		for {
			list, err := client.GetFSAList(ctx, edev.FunctionSetAssignmentsListLink.Href)
			if err != nil {
				log.Fatalf("GET FSAList: %v", err)
			}
			if len(list.FunctionSetAssignments) > 0 {
				fsaList = list
				log.Printf("FSAList: %d entries (paging cap 255; cursor walk deferred)", len(fsaList.FunctionSetAssignments))
				break
			}
			if cfg.CSIP && !cfg.AllowUnregistered {
				pollEvery := pinPollInterval(dcap.PollRate)
				log.Printf("FSAList empty; re-polling every %s (CSIP V1.2 CORE-012 expects >=1 FSA per provisioned device)", pollEvery)
				select {
				case <-ctx.Done():
					return
				case <-time.After(pollEvery):
				}
				continue
			}
			log.Println("FSAList empty; proceeding (--csip off or --allow-unregistered)")
			fsaList = list
			break
		}
	}
	// fsaList is the Phase 2c cache seam consumed by IEEE-036 (FSA -> DERProgram
	// tree walk, below) and IEEE-037 (Primacy + mRID selection).
	log.Printf("Phase 2c (FSAList): cached %d FSA(s)", len(fsaList.FunctionSetAssignments))

	// Phase 2c (continued, IEEE-036): walk each FSA's DERProgramListLink and
	// fetch each DERProgram's DefaultDERControl + DERControlList + DERCurveList
	// subtrees. CSIP V1.2 CORE-012 step 2 — list discovery, NOT selection.
	// Primacy + mRID selection of the highest-priority DERProgram is IEEE-037,
	// the next ticket. Control application (consuming the cache) is Phase 5.
	//
	// The cache `derProgramsByMRID` is the seam IEEE-037 consumes. Keying on
	// mRID matches the IEEE 2030.5 §10.1.3 list-ordering tie-break field; if
	// the same DERProgram is reachable from multiple FSAs the later GET wins
	// (acceptable per the spec — DERProgram resources are identified by mRID,
	// not by FSA path).
	//
	// Per-FSA missing-DERProgramListLink: skip that FSA, walk the rest.
	// Per-DERProgram missing-DefaultDERControlLink / DERControlListLink /
	// DERCurveListLink: skip that subtree GET, still record the program. Empty
	// aggregate program set across all FSAs:
	//   - --csip strict: idle-loop on dcap.PollRate (same idiom IEEE-035 uses
	//     for empty FSAList). ctx-cancel exits cleanly.
	//   - --csip off OR --allow-unregistered: log and proceed with an empty
	//     cache.
	//
	// IEEE-036 tests deferred per Craig override 2026-05-12. Required coverage
	// captured in backlog (IEEE-036) and the PR body.
	derProgramsByMRID := make(map[string]sep2.DERProgram)
	if len(fsaList.FunctionSetAssignments) > 0 {
		log.Println("=== Phase 2c: DERProgram Tree Walk ===")
		for {
			derProgramsByMRID = make(map[string]sep2.DERProgram)
			if err := walkDERProgramTree(ctx, client, fsaList, derProgramsByMRID); err != nil {
				log.Fatalf("walk DERProgram tree: %v", err)
			}
			if len(derProgramsByMRID) > 0 {
				log.Printf("Phase 2c (DERProgram walk): cached %d DERProgram(s) across %d FSA(s)",
					len(derProgramsByMRID), len(fsaList.FunctionSetAssignments))
				break
			}
			if cfg.CSIP && !cfg.AllowUnregistered {
				pollEvery := pinPollInterval(dcap.PollRate)
				log.Printf("No DERPrograms enumerated across any FSA; re-polling every %s (CSIP V1.2 CORE-012 step 2 expects >=1 DERProgram per provisioned device)", pollEvery)
				select {
				case <-ctx.Done():
					return
				case <-time.After(pollEvery):
				}
				continue
			}
			log.Println("No DERPrograms enumerated; proceeding with empty cache (--csip off or --allow-unregistered)")
			break
		}
	}
	// Phase 2c (continued, IEEE-037): apply IEEE 2030.5 §10.1.3 list-ordering
	// + CSIP V1.2 CORE-012 step 2 selection over the IEEE-036 cache. Lowest
	// Primacy wins; ties on Primacy broken by MRID lex-min. Empty cache →
	// no selection; Phase 5's ApplyControls(nil, ...) seam at line below
	// stays nil-base and falls back to its own default control.
	//
	// selectedDERProgram + selectedDefaultControlHref are the Phase 5
	// consumption seam: Phase 5 (IEEE-038+) will GET the DefaultDERControl
	// at selectedDefaultControlHref and pass its DERControlBase into
	// ApplyControls instead of the current literal nil at the call site
	// below. Until Phase 5 lands, the selection result is logged for
	// operator visibility and the existing ApplyControls(nil, ...) path
	// is preserved.
	selectedDERProgram, selected := inverter.SelectHighestPriority(derProgramsByMRID)
	var selectedDefaultControlHref string
	if selected {
		if selectedDERProgram.DefaultDERControlLink != nil {
			selectedDefaultControlHref = selectedDERProgram.DefaultDERControlLink.Href
		}
		log.Printf("Phase 2c (Primacy selection): winner mRID=%s primacy=%d DefaultDERControlLink=%q",
			selectedDERProgram.MRID, selectedDERProgram.Primacy, selectedDefaultControlHref)
	} else {
		log.Println("Phase 2c (Primacy selection): no DERProgram cached; Phase 5 will fall back to nil base")
	}
	// Suppress unused-variable warning for the default-control href — IEEE-041
	// will replace ApplyControls(nil, ...) at main.go:667 with the resolved
	// default control derived from this href.
	_ = selectedDefaultControlHref

	// Phase 5 entry (IEEE-038): start the DERControlList polling goroutine on
	// the active DERProgram's DERControlListLink. The cache surfaces added /
	// updated / cancelled events for the IEEE-039 scheduler and IEEE-040 state
	// machine that follow.
	//
	// pollRate source: dcap.PollRate. The advertised list-level pollRate lives
	// on the ListResource returned by the GET — not on the *ListLink — so we
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
	} else {
		log.Println("Phase 5 (IEEE-038): no DERControlListLink on selected program; polling skipped")
	}
	// Suppress unused-variable warning for the cache — IEEE-039+ scheduler /
	// state machine will consume it via Snapshot() and Diff().
	_ = derControlCache

	// Phase 3: DER Setup — follow EndDevice.DERListLink to find the first
	// DER, then PUT to its DERCapabilityLink / DERSettingsLink. DERStatus
	// goes through the reporter loop in Phase 5 against DERStatusLink.
	log.Println("=== Phase 3: DER Setup ===")
	var derStatusHref string
	if edev.DERListLink == nil {
		log.Println("EndDevice has no DERListLink; skipping Phase 3 DER setup")
	} else {
		var derList sep2.DERList
		if err := client.Get(ctx, edev.DERListLink.Href, &derList); err != nil {
			log.Fatalf("GET DER list %s: %v", edev.DERListLink.Href, err)
		}
		if len(derList.DER) == 0 {
			log.Println("DER list empty; skipping Phase 3 DER setup")
		} else {
			// First DER only — multi-DER inverters are a follow-up.
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
				// IEEE-031: outbound timestamp — use the server-synced clock
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

	// Phase 4: Metering Setup — POST a MirrorUsagePoint to the list href
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
			// MirrorMeterReadingListLink — we do not assume the URL.
			var mup sep2.MirrorUsagePoint
			if err := client.Get(ctx, mupLoc, &mup); err != nil {
				log.Printf("GET MirrorUsagePoint %s: %v (metering disabled)", mupLoc, err)
			} else if mup.MirrorMeterReadingListLink == nil {
				log.Println("MirrorUsagePoint has no MirrorMeterReadingListLink; metering disabled")
			} else {
				mmrHref = mup.MirrorMeterReadingListLink.Href
			}
		}
	}

	// Create reporter — empty hrefs cause the corresponding channel to be
	// a silent no-op (see reporter.go).
	reporter := inverter.NewReporter(client, derStatusHref, mmrHref)

	// Phase 5: Simulation Loop
	log.Printf("=== Phase 5: Simulation — %s ===", scenario.Name)

	simStart := time.Date(2024, 6, 21, 6, 0, 0, 0, time.UTC) // start at sunrise
	simTime := simStart
	stepIdx := 0
	currentGrid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: simTime}
	lastReport := time.Time{}

	ticker := time.NewTicker(cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			return
		case <-ticker.C:
			// Advance simulation time
			simDelta := time.Duration(float64(cfg.TickInterval) * cfg.TimeScale)
			simTime = simTime.Add(simDelta)
			currentGrid.Time = simTime

			// Check if simulation duration exceeded
			elapsed := simTime.Sub(simStart)
			if elapsed >= scenario.Duration {
				log.Printf("Scenario %s complete (duration %v)", scenario.Name, scenario.Duration)
				return
			}

			// Apply scenario steps
			for stepIdx < len(scenario.Steps) && elapsed >= scenario.Steps[stepIdx].AtTime {
				step := scenario.Steps[stepIdx]
				if step.Grid != nil {
					currentGrid.VoltsPU = step.Grid.VoltsPU
					currentGrid.FreqHz = step.Grid.FreqHz
				}
				log.Printf("[%v] %s", step.AtTime, step.Description)
				stepIdx++
			}

			// Compute power
			irr := inverter.Irradiance(simTime)
			maxP := inverter.MaxPowerW(irr)

			// Apply controls (using nil base for now — TODO: poll server for active controls)
			controls := inverter.ApplyControls(nil, currentGrid, maxP)

			// Compute output
			state := inverter.ComputeOutput(controls, currentGrid)

			// Broadcast to HMI
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
					Irradiance: irr,
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
					state.PowerFactor, state.VoltsPU, state.FreqHz, state.Mode, irr)
			}
		}
	}
}
