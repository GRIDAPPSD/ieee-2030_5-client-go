package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
)

func writeSimConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "solo-pv-1.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// resolveSim parses args through the production flag registration, then
// applies the sim config at path, and returns the resulting config.
// applySimConfig writes the process-wide inverter.Rating, so every caller
// runs serially (no t.Parallel) and the rating is restored afterwards.
func resolveSim(t *testing.T, path string, env map[string]string, args ...string) (*inverter.SimConfig, *cliFlags, error) {
	t.Helper()
	saved := inverter.Rating
	t.Cleanup(func() { inverter.Rating = saved })
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg := &inverter.SimConfig{}
	cf := registerFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err := applySimConfig(fs, cfg, path, envOf(env))
	return cfg, cf, err
}

func TestSimConfigPrecedenceFlagEnvFileDefault(t *testing.T) {
	file := writeSimConfig(t, `{"server":"https://file:1","report":{"interval_s":30},"hmi":{"port":9001}}`)
	empty := writeSimConfig(t, `{}`)
	env := map[string]string{"SEP2_SERVER": "https://env:2", "SEP2_REPORT_INTERVAL_S": "20", "SEP2_HMI_PORT": "9002"}
	flags := []string{"--server=https://flag:3", "--report-interval=10s", "--hmi-port=9003"}

	type got struct {
		server string
		report time.Duration
		hmi    int
	}
	read := func(c *inverter.SimConfig, cf *cliFlags) got {
		return got{c.ServerURL, c.ReportInterval, *cf.HMIPort}
	}
	for _, tc := range []struct {
		name string
		path string
		env  map[string]string
		args []string
		want got
	}{
		{"flag beats env and file", file, env, flags, got{"https://flag:3", 10 * time.Second, 9003}},
		{"env beats file", file, env, nil, got{"https://env:2", 20 * time.Second, 9002}},
		{"file beats default", file, nil, nil, got{"https://file:1", 30 * time.Second, 9001}},
		{"default: server keeps the flag default, report interval takes the schema default of 60 s", empty, nil, nil, got{defaultServerURL, 60 * time.Second, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cf, err := resolveSim(t, tc.path, tc.env, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if g := read(c, cf); g != tc.want {
				t.Fatalf("got %+v, want %+v", g, tc.want)
			}
		})
	}
}

func TestSimConfigMapsRoleBackendAndLookup(t *testing.T) {
	c, _, err := resolveSim(t, writeSimConfig(t, `{"replay":{"file":"rec.csv"},"pin":4242}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientRole != "der" || c.Backend != "replay" || !c.CSIP || c.ExpectedPIN != 4242 {
		t.Fatalf("role=%q backend=%q csip=%v pin=%d, want der replay true 4242", c.ClientRole, c.Backend, c.CSIP, c.ExpectedPIN)
	}
	agg := `{"role":"aggregator","backend":"synthetic","managed":[{"name":"b","lfdi":"` + strings.Repeat("A", 40) + `"}]}`
	c, _, err = resolveSim(t, writeSimConfig(t, agg), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientRole != "aggregator" || c.CSIP || c.Backend != "synthetic" {
		t.Fatalf("role=%q csip=%v backend=%q, want aggregator false synthetic", c.ClientRole, c.CSIP, c.Backend)
	}
}

func TestSimConfigEmptyEnvCountsAsUnset(t *testing.T) {
	c, _, err := resolveSim(t, writeSimConfig(t, `{"server":"https://file:1"}`), map[string]string{"SEP2_SERVER": ""})
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerURL != "https://file:1" {
		t.Fatalf("server = %q, want the file value", c.ServerURL)
	}
}

func TestSimConfigBadEnvValueNamesTheVariable(t *testing.T) {
	_, _, err := resolveSim(t, writeSimConfig(t, `{}`), map[string]string{"SEP2_HMI_PORT": "abc"})
	if err == nil || !strings.Contains(err.Error(), "SEP2_HMI_PORT") {
		t.Fatalf("err = %v, want one naming SEP2_HMI_PORT", err)
	}
}

func TestSimConfigUnknownKeyStopsTheStartNamingIt(t *testing.T) {
	for name, body := range map[string]string{
		"top level": `{"serverr":"x"}`,
		"nested":    `{"device":{"rated_kw":5}}`,
	} {
		_, _, err := resolveSim(t, writeSimConfig(t, body), nil)
		if err == nil {
			t.Fatalf("%s: unknown key accepted", name)
		}
		key := "serverr"
		if name == "nested" {
			key = "rated_kw"
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error %q does not name %q", name, err, key)
		}
	}
}

func TestSimConfigNameplateAndReplaySettings(t *testing.T) {
	saved := inverter.Rating
	t.Cleanup(func() { inverter.Rating = saved })
	body := `{"device":{"type":"battery","rated_w":5000,"capacity_wh":12000,"initial_soc":0.25},"replay":{"file":"/r.csv","clock":"start","scale":2}}`
	c, _, err := resolveSim(t, writeSimConfig(t, body), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := inverter.ReplaySettings{File: "/r.csv", Type: "battery", Clock: "start", Scale: 2, RatedW: 5000, CapacityWh: 12000, InitialSOC: 0.25}
	if c.Replay != want {
		t.Fatalf("Replay = %+v, want %+v", c.Replay, want)
	}
	if inverter.Rating.RatedW != 5000 || inverter.Rating.RatedVA != 5550 || inverter.Rating.RatedVAr != 2200 {
		t.Fatalf("Rating = %+v, want 5000 W, 5550 VA, 2200 VAr", inverter.Rating)
	}
}

func TestSimConfigPathFlagBeatsEnv(t *testing.T) {
	t.Parallel()
	env := envOf(map[string]string{"SEP2_SIM_CONFIG": "/env.json"})
	if got := simConfigPath("/flag.json", env); got != "/flag.json" {
		t.Errorf("flag+env = %q, want /flag.json", got)
	}
	if got := simConfigPath("", env); got != "/env.json" {
		t.Errorf("env only = %q, want /env.json", got)
	}
	if got := simConfigPath("", envOf(nil)); got != "" {
		t.Errorf("neither = %q, want empty (sim mode off)", got)
	}
}

func TestSimConfigFlagDefaultsUnchangedWithoutAFile(t *testing.T) {
	t.Parallel()
	cfg, cf := newTestFlagSet(t)
	if *cf.SimConfigPath != "" {
		t.Fatalf("--sim-config default = %q, want empty", *cf.SimConfigPath)
	}
	if cfg.Backend != "synthetic" || cfg.ReportInterval != 10*time.Second || cfg.ServerURL != defaultServerURL || cfg.ClientRole != "der" {
		t.Fatalf("defaults moved: backend=%q report=%v server=%q role=%q", cfg.Backend, cfg.ReportInterval, cfg.ServerURL, cfg.ClientRole)
	}
}

// The shipped example config must load, resolve its recording beside
// itself, and build a working replay device through device.New.
func TestShippedExampleSimConfigBuildsAReplayDevice(t *testing.T) {
	saved := inverter.Rating
	t.Cleanup(func() { inverter.Rating = saved })
	example := filepath.Join("..", "..", "sim", "derms", "solo-pv-1.example.json")
	c, _, err := resolveSim(t, example, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != "replay" || c.Replay.Type != "pv" || c.Replay.RatedW != 3000 {
		t.Fatalf("backend=%q replay=%+v", c.Backend, c.Replay)
	}
	dev, err := device.New(*c, inverter.Scenario{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := dev.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxPowerW < 0 || got.MaxPowerW > 3000 {
		t.Fatalf("MaxPowerW = %v, want within 0 to 3000 W", got.MaxPowerW)
	}
}
