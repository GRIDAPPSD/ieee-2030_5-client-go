// Flag registration, extracted from main() so a test can exercise the exact
// same defaults production parses instead of a duplicate declaration that
// could drift from it silently. Fix-round-1 finding 4: flag-default tests
// that build their own FlagSet with hand-copied defaults cannot fail when
// the real default changes; only a test that calls this function can.

package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
)

// cliFlags holds the flag values registerFlags does not bind directly into
// cfg: either because main() needs the pointer itself (HMIPort,
// ListScenarios), or because the value needs post-parse validation before
// it can be written into cfg (PEN, clamped to uint32).
type cliFlags struct {
	HMIPort             *int
	ListScenarios       *bool
	PEN                 *uint64
	NotifyListen        *string
	NotifyAdvertiseHost *string
	FleetFiles          *repeatedStringFlag
	RunDir              *string
}

// repeatedStringFlag implements flag.Value so --fleet-file may be given
// more than once, one per fleet: the stdlib flag package has no repeated
// string flag of its own.
type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// registerFlags registers every inverterclient flag on fs, writing directly
// into cfg where the value needs no further validation, and returns the
// rest via cliFlags. Callers still call fs.Parse (main calls flag.Parse
// after passing flag.CommandLine; tests call fs.Parse(nil) or with their
// own args).
func registerFlags(fs *flag.FlagSet, cfg *inverter.SimConfig) *cliFlags {
	fs.StringVar(&cfg.ServerURL, "server", defaultServerURL, "IEEE 2030.5 server URL")
	fs.StringVar(&cfg.CertFile, "cert", "certs/device.crt", "Client certificate PEM")
	fs.StringVar(&cfg.KeyFile, "key", "certs/device.key", "Client private key PEM")
	fs.StringVar(&cfg.CAFile, "ca", "certs/ca.crt", "CA certificate PEM")
	fs.StringVar(&cfg.Scenario, "scenario", "normal", "Scenario name")
	fs.Float64Var(&cfg.TimeScale, "timescale", 60.0, "Simulation speed (60 = 1min real = 1hr sim)")
	fs.DurationVar(&cfg.TickInterval, "tick", 1*time.Second, "Simulation tick interval")
	fs.DurationVar(&cfg.ReportInterval, "report-interval", 10*time.Second, "Status report interval")

	// Default 0 (disabled): any number of aggregator and DER-client
	// processes of one binary may run on one host, and a fixed 8080
	// default made the second default-flagged process on a host collide.
	// Operators who want the dashboard set the port explicitly.
	hmiPort := fs.Int("hmi-port", 0, "HMI web dashboard port (0 to disable)")
	listScenarios := fs.Bool("list-scenarios", false, "List available scenarios and exit")
	fs.BoolVar(&cfg.CSIP, "csip", false, "CSIP mode: lookup own EndDevice in server's /edev list instead of POST-registering")
	fs.UintVar(&cfg.ExpectedPIN, "pin", 0, "expected Registration PIN (0 = skip match check; nonzero mismatch is fatal)")
	fs.BoolVar(&cfg.AllowUnregistered, "allow-unregistered", false, "bypass missing-RegistrationLink check in CSIP mode (dev/test only)")

	// PEN (Private Enterprise Number) stamped into every outbound LogEvent.
	// See GRIDAPPSD/ieee-2030_5-server-go#189.
	// Env var SEP2_PEN seeds the default; the CLI
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
	penFlag := fs.Uint64("pen", defaultPEN, "IANA Private Enterprise Number stamped into outbound LogEvents (env: SEP2_PEN; 0 = no manufacturer namespace)")

	// Inbound HTTPS Notification listener. Default 127.0.0.1:0 binds a
	// random local port; the subscription POST will publish whatever we
	// actually bound to. Empty string disables the
	// listener (no subscription/notification flow; fall back to polling).
	// Env var SEP2_NOTIFY_LISTEN seeds the default but the CLI flag still
	// wins per stdlib flag.Parse() precedence.
	defaultNotifyListen := os.Getenv("SEP2_NOTIFY_LISTEN")
	if defaultNotifyListen == "" {
		defaultNotifyListen = "127.0.0.1:0"
	}
	notifyListen := fs.String("notify-listen", defaultNotifyListen, "inbound HTTPS Notification listener address (env: SEP2_NOTIFY_LISTEN; empty disables)")

	// The bind address and the URL advertised in a subscription POST are
	// separate settings: a listener bound to a reachable interface (e.g.
	// 0.0.0.0 or a real host IP) may still need a different host (a DNS
	// name, a NAT'd address) in the notify URL the server calls back on.
	// Empty (default) keeps today's behavior: advertise the bound address.
	notifyAdvertiseHost := fs.String("notify-advertise-host", "", "host:port advertised in subscription notify URLs (default: the bound --notify-listen address)")

	// Backend selects the physical-state source for the tick loop. Default
	// "synthetic" preserves the existing scenario-harness behavior.
	fs.StringVar(&cfg.Backend, "backend", "synthetic", "device backend: synthetic|gridlabd|realdevice")

	// Role selects the consumer-policy role for notification dispatch.
	// Default "simulator" preserves the existing behavior. The role is
	// resolved once at construction into a concrete Dispatcher type: NOT
	// a runtime branch in the dispatch path.
	fs.StringVar(&cfg.Role, "role", "simulator", "consumer-policy role: simulator|production")

	// ClientRole selects the IEEE 2030.5 client role, resolved once at
	// start into which EndDevice(s) the process acts for. Distinct from
	// --role above (the dispatch policy); --role keeps its
	// existing meaning.
	fs.StringVar(&cfg.ClientRole, "client-role", string(guard.RoleDER), "IEEE 2030.5 client role: der|aggregator")

	// FleetFiles and RunDir are aggregator-only: a fleet file names a
	// GridLAB-D sidecar fleet (internal/sim/gridlabd) this process
	// supervises. Given in the der role, gridlabd.NewManager refuses at
	// start rather than partially running. Repeatable: one flag
	// occurrence per fleet.
	var fleetFiles repeatedStringFlag
	fs.Var(&fleetFiles, "fleet-file", "path to a fleet JSON (aggregator role only; repeatable, one per fleet)")
	runDir := fs.String("run-dir", "", "directory for fleet sidecar sockets (aggregator role, required with --fleet-file)")

	return &cliFlags{
		HMIPort:             hmiPort,
		ListScenarios:       listScenarios,
		PEN:                 penFlag,
		NotifyListen:        notifyListen,
		NotifyAdvertiseHost: notifyAdvertiseHost,
		FleetFiles:          &fleetFiles,
		RunDir:              runDir,
	}
}
