// Tests for the inverterclient parameters the README documents: --scenario,
// --list-scenarios, --help and SEP2_PEN. The first three run the compiled
// binary, since the refusals and the exit codes live in main().

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

var (
	cliBinOnce sync.Once
	cliBinPath string
	cliBinErr  error
)

// cliBinary builds the binary once per test run and shares it.
func cliBinary(t *testing.T) string {
	t.Helper()
	cliBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "inverterclient-cli")
		if err != nil {
			cliBinErr = err
			return
		}
		cliBinPath = filepath.Join(dir, "inverterclient")
		if out, err := exec.Command("go", "build", "-o", cliBinPath, ".").CombinedOutput(); err != nil {
			cliBinErr = errors.New("go build: " + err.Error() + "\n" + string(out))
		}
	})
	if cliBinErr != nil {
		t.Fatal(cliBinErr)
	}
	return cliBinPath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if cliBinPath != "" {
		_ = os.RemoveAll(filepath.Dir(cliBinPath))
	}
	os.Exit(code)
}

// runCLI runs the binary from an empty directory, so no certs/ resolve.
func runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliBinary(t), args...)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), ee.ExitCode()
}

func TestScenarioFlagRefusesAnUnknownName(t *testing.T) {
	t.Parallel()
	out, code := runCLI(t, "--scenario", "nosuch")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "unknown scenario: nosuch") {
		t.Fatalf("output lacks the refusal:\n%s", out)
	}
}

// A valid name passes the lookup. Run without certs it stops later, at
// device or client setup, so the proof is that the refusal is absent and
// the process got past the lookup.
func TestScenarioFlagAcceptsEveryKnownName(t *testing.T) {
	t.Parallel()
	for name := range inverter.AllScenarios() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, _ := runCLI(t, "--scenario", name, "--notify-listen=")
			if strings.Contains(out, "unknown scenario") {
				t.Fatalf("--scenario %s refused:\n%s", name, out)
			}
		})
	}
}

func TestListScenariosPrintsEveryNameAndExitsZero(t *testing.T) {
	t.Parallel()
	out, code := runCLI(t, "--list-scenarios")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out)
	}
	all := inverter.AllScenarios()
	if len(all) == 0 {
		t.Fatal("AllScenarios is empty; the check below would pass vacuously")
	}
	for name, s := range all {
		if !strings.Contains(out, "  "+name+" ") {
			t.Errorf("--list-scenarios omits %q:\n%s", name, out)
		}
		if !strings.Contains(out, s.Description) {
			t.Errorf("--list-scenarios omits the description of %q", name)
		}
	}
}

func TestHelpListsEveryRegisteredFlagAndExitsZero(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("names", flag.ContinueOnError)
	registerFlags(fs, &inverter.SimConfig{})
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no flags registered; the check below would pass vacuously")
	}

	for _, helpFlag := range []string{"--help", "-h"} {
		out, code := runCLI(t, helpFlag)
		if code != 0 {
			t.Fatalf("%s exit = %d, want 0; output:\n%s", helpFlag, code, out)
		}
		for _, n := range names {
			if !strings.Contains(out, "\n  -"+n) {
				t.Errorf("%s output omits flag -%s", helpFlag, n)
			}
		}
	}
}

func TestPENFromEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want uint64
	}{
		{"unset gives zero", "", nil, 0},
		{"valid value seeds the default", "40732", nil, 40732},
		{"uint32 max is accepted", "4294967295", nil, 4294967295},
		{"non-numeric falls back to zero", "abc", nil, 0},
		{"above uint32 falls back to zero", "4294967296", nil, 0},
		{"negative falls back to zero", "-1", nil, 0},
		{"flag wins over the environment", "40732", []string{"--pen", "7"}, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEP2_PEN", tc.env)
			fs := flag.NewFlagSet("pen", flag.ContinueOnError)
			cf := registerFlags(fs, &inverter.SimConfig{})
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if *cf.PEN != tc.want {
				t.Fatalf("PEN = %d, want %d", *cf.PEN, tc.want)
			}
		})
	}
}
