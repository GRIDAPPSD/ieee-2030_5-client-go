// Sim mode: one JSON file holds every sim setting for the process. A
// setting resolves flag > env > file > default, where "default" is the
// config schema's default when a file is loaded. With no file the flags
// behave exactly as they always have: none of the env names below is read.

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
)

// simConfigEnv names the env var that stands in for --sim-config.
const simConfigEnv = "SEP2_SIM_CONFIG"

// simSetting binds one config key to the flag it overrides and the env var
// that sits between the flag and the file (an empty env var counts as unset,
// as SEP2_NOTIFY_LISTEN already does). value returns the file's value
// as the flag would be given it, and ok=false when the file leaves the
// flag at its own default.
type simSetting struct {
	key   string
	flag  string
	env   string
	value func(f *simconfig.File, role string) (string, bool)
	// envCheck applies to an env value the checks the file's own value
	// passes in simconfig; nil means the flag's parser is the only check.
	envCheck func(string) error
	// envValue converts the env var's text to the flag's; nil passes it
	// through. report.interval_s is in seconds, the flag takes a duration.
	envValue func(string) (string, error)
}

func oneOfEnv(allowed ...string) func(string) error {
	return func(v string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("%q must be one of %s", v, strings.Join(allowed, ", "))
	}
}

// portEnv holds an env port to the range hmi.port is held to in the file.
func portEnv(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("want a port number: %w", err)
	}
	if n < 0 || n > 65535 {
		return fmt.Errorf("%d must be within 0 to 65535", n)
	}
	return nil
}

func always(s string) (string, bool) { return s, true }

func setIf(s string) (string, bool) { return s, s != "" }

var simSettings = []simSetting{
	{key: "role", flag: "client-role", env: "SEP2_CLIENT_ROLE", envCheck: oneOfEnv("der", "aggregator"), value: func(f *simconfig.File, _ string) (string, bool) { return always(f.Role) }},
	{key: "server", flag: "server", env: "SEP2_SERVER", value: func(f *simconfig.File, _ string) (string, bool) { return setIf(f.Server) }},
	{key: "cert", flag: "cert", env: "SEP2_CERT", value: func(f *simconfig.File, _ string) (string, bool) { return setIf(f.Cert) }},
	{key: "key", flag: "key", env: "SEP2_KEY", value: func(f *simconfig.File, _ string) (string, bool) { return setIf(f.Key) }},
	{key: "ca", flag: "ca", env: "SEP2_CA", value: func(f *simconfig.File, _ string) (string, bool) { return setIf(f.CA) }},
	// The default depends on the role the process finally runs in, which a
	// flag or env var may have changed from the file's; applySimConfig
	// sets the role first and passes it here.
	{key: "lookup_own_edev", flag: "csip", env: "SEP2_LOOKUP_OWN_EDEV", value: func(f *simconfig.File, role string) (string, bool) {
		v, set := f.LookupFor(role)
		return strconv.FormatBool(v), set
	}},
	{key: "pin", flag: "pin", env: "SEP2_PIN", value: func(f *simconfig.File, _ string) (string, bool) { return always(strconv.FormatUint(uint64(f.Pin), 10)) }},
	{key: "backend", flag: "backend", env: "SEP2_BACKEND", envCheck: oneOfEnv("replay", "synthetic"), value: func(f *simconfig.File, _ string) (string, bool) { return always(f.Backend) }},
	{key: "report.interval_s", flag: "report-interval", env: "SEP2_REPORT_INTERVAL_S",
		value: func(f *simconfig.File, _ string) (string, bool) {
			return always((time.Duration(f.Report.IntervalS) * time.Second).String())
		},
		envValue: func(v string) (string, error) {
			n, err := strconv.ParseUint(v, 10, 31)
			if err != nil {
				return "", fmt.Errorf("want whole seconds: %w", err)
			}
			if n == 0 {
				return "", errors.New("must be positive, as report.interval_s is")
			}
			return (time.Duration(n) * time.Second).String(), nil
		}},
	{key: "notify.listen", flag: "notify-listen", env: "SEP2_NOTIFY_LISTEN", value: func(f *simconfig.File, _ string) (string, bool) {
		if f.Notify.Listen == nil {
			return "", false
		}
		return *f.Notify.Listen, true
	}},
	{key: "hmi.port", flag: "hmi-port", env: "SEP2_HMI_PORT", envCheck: portEnv, value: func(f *simconfig.File, _ string) (string, bool) { return always(strconv.Itoa(f.HMI.Port)) }},
}

