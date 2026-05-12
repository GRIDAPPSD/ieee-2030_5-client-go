package main

import (
	"context"
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

	// Phase 2: Registration
	log.Println("=== Phase 2: Registration ===")
	edev, err := client.Register(ctx)
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	edevID := extractID(edev.Href)
	derID := "1" // default DER ID
	log.Printf("Registered: href=%s SFDI=%s", edev.Href, edev.SFDI)

	// Phase 3: DER Setup
	log.Println("=== Phase 3: DER Setup ===")
	maxW := sep2.ActivePower{Value: int64(inverter.Rating.RatedW)}
	maxVAr := sep2.ReactivePower{Value: int64(inverter.Rating.RatedVAr)}
	modesSupported := uint32(0xFF) // all modes
	derType := uint8(4)           // PV inverter

	if err := client.PutDERCapability(ctx, edevID, derID, sep2.DERCapability{
		RTGMaxW:        &maxW,
		RTGMaxVar:      &maxVAr,
		ModesSupported: &modesSupported,
		Type:           &derType,
	}); err != nil {
		log.Printf("PUT DERCapability: %v (continuing)", err)
	}

	setMaxW := sep2.ActivePower{Value: int64(inverter.Rating.RatedW)}
	if err := client.PutDERSettings(ctx, edevID, derID, sep2.DERSettings{
		SetMaxW:     &setMaxW,
		UpdatedTime: time.Now().Unix(),
	}); err != nil {
		log.Printf("PUT DERSettings: %v (continuing)", err)
	}
	log.Println("DER capability and settings reported")

	// Phase 4: Metering Setup
	log.Println("=== Phase 4: Metering Setup ===")
	mupHref, err := client.CreateMirrorUsagePoint(ctx, sep2.MirrorUsagePoint{
		MRID:                "mup-" + client.SFDI()[:8],
		Description:         "PV Inverter Metering",
		ServiceCategoryKind: 0,
		Status:              1,
	})
	if err != nil {
		log.Printf("create MirrorUsagePoint: %v (metering disabled)", err)
	} else {
		log.Printf("MirrorUsagePoint: %s", mupHref)
	}

	// Create reporter
	reporter := inverter.NewReporter(client, edevID, derID, extractID(mupHref))

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

func extractID(href string) string {
	if href == "" {
		return ""
	}
	// Extract last path segment: "/mup/abc" → "abc", "/edev/xyz" → "xyz"
	for i := len(href) - 1; i >= 0; i-- {
		if href[i] == '/' {
			return href[i+1:]
		}
	}
	return href
}
