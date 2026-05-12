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