// simConfigPath returns the sim config path: the flag wins over the env
// var; "" means sim mode is off.
func simConfigPath(flagValue string, lookupEnv func(string) (string, bool)) string {
	if flagValue != "" {
		return flagValue
	}
	v, _ := lookupEnv(simConfigEnv)
	return v
}

// applySimConfig loads path and writes each setting that neither an
// explicit flag nor its env var already decided, then fills the replay and
// nameplate settings. fs must already be parsed so fs.Visit reports which
// flags the operator set.
func applySimConfig(fs *flag.FlagSet, cfg *inverter.SimConfig, path string, lookupEnv func(string) (string, bool)) (*simconfig.File, error) {
	sc, err := simconfig.Load(path)
	if err != nil {
		return nil, err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for _, s := range simSettings {
		if explicit[s.flag] {
			continue
		}
		v, from := "", ""
		if ev, ok := lookupEnv(s.env); ok && ev != "" {
			from = "env " + s.env
			v = ev
			if s.envCheck != nil {
				if cerr := s.envCheck(ev); cerr != nil {
					return nil, fmt.Errorf("%s: %w", from, cerr)
				}
			}
			if s.envValue != nil {
				var cerr error
				if v, cerr = s.envValue(ev); cerr != nil {
					return nil, fmt.Errorf("%s: %w", from, cerr)
				}
			}
		} else if fv, ok := s.value(sc, cfg.ClientRole); ok {
			v, from = fv, "sim config key "+s.key
		} else {
			continue
		}
		if err := fs.Set(s.flag, v); err != nil {
			return nil, fmt.Errorf("%s: %w", from, err)
		}
	}
	if err := sc.ValidateForRole(cfg.ClientRole); err != nil {
		return nil, err
	}
	if cfg.Backend == "replay" && sc.Replay.File == "" {
		return nil, errors.New("backend \"replay\" needs replay.file in the sim config")
	}
	cfg.Replay = inverter.ReplaySettings{
		File: sc.Replay.File, Type: sc.Device.Type, Clock: sc.Replay.Clock, Scale: sc.Replay.Scale,
		RatedW: sc.Device.RatedW, CapacityWh: sc.Device.CapacityWh, InitialSOC: *sc.Device.InitialSOC,
	}
	applyNameplate(sc.Device.RatedW)
	return sc, nil
}

// applyNameplate sets the process-wide nameplate from the config's rated
// power, keeping the VA and VAr ratios of the built-in 10 kW rating
// (11.1 kVA and the 44 percent reactive capability of IEEE 1547 Table 7).
func applyNameplate(ratedW float64) {
	inverter.Rating.RatedW = ratedW
	inverter.Rating.RatedVA = ratedW * 111 / 100
	inverter.Rating.RatedVAr = ratedW * 44 / 100
}

// maybeApplySimConfig is the one gate main passes through: with no sim
// config path every flag behaves as it always has and no sim-mode env var
// is read; with one, the config is applied or the start is refused.
func maybeApplySimConfig(fs *flag.FlagSet, cfg *inverter.SimConfig, flagPath string, lookupEnv func(string) (string, bool)) error {
	path := simConfigPath(flagPath, lookupEnv)
	if path == "" {
		return nil
	}
	_, err := applySimConfig(fs, cfg, path, lookupEnv)
	return err
}

// osLookupEnv is the production env source.
func osLookupEnv(k string) (string, bool) { return os.LookupEnv(k) }
