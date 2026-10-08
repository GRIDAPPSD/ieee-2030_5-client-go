package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// managedJSON keeps every file body valid when a layer makes the role an
// aggregator, which needs a managed device.
const managedJSON = `"managed":[{"name":"b","lfdi":"` + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + `"}]`

func fileBody(parts ...string) string {
	return "{" + strings.Join(append(parts, managedJSON), ",") + "}"
}

// Every setting resolves flag > env > file > default. Each row names the
// setting's file fragment, env value and flag argument, and the value read
// back from the resolved config at each layer.
func TestSimConfigEveryLayerWinsInOrder(t *testing.T) {
	type read func(*inverter.SimConfig, *cliFlags) string
	for _, tc := range []struct {
		key                                      string
		base                                     string // always in the file
		fileKV, env, envName, flagArg            string
		wantDefault, wantFile, wantEnv, wantFlag string
		read                                     read
	}{
		{"role", "", `"role":"aggregator"`, "der", "SEP2_CLIENT_ROLE", "--client-role=aggregator", "der", "aggregator", "der", "aggregator",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.ClientRole }},
		{"server", "", `"server":"https://file:1"`, "https://env:2", "SEP2_SERVER", "--server=https://flag:3", defaultServerURL, "https://file:1", "https://env:2", "https://flag:3",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.ServerURL }},
		{"cert", "", `"cert":"file.crt"`, "env.crt", "SEP2_CERT", "--cert=flag.crt", "certs/device.crt", "file.crt", "env.crt", "flag.crt",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.CertFile }},
		{"key", "", `"key":"file.key"`, "env.key", "SEP2_KEY", "--key=flag.key", "certs/device.key", "file.key", "env.key", "flag.key",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.KeyFile }},
		{"ca", "", `"ca":"file-ca.crt"`, "env-ca.crt", "SEP2_CA", "--ca=flag-ca.crt", "certs/ca.crt", "file-ca.crt", "env-ca.crt", "flag-ca.crt",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.CAFile }},
		{"lookup_own_edev", "", `"lookup_own_edev":false`, "true", "SEP2_LOOKUP_OWN_EDEV", "--csip=false", "true", "false", "true", "false",
			func(c *inverter.SimConfig, _ *cliFlags) string { return fmt.Sprint(c.CSIP) }},
		{"pin", "", `"pin":1111`, "2222", "SEP2_PIN", "--pin=3333", "0", "1111", "2222", "3333",
			func(c *inverter.SimConfig, _ *cliFlags) string { return fmt.Sprint(c.ExpectedPIN) }},
		{"backend", `"replay":{"file":"r.csv"}`, `"backend":"synthetic"`, "replay", "SEP2_BACKEND", "--backend=synthetic", "replay", "synthetic", "replay", "synthetic",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.Backend }},
		{"report.interval_s", "", `"report":{"interval_s":30}`, "20", "SEP2_REPORT_INTERVAL_S", "--report-interval=10s", "1m0s", "30s", "20s", "10s",
			func(c *inverter.SimConfig, _ *cliFlags) string { return c.ReportInterval.String() }},
		{"notify.listen", "", `"notify":{"listen":"127.0.0.1:1"}`, "127.0.0.1:2", "SEP2_NOTIFY_LISTEN", "--notify-listen=127.0.0.1:3", "127.0.0.1:0", "127.0.0.1:1", "127.0.0.1:2", "127.0.0.1:3",
			func(_ *inverter.SimConfig, cf *cliFlags) string { return *cf.NotifyListen }},
		{"hmi.port", "", `"hmi":{"port":9001}`, "9002", "SEP2_HMI_PORT", "--hmi-port=9003", "0", "9001", "9002", "9003",
			func(_ *inverter.SimConfig, cf *cliFlags) string { return fmt.Sprint(*cf.HMIPort) }},
	} {
		parts := func(kv string) []string {
			var p []string
			if tc.base != "" {
				p = append(p, tc.base)
			}
			if kv != "" {
				p = append(p, kv)
			}
			return p
		}
		for _, layer := range []struct {
			name string
			file string
			env  map[string]string
			args []string
			want string
		}{
			{"default", fileBody(parts("")...), nil, nil, tc.wantDefault},
			{"file", fileBody(parts(tc.fileKV)...), nil, nil, tc.wantFile},
			{"env beats file", fileBody(parts(tc.fileKV)...), map[string]string{tc.envName: tc.env}, nil, tc.wantEnv},
			{"flag beats env and file", fileBody(parts(tc.fileKV)...), map[string]string{tc.envName: tc.env}, []string{tc.flagArg}, tc.wantFlag},
		} {
			t.Run(tc.key+"/"+layer.name, func(t *testing.T) {
				c, cf, err := resolveSim(t, writeSimConfig(t, layer.file), layer.env, layer.args...)
				if err != nil {
					t.Fatal(err)
				}
				if got := tc.read(c, cf); got != layer.want {
					t.Fatalf("%s = %q, want %q", tc.key, got, layer.want)
				}
			})
		}
	}
}

