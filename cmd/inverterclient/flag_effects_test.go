package main

// Tests that set an inverterclient flag the way the README shows and assert
// what it does, not the value it parses to (#122). Flags whose effect shows
// only on the wire are run against the built binary and a fake gotls server;
// the rest go through the production flag registration into the code the
// flag feeds.

import (
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/dispatch"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	certs "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
)

type fxReq struct{ method, path, body string }

// fxServer is a minimal IEEE 2030.5 server: one EndDevice with a
// Registration, a SubscriptionList and a DERList link, recording every
// request it gets.
type fxServer struct {
	mu   sync.Mutex
	reqs []fxReq
	pin  uint32
	lfdi string
}

func (s *fxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, fxReq{r.Method, r.URL.Path, string(body)})
	s.mu.Unlock()

	edev := sep2.EndDevice{
		LFDI:                 s.lfdi,
		RegistrationLink:     &sep2.Link{Href: "/edev/0/rg"},
		SubscriptionListLink: &sep2.ListLink{Href: "/edev/0/sub"},
		DERListLink:          &sep2.ListLink{Href: "/edev/0/der"},
		LogEventListLink:     &sep2.ListLink{Href: "/edev/0/lel"},
	}
	edev.Href = "/edev/0"
	var v any
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/dcap":
		dcap := sep2.DeviceCapability{PollRate: 1, EndDeviceListLink: &sep2.ListLink{Href: "/edev"}}
		dcap.Href = "/dcap"
		v = &dcap
	case r.Method == http.MethodGet && r.URL.Path == "/edev":
		v = &sep2.EndDeviceList{EndDevice: []sep2.EndDevice{edev}}
	case r.Method == http.MethodPost && r.URL.Path == "/edev":
		w.Header().Set("Location", "/edev/0")
		w.WriteHeader(http.StatusCreated)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/edev/0":
		v = &edev
	case r.Method == http.MethodGet && r.URL.Path == "/edev/0/rg":
		v = &sep2.Registration{PIN: s.pin}
	case r.Method == http.MethodPost && r.URL.Path == "/edev/0/lel":
		w.Header().Set("Location", "/edev/0/lel/1")
		w.WriteHeader(http.StatusCreated)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/edev/0/sub":
		w.Header().Set("Location", "/edev/0/sub/1")
		w.WriteHeader(http.StatusCreated)
		return
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/sep+xml")
	_ = xml.NewEncoder(w).Encode(v)
}

func (s *fxServer) count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (s *fxServer) body(method, path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reqs {
		if r.method == method && r.path == path {
			return r.body
		}
	}
	return ""
}

func fxLFDI(t *testing.T, certPath string) string {
	t.Helper()
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := certs.ParseCertificatePEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return sepTLS.LFDI(c)
}

// lockedBuf is a writer the process output can land in while the test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fxProc struct {
	pid  int
	out  *lockedBuf
	done chan error
}

// fxStart runs bin in an empty directory, so a relative default such as
// certs/device.crt never resolves, and kills it by PID at test end.
func fxStart(t *testing.T, bin string, args ...string) *fxProc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	out := &lockedBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	p := &fxProc{pid: cmd.Process.Pid, out: out, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(p.pid, syscall.SIGKILL) // ESRCH after exit is expected
	})
	return p
}

// exited reports whether the process ended within d, and its exit code.
func (p *fxProc) exited(t *testing.T, d time.Duration) (int, bool) {
	t.Helper()
	select {
	case err := <-p.done:
		p.done <- err
		if err == nil {
			return 0, true
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("wait: %v", err)
		}
		return ee.ExitCode(), true
	case <-time.After(d):
		return 0, false
	}
}

func fxWaitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// waitCond polls cond until it holds, the process exits, or d passes.
func (p *fxProc) waitCond(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	return fxWaitFor(d, func() bool {
		if cond() {
			return true
		}
		_, gone := p.exited(t, 0)
		return gone
	}) && cond()
}