// The role-dependent defaults follow the role the process finally runs in,
// not the role the file wrote.
func TestSimConfigRoleDefaultsFollowTheResolvedRole(t *testing.T) {
	aggregatorFile := fileBody(`"role":"aggregator"`)
	for _, tc := range []struct {
		name     string
		body     string
		env      map[string]string
		args     []string
		wantRole string
		wantCSIP bool
	}{
		{"empty file, --client-role=aggregator", fileBody(), nil, []string{"--client-role=aggregator"}, "aggregator", false},
		{"empty file, SEP2_CLIENT_ROLE=aggregator", fileBody(), map[string]string{"SEP2_CLIENT_ROLE": "aggregator"}, nil, "aggregator", false},
		{"aggregator file, --client-role=der keeps the CSIP registration path", aggregatorFile, nil, []string{"--client-role=der"}, "der", true},
		{"aggregator file, SEP2_CLIENT_ROLE=der keeps the CSIP registration path", aggregatorFile, map[string]string{"SEP2_CLIENT_ROLE": "der"}, nil, "der", true},
		{"aggregator file, role not overridden", aggregatorFile, nil, nil, "aggregator", false},
		{"der file, role not overridden", fileBody(), nil, nil, "der", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, err := resolveSim(t, writeSimConfig(t, tc.body), tc.env, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if c.ClientRole != tc.wantRole || c.CSIP != tc.wantCSIP {
				t.Fatalf("role=%q csip=%v, want %q %v", c.ClientRole, c.CSIP, tc.wantRole, tc.wantCSIP)
			}
			if name := derOnlyFlagUsedInAggregatorRole(*c); name != "" {
				t.Fatalf("the start would be refused naming %s, a flag the operator never passed", name)
			}
		})
	}
}

// A role chosen by a flag or env var needs the same managed devices the
// file's own role does, and the refusal names the real cause.
func TestSimConfigAggregatorByOverrideNeedsManagedDevices(t *testing.T) {
	empty := writeSimConfig(t, `{}`)
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"flag": {nil, []string{"--client-role=aggregator"}},
		"env":  {map[string]string{"SEP2_CLIENT_ROLE": "aggregator"}, nil},
	} {
		_, _, err := resolveSim(t, empty, tc.env, tc.args...)
		if err == nil || !strings.Contains(err.Error(), "managed") {
			t.Errorf("%s: err = %v, want one naming the missing managed device", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "--csip") {
			t.Errorf("%s: error %q blames --csip, a flag the operator never passed", name, err)
		}
	}
}