func fxBuild(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "inverterclient-fx-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = repoRoot(t) + "/cmd/inverterclient"
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func fxFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func fxCertArgs(env *derWalkTestEnv) []string {
	return []string{"--cert=" + env.deviceCertPath, "--key=" + env.deviceKeyPath, "--ca=" + env.caCertPath}
}

// The binary refuses these at start, before any network: the exit code and
// the message are what an operator sees.
func TestFlagEffects_RefusedAtStart(t *testing.T) {
	bin := fxBuild(t)
	for _, tc := range []struct {
		name     string
		arg      string
		wantExit int
		wantOut  string
		notOut   string
	}{
		{"unknown role", "--role=bogus", 1, `unknown role "bogus" (want simulator|production)`, ""},
		{"pen over uint32", "--pen=4294967296", 1, "--pen=4294967296 exceeds uint32 max (4294967295)", ""},
		// Control: the largest valid PEN gets past the check and stops later,
		// at the missing certificates.
		{"pen at uint32 max is accepted", "--pen=4294967295", 1, "create client", "exceeds uint32 max"},
		{"unknown backend", "--backend=bogus", 1, `unknown backend "bogus"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fxStart(t, bin, tc.arg, "--notify-listen=")
			code, ok := p.exited(t, 20*time.Second)
			if !ok {
				t.Fatalf("binary still running; output:\n%s", p.out.String())
			}
			out := p.out.String()
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d; output:\n%s", code, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
			if tc.notOut != "" && strings.Contains(out, tc.notOut) {
				t.Errorf("output has %q:\n%s", tc.notOut, out)
			}
		})
	}
}

// --server, --cert, --key and --ca decide who the client talks to and as
// whom: the fake server sees /dcap only when all four are right, and each
// one swapped for a value from another CA stops the first request.
func TestFlagEffects_ServerAndCertFlags(t *testing.T) {
	bin := fxBuild(t)
	env := newDERWalkTestEnv(t)
	other := newDERWalkTestEnv(t)

	for _, tc := range []struct {
		name     string
		args     func(serverURL string) []string
		wantDcap bool
		wantOut  string // the refusal as the side that refused prints it
	}{
		{"all four set", func(u string) []string { return append([]string{"--server=" + u}, fxCertArgs(env)...) }, true, ""},
		{"cert from another CA is refused by the server", func(u string) []string {
			return []string{"--server=" + u, "--cert=" + other.deviceCertPath, "--key=" + other.deviceKeyPath, "--ca=" + env.caCertPath}
		}, false, "remote error: tls: bad certificate"},
		{"key that does not match the cert stops the client", func(u string) []string {
			return []string{"--server=" + u, "--cert=" + env.deviceCertPath, "--key=" + other.deviceKeyPath, "--ca=" + env.caCertPath}
		}, false, "private key does not match public key"},
		{"ca that did not sign the server cert is refused by the client", func(u string) []string {
			return []string{"--server=" + u, "--cert=" + env.deviceCertPath, "--key=" + env.deviceKeyPath, "--ca=" + other.caCertPath}
		}, false, "x509: certificate signed by unknown authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
			serverURL, _ := startDERWalkListener(t, env, srv)
			p := fxStart(t, bin, append(tc.args(serverURL), "--notify-listen=")...)
			if tc.wantDcap {
				if !p.waitCond(t, 15*time.Second, func() bool { return srv.count("GET", "/dcap") > 0 }) {
					t.Fatalf("server never saw GET /dcap; output:\n%s", p.out.String())
				}
				return
			}
			code, ok := p.exited(t, 20*time.Second)
			if !ok {
				t.Fatalf("binary still running; output:\n%s", p.out.String())
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1; output:\n%s", code, p.out.String())
			}
			if !strings.Contains(p.out.String(), tc.wantOut) {
				t.Errorf("output lacks %q, so the refusal came from somewhere else:\n%s", tc.wantOut, p.out.String())
			}
			if n := srv.count("GET", "/dcap"); n != 0 {
				t.Errorf("server saw %d GET /dcap, want 0", n)
			}
		})
	}
}

// --csip chooses between looking the EndDevice up (GET the list) and
// registering it (POST the list).
func TestFlagEffects_CSIPChoosesLookupOrRegister(t *testing.T) {
	bin := fxBuild(t)
	env := newDERWalkTestEnv(t)
	for _, tc := range []struct {
		name               string
		extra              []string
		wantGets, wantPost int
	}{
		{"csip", []string{"--csip"}, 1, 0},
		{"default registers", nil, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
			serverURL, _ := startDERWalkListener(t, env, srv)
			args := append([]string{"--server=" + serverURL, "--notify-listen="}, fxCertArgs(env)...)
			p := fxStart(t, bin, append(args, tc.extra...)...)
			if !p.waitCond(t, 15*time.Second, func() bool { return srv.count("GET", "/edev/0/rg") > 0 }) {
				t.Fatalf("server never saw the Registration read; output:\n%s", p.out.String())
			}
			if got := srv.count("GET", "/edev"); got != tc.wantGets {
				t.Errorf("GET /edev = %d, want %d", got, tc.wantGets)
			}
			if got := srv.count("POST", "/edev"); got != tc.wantPost {
				t.Errorf("POST /edev = %d, want %d", got, tc.wantPost)
			}
		})
	}
}

// --pin is checked against the PIN the server shows in the Registration: a
// mismatch is fatal, a match or no --pin carries on to subscribing.
func TestFlagEffects_PINMatchIsEnforced(t *testing.T) {
	bin := fxBuild(t)
	env := newDERWalkTestEnv(t)
	const serverPIN = 424242
	for _, tc := range []struct {
		name      string
		pin       string
		wantFatal bool
	}{
		{"matching pin proceeds", "--pin=424242", false},
		{"no pin proceeds", "", false},
		{"different pin is fatal", "--pin=111115", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath), pin: serverPIN}
			serverURL, _ := startDERWalkListener(t, env, srv)
			args := append([]string{"--server=" + serverURL}, fxCertArgs(env)...)
			if tc.pin != "" {
				args = append(args, tc.pin)
			}
			p := fxStart(t, bin, args...)
			if tc.wantFatal {
				code, ok := p.exited(t, 20*time.Second)
				if !ok || code != 1 {
					t.Fatalf("exited=%v code=%d, want exit 1; output:\n%s", ok, code, p.out.String())
				}
				if !strings.Contains(p.out.String(), "PIN mismatch") {
					t.Errorf("output lacks PIN mismatch:\n%s", p.out.String())
				}
				if srv.count("POST", "/edev/0/sub") != 0 {
					t.Error("client subscribed after a PIN mismatch")
				}
				return
			}
			if !p.waitCond(t, 15*time.Second, func() bool { return srv.count("POST", "/edev/0/sub") > 0 }) {
				t.Fatalf("client never got past the PIN check; output:\n%s", p.out.String())
			}
			if _, gone := p.exited(t, 0); gone {
				t.Errorf("client exited; output:\n%s", p.out.String())
			}
		})
	}
}

// --notify-listen is where the receiver binds, and --notify-advertise-host
// is what the subscription tells the server to call back on. What the
// server receives is the consumer's view of both.
func TestFlagEffects_NotifyListenAndAdvertiseHost(t *testing.T) {
	bin := fxBuild(t)
	env := newDERWalkTestEnv(t)
	for _, tc := range []struct {
		name      string
		advertise string // "" = flag not set
		wantHost  func(port int) string
	}{
		{"bound address by default", "", func(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }},
		{"bare host takes the bound port", "notify.example.test", func(port int) string { return fmt.Sprintf("notify.example.test:%d", port) }},
		{"host:port overrides both", "notify.example.test:9443", func(int) string { return "notify.example.test:9443" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
			serverURL, _ := startDERWalkListener(t, env, srv)
			port := fxFreePort(t)
			args := append([]string{"--server=" + serverURL, "--notify-listen=127.0.0.1:" + strconv.Itoa(port)}, fxCertArgs(env)...)
			if tc.advertise != "" {
				args = append(args, "--notify-advertise-host="+tc.advertise)
			}
			p := fxStart(t, bin, args...)
			if !p.waitCond(t, 15*time.Second, func() bool { return srv.count("POST", "/edev/0/sub") > 0 }) {
				t.Fatalf("no subscription POST; output:\n%s", p.out.String())
			}
			var sub sep2.Subscription
			if err := xml.Unmarshal([]byte(srv.body("POST", "/edev/0/sub")), &sub); err != nil {
				t.Fatalf("decode subscription: %v", err)
			}
			u, err := url.Parse(sub.NotificationURI)
			if err != nil {
				t.Fatalf("notification URI %q: %v", sub.NotificationURI, err)
			}
			if want := tc.wantHost(port); u.Host != want || u.Scheme != "https" || u.Path != "/notify" {
				t.Errorf("notification URI = %q, want https://%s/notify", sub.NotificationURI, want)
			}
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
			if err != nil {
				t.Errorf("nothing listening on the --notify-listen port: %v", err)
			} else {
				_ = c.Close()
			}
		})
	}

	t.Run("empty disables the receiver and the subscription", func(t *testing.T) {
		srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
		serverURL, _ := startDERWalkListener(t, env, srv)
		args := append([]string{"--server=" + serverURL, "--notify-listen="}, fxCertArgs(env)...)
		p := fxStart(t, bin, args...)
		if !p.waitCond(t, 15*time.Second, func() bool { return srv.count("GET", "/edev/0/rg") > 0 }) {
			t.Fatalf("client never registered; output:\n%s", p.out.String())
		}
		time.Sleep(1500 * time.Millisecond)
		if n := srv.count("POST", "/edev/0/sub"); n != 0 {
			t.Errorf("subscription POSTs = %d, want 0 with the receiver disabled", n)
		}
	})
}

// --hmi-port serves the dashboard on that port; the default serves nothing.
func TestFlagEffects_HMIPortServesDashboard(t *testing.T) {
	bin := fxBuild(t)
	env := newDERWalkTestEnv(t)

	t.Run("set", func(t *testing.T) {
		srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
		serverURL, _ := startDERWalkListener(t, env, srv)
		port := fxFreePort(t)
		args := append([]string{"--server=" + serverURL, "--notify-listen=", "--hmi-port=" + strconv.Itoa(port)}, fxCertArgs(env)...)
		p := fxStart(t, bin, args...)
		var status int
		ok := fxWaitFor(15*time.Second, func() bool {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			return true
		})
		if !ok || status != http.StatusOK {
			t.Fatalf("dashboard on port %d: reachable=%v status=%d; output:\n%s", port, ok, status, p.out.String())
		}
		// Ties the log line the default-off case looks for to the property: a
		// renamed line fails here instead of emptying that check.
		if !strings.Contains(p.out.String(), "HMI dashboard: http://") {
			t.Errorf("dashboard is up but the start line is missing; output:\n%s", p.out.String())
		}
	})

	t.Run("default is off", func(t *testing.T) {
		srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
		serverURL, _ := startDERWalkListener(t, env, srv)
		args := append([]string{"--server=" + serverURL, "--notify-listen="}, fxCertArgs(env)...)
		p := fxStart(t, bin, args...)
		if !p.waitCond(t, 15*time.Second, func() bool { return srv.count("GET", "/dcap") > 0 }) {
			t.Fatalf("client never reached the server; output:\n%s", p.out.String())
		}
		// Phase 5 starts after the HMI step, so reaching it shows the HMI
		// code ran and chose not to bind.
		if !p.waitCond(t, 15*time.Second, func() bool { return strings.Contains(p.out.String(), "=== Phase 5: Simulation") }) {
			t.Fatalf("client never reached the simulation loop; output:\n%s", p.out.String())
		}
		if strings.Contains(p.out.String(), "HMI dashboard") {
			t.Errorf("HMI started without --hmi-port:\n%s", p.out.String())
		}
	})
}

// parseFlagsOnly runs args through the production flag registration.
func parseFlagsOnly(t *testing.T, args ...string) *inverter.SimConfig {
	t.Helper()
	cfg := &inverter.SimConfig{}
	fs := flag.NewFlagSet("flag-effects", flag.ContinueOnError)
	registerFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cfg
}

// --backend picks the device implementation and --role the dispatcher, once.
func TestFlagEffects_BackendAndRoleSelectImplementations(t *testing.T) {
	t.Parallel()
	sc := inverter.AllScenarios()["normal"]
	for _, tc := range []struct {
		arg  string
		want func(device.DERDevice) bool
		desc string
	}{
		{"--backend=synthetic", func(d device.DERDevice) bool { _, ok := d.(*device.Synthetic); return ok }, "*device.Synthetic"},
		{"--backend=gridlabd", func(d device.DERDevice) bool { _, ok := d.(*device.GridLABD); return ok }, "*device.GridLABD"},
		{"--backend=realdevice", func(d device.DERDevice) bool {
			_, syn := d.(*device.Synthetic)
			_, gl := d.(*device.GridLABD)
			return !syn && !gl
		}, "a guarded real-device backend"},
	} {
		d, err := device.New(*parseFlagsOnly(t, tc.arg), sc)
		if err != nil {
			t.Errorf("%s: %v", tc.arg, err)
			continue
		}
		if !tc.want(d) {
			t.Errorf("%s built %T, want %s", tc.arg, d, tc.desc)
		}
	}

	for _, tc := range []struct {
		args    []string
		prod    bool
		warning bool
	}{
		{nil, false, false},
		{[]string{"--role=simulator"}, false, false},
		{[]string{"--role=production"}, true, true},
		{[]string{"--role=production", "--backend=realdevice"}, true, false},
	} {
		cfg := parseFlagsOnly(t, tc.args...)
		d, err := dispatch.New(*cfg)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		_, isProd := d.(*dispatch.ProductionDispatcher)
		if isProd != tc.prod {
			t.Errorf("%v built %T, production=%v want %v", tc.args, d, isProd, tc.prod)
		}
		if warned, _ := dispatch.CheckRoleBackendCoherence(cfg.Role, cfg.Backend); warned != tc.warning {
			t.Errorf("%v: dry-run warning = %v, want %v", tc.args, warned, tc.warning)
		}
	}
}

// --timescale and --tick set how far the synthetic clock moves per tick:
// the default is 1 s x 60, and the flags change it.
func TestFlagEffects_TimescaleAndTickSetSimStep(t *testing.T) {
	t.Parallel()
	sc := inverter.AllScenarios()["normal"]
	for _, tc := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"defaults", nil, 60 * time.Second},
		// Each default is pinned by setting only the other flag.
		{"timescale only, default tick is 1s", []string{"--timescale=10"}, 10 * time.Second},
		{"tick only, default timescale is 60", []string{"--tick=2s"}, 120 * time.Second},
		{"both set", []string{"--timescale=10", "--tick=2s"}, 20 * time.Second},
	} {
		cfg := parseFlagsOnly(t, tc.args...)
		d, err := device.New(*cfg, sc)
		if err != nil {
			t.Fatal(err)
		}
		syn := d.(*device.Synthetic)
		before := syn.SimTime()
		if _, err := syn.ReadState(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := syn.SimTime().Sub(before); got != tc.want {
			t.Errorf("%s: sim time moved %v per tick, want %v", tc.name, got, tc.want)
		}
	}
}

// replay.clock decides which row of the recording the first read returns:
// "start" begins at row 0, "wall" follows the UTC minute of day.
func TestFlagEffects_ReplayClockPicksTheRow(t *testing.T) {
	// Row m holds m+1 W, so a read names the row it came from: "start" is
	// 1 W and "wall" is the UTC minute of day plus one.
	var b strings.Builder
	for m := 0; m < 1440; m++ {
		fmt.Fprintf(&b, "2020-01-01 %02d:%02d:00 UTC,%d\n", m/60, m%60, m+1)
	}
	rec := filepath.Join(t.TempDir(), "rec.csv")
	if err := os.WriteFile(rec, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if time.Now().UTC().Hour() == 0 && time.Now().UTC().Minute() == 0 {
		t.Skip("00:00 UTC makes wall and start read the same row")
	}

	minuteRow := func(now time.Time) float64 { return float64(now.Hour()*60 + now.Minute() + 1) }
	for _, clock := range []string{"start", "wall"} {
		t.Run(clock, func(t *testing.T) {
			body := fmt.Sprintf(`{"backend":"replay","device":{"type":"pv","rated_w":5000},"replay":{"file":%q,"clock":%q,"scale":1}}`, rec, clock)
			cfg, _, err := resolveSim(t, writeSimConfig(t, body), nil)
			if err != nil {
				t.Fatal(err)
			}
			d, err := device.New(*cfg, inverter.Scenario{})
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC()
			st, err := d.ReadState(t.Context())
			after := time.Now().UTC()
			if err != nil {
				t.Fatal(err)
			}
			// A read that straddles a minute boundary may name either row.
			want := []float64{1}
			if clock == "wall" {
				want = []float64{minuteRow(before), minuteRow(after)}
			}
			if st.MaxPowerW != want[0] && st.MaxPowerW != want[len(want)-1] {
				t.Errorf("clock=%s: first read = %v W, want one of %v W", clock, st.MaxPowerW, want)
			}
		})
	}
}

// --pen is stamped into every LogEvent the client posts: the alarm the
// enterservice scenario raises on its first tick reaches the server with it.
func TestFlagEffects_PENIsStampedOnPostedLogEvents(t *testing.T) {
	bin := fxBuild(t)
	env := newDERWalkTestEnv(t)
	srv := &fxServer{lfdi: fxLFDI(t, env.deviceCertPath)}
	serverURL, _ := startDERWalkListener(t, env, srv)
	args := append([]string{"--server=" + serverURL, "--notify-listen=", "--scenario=enterservice", "--tick=50ms", "--timescale=600", "--pen=40732"}, fxCertArgs(env)...)
	p := fxStart(t, bin, args...)
	if !p.waitCond(t, 20*time.Second, func() bool { return srv.count("POST", "/edev/0/lel") > 0 }) {
		t.Fatalf("no LogEvent POST; output:\n%s", p.out.String())
	}
	var evt sep2.LogEvent
	if err := xml.Unmarshal([]byte(srv.body("POST", "/edev/0/lel")), &evt); err != nil {
		t.Fatalf("decode LogEvent: %v", err)
	}
	if evt.LogEventPEN != 40732 {
		t.Errorf("LogEventPEN = %d, want 40732", evt.LogEventPEN)
	}
}