// Env values get the checks the file's own value gets.
func TestSimConfigEnvValuesAreValidatedLikeFileValues(t *testing.T) {
	file := writeSimConfig(t, `{}`)
	for _, tc := range []struct{ env, val string }{
		{"SEP2_HMI_PORT", "70000"},
		{"SEP2_HMI_PORT", "-1"},
		{"SEP2_CLIENT_ROLE", "boss"},
		{"SEP2_BACKEND", "gridlabd"},
		{"SEP2_BACKEND", "replay"}, // the file names no replay.file
		{"SEP2_REPORT_INTERVAL_S", "0"},
		{"SEP2_REPORT_INTERVAL_S", "abc"},
		{"SEP2_PIN", "abc"},
		{"SEP2_LOOKUP_OWN_EDEV", "maybe"},
	} {
		_, _, err := resolveSim(t, file, map[string]string{tc.env: tc.val})
		if err == nil {
			t.Errorf("%s=%s accepted", tc.env, tc.val)
			continue
		}
		if tc.env != "SEP2_BACKEND" && !strings.Contains(err.Error(), tc.env) {
			t.Errorf("%s=%s: error %q does not name the variable", tc.env, tc.val, err)
		}
	}
	for _, v := range []string{"0", "65535"} {
		if _, _, err := resolveSim(t, file, map[string]string{"SEP2_HMI_PORT": v}); err != nil {
			t.Errorf("SEP2_HMI_PORT=%s refused: %v", v, err)
		}
	}
}

// main passes through maybeApplySimConfig, so this is the gate: no path
// reads no sim-mode env var and changes no flag; a path applies or refuses.
func TestMaybeApplySimConfigIsGatedOnThePath(t *testing.T) {
	newFlags := func() (*flag.FlagSet, *inverter.SimConfig) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cfg := &inverter.SimConfig{}
		registerFlags(fs, cfg)
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		return fs, cfg
	}
	t.Run("no path: no env var is read and nothing changes", func(t *testing.T) {
		fs, cfg := newFlags()
		var read []string
		lookup := func(k string) (string, bool) {
			read = append(read, k)
			if k == simConfigEnv {
				return "", false
			}
			return "https://env:9", true
		}
		if err := maybeApplySimConfig(fs, cfg, "", lookup); err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != defaultServerURL {
			t.Errorf("ServerURL = %q, want the flag default %q", cfg.ServerURL, defaultServerURL)
		}
		if len(read) != 1 || read[0] != simConfigEnv {
			t.Errorf("env names read = %v, want only %s", read, simConfigEnv)
		}
	})
	t.Run("flag path applies", func(t *testing.T) {
		saved := inverter.Rating
		t.Cleanup(func() { inverter.Rating = saved })
		fs, cfg := newFlags()
		p := writeSimConfig(t, `{"server":"https://file:1"}`)
		if err := maybeApplySimConfig(fs, cfg, p, envOf(nil)); err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != "https://file:1" {
			t.Errorf("ServerURL = %q, want the file value", cfg.ServerURL)
		}
	})
	t.Run("env path applies", func(t *testing.T) {
		saved := inverter.Rating
		t.Cleanup(func() { inverter.Rating = saved })
		fs, cfg := newFlags()
		p := writeSimConfig(t, `{"server":"https://file:1"}`)
		if err := maybeApplySimConfig(fs, cfg, "", envOf(map[string]string{simConfigEnv: p})); err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != "https://file:1" {
			t.Errorf("ServerURL = %q, want the file value", cfg.ServerURL)
		}
	})
	t.Run("a bad file is an error", func(t *testing.T) {
		fs, cfg := newFlags()
		p := writeSimConfig(t, `{"serverr":"x"}`)
		if err := maybeApplySimConfig(fs, cfg, p, envOf(nil)); err == nil {
			t.Fatal("bad file accepted")
		}
	})
}

// The built binary refuses a bad sim config with exit 1 before it does
// anything else, by flag and by env: this covers main's call site, which no
// in-process test can reach.
func TestMainRefusesABadSimConfigAtStart(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "inverterclient-simcfg-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	bad := writeSimConfig(t, `{"serverr":"x"}`)
	for name, tc := range map[string]struct {
		args []string
		env  []string
	}{
		"flag": {[]string{"--sim-config=" + bad}, nil},
		"env":  {nil, []string{simConfigEnv + "=" + bad}},
	} {
		cmd := exec.Command(bin, tc.args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), tc.env...)
		done := make(chan struct{})
		var out []byte
		var runErr error
		go func() { out, runErr = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%s: binary did not exit", name)
		}
		if code := exitCodeOf(t, runErr, out); code != 1 {
			t.Errorf("%s: exit code %d, want 1; output:\n%s", name, code, out)
		}
		if !strings.Contains(string(out), "serverr") {
			t.Errorf("%s: output %q does not name the unknown key", name, out)
		}
	}
}
