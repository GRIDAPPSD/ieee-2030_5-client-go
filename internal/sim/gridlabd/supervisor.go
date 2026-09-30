package gridlabd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// Default supervisor timing. Restart backoff matches the aggregator design
// (2 s to 60 s, at most 5 attempts per 10 minutes, then the fleet stays
// DOWN until an operator restarts the aggregator).
const (
	defaultStartTimeout  = 30 * time.Second
	defaultCallTimeout   = 10 * time.Second
	defaultStopGrace     = 5 * time.Second
	defaultBackoffMin    = 2 * time.Second
	defaultBackoffMax    = 60 * time.Second
	defaultMaxRestarts   = 5
	defaultRestartWindow = 10 * time.Minute
)

// SupervisorConfig configures one fleet's sidecar.
type SupervisorConfig struct {
	Fleet         string
	SocketPath    string   // never under /tmp; caller picks the run directory
	FleetFilePath string   // absolute path to the fleet JSON the sidecar loads
	Command       []string // interpreter plus module, e.g. {"python3","-m","gldsidecar"}; nil defaults to that

	StartTimeout time.Duration // bound on start-through-hello
	CallTimeout  time.Duration // bound on an ordinary RPC (hello, set, step_to, get)
	StopGrace    time.Duration // wait between shutdown, SIGTERM, SIGKILL

	BackoffMin    time.Duration
	BackoffMax    time.Duration
	MaxRestarts   int
	RestartWindow time.Duration

	Stderr io.Writer // sidecar stderr destination; nil discards
	Log    func(format string, args ...any)
}

func (cfg SupervisorConfig) withDefaults() SupervisorConfig {
	if cfg.Command == nil {
		cfg.Command = []string{"python3", "-m", "gldsidecar"}
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultStartTimeout
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultCallTimeout
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = defaultStopGrace
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = defaultBackoffMin
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = defaultBackoffMax
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = defaultMaxRestarts
	}
	if cfg.RestartWindow <= 0 {
		cfg.RestartWindow = defaultRestartWindow
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return cfg
}

// Supervisor starts, health-checks, restarts and stops one fleet's sidecar
// process. One Supervisor per fleet (design: "one sidecar process per
// fleet").
type Supervisor struct {
	cfg    SupervisorConfig
	launch processLauncher

	mu         sync.RWMutex
	clientConn *Client
	proc       sidecarProcess
	healthy    bool

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewSupervisor constructs a Supervisor that execs the real sidecar.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	cfg = cfg.withDefaults()
	return newSupervisor(cfg, defaultLauncher(cfg))
}

// newSupervisor applies the same defaults NewSupervisor does, so an
// injected test launcher never has to fill in every timing field itself.
func newSupervisor(cfg SupervisorConfig, launch processLauncher) *Supervisor {
	return &Supervisor{
		cfg:    cfg.withDefaults(),
		launch: launch,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func defaultLauncher(cfg SupervisorConfig) processLauncher {
	return func(ctx context.Context, args []string) (sidecarProcess, error) {
		return startExecProcess(ctx, append(append([]string{}, cfg.Command...), args...), cfg.Stderr)
	}
}

// Healthy reports whether the sidecar is currently reachable. False while
// starting, restarting, stopped, or DOWN after exhausting its restart
// budget.
func (s *Supervisor) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthy
}

// client returns the current connection, or nil when unhealthy. Every
// FleetTransport method goes through this rather than caching a *Client,
// so a device backend never holds a stale connection across a restart.
func (s *Supervisor) client() *Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.healthy {
		return nil
	}
	return s.clientConn
}

// process returns the current sidecar process handle, or nil when
// unhealthy. Used by tests that need to act on the real OS process (e.g.
// killing it) without a Signal/Kill method on Supervisor itself.
func (s *Supervisor) process() sidecarProcess {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.healthy {
		return nil
	}
	return s.proc
}

func (s *Supervisor) setState(client *Client, proc sidecarProcess, healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientConn, s.proc, s.healthy = client, proc, healthy
}

// Run starts the sidecar and supervises it until ctx is cancelled or Stop
// is called. The first start is not retried: a hello mismatch or a launch
// failure is refused outright, matching "refuses to run on a version or
// object mismatch". A crash after a successful start is restarted with
// bounded backoff; exhausting the restart budget leaves the fleet DOWN and
// returns an error.
func (s *Supervisor) Run(ctx context.Context) error {
	defer close(s.doneCh)

	proc, client, err := s.startOnce(ctx)
	if err != nil {
		return fmt.Errorf("fleet %s: initial start refused: %w", s.cfg.Fleet, err)
	}
	s.setState(client, proc, true)

	var restarts []time.Time
	for {
		select {
		case <-ctx.Done():
			s.shutdown(context.Background(), client, proc)
			return ctx.Err()
		case <-s.stopCh:
			s.shutdown(context.Background(), client, proc)
			return nil
		case werr := <-proc.Wait():
			s.setState(nil, nil, false)
			_ = client.Close()
			s.cfg.Log("fleet %s: sidecar exited (%v)", s.cfg.Fleet, werr)
		}

		// Keep attempting a restart, respecting budget and backoff, until
		// one succeeds. A failed restart attempt must never fall through
		// to the select above with a stale or nil proc/client.
		for {
			restarts = pruneRestarts(restarts, s.cfg.RestartWindow)
			if len(restarts) >= s.cfg.MaxRestarts {
				return fmt.Errorf("fleet %s: restart budget exhausted after %d attempts in %s, staying down",
					s.cfg.Fleet, len(restarts), s.cfg.RestartWindow)
			}
			delay := backoffDelay(len(restarts), s.cfg.BackoffMin, s.cfg.BackoffMax)
			restarts = append(restarts, time.Now())
			s.cfg.Log("fleet %s: restarting in %s (attempt %d/%d)", s.cfg.Fleet, delay, len(restarts), s.cfg.MaxRestarts)

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			case <-s.stopCh:
				return nil
			}

			proc, client, err = s.startOnce(ctx)
			if err != nil {
				s.cfg.Log("fleet %s: restart attempt failed: %v", s.cfg.Fleet, err)
				continue
			}
			s.setState(client, proc, true)
			break
		}
	}
}

// startOnce launches the process, waits for the socket, dials it, and
// performs the hello handshake. On any failure it tears down what it
// started and returns the error; nothing is left running.
func (s *Supervisor) startOnce(ctx context.Context) (sidecarProcess, *Client, error) {
	startCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()

	_ = os.Remove(s.cfg.SocketPath) // stale socket from an unclean prior exit; the sidecar also does this on bind

	args := []string{"--socket", s.cfg.SocketPath, "--fleet-file", s.cfg.FleetFilePath}
	proc, err := s.launch(startCtx, args)
	if err != nil {
		return nil, nil, err
	}

	client, err := waitForSocket(startCtx, s.cfg.SocketPath)
	if err != nil {
		_ = proc.Kill()
		<-proc.Wait()
		return nil, nil, fmt.Errorf("connect to fleet %s sidecar: %w", s.cfg.Fleet, err)
	}

	helloCtx, helloCancel := context.WithTimeout(startCtx, s.cfg.CallTimeout)
	hello, err := client.Hello(helloCtx)
	helloCancel()
	if err != nil {
		_ = client.Close()
		_ = proc.Kill()
		<-proc.Wait()
		return nil, nil, fmt.Errorf("fleet %s hello: %w", s.cfg.Fleet, err)
	}
	if hello.Protocol != ProtocolVersion {
		_ = client.Close()
		_ = proc.Kill()
		<-proc.Wait()
		return nil, nil, fmt.Errorf("fleet %s: sidecar speaks protocol %d, this client speaks %d", s.cfg.Fleet, hello.Protocol, ProtocolVersion)
	}
	return proc, client, nil
}

// waitForSocket polls for sockPath to appear and dials as soon as it does,
// bounded by ctx.
func waitForSocket(ctx context.Context, sockPath string) (*Client, error) {
	const pollInterval = 20 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(sockPath); err == nil {
			client, err := Dial(ctx, sockPath)
			if err == nil {
				return client, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for socket %s: %w", sockPath, ctx.Err())
		case <-ticker.C:
		}
	}
}

// shutdown asks the sidecar to stop cleanly, then escalates to SIGTERM and
// SIGKILL by PID (design: "shutdown request, wait 5 s, SIGTERM, wait 5 s,
// SIGKILL, by PID; remove the socket; confirm the child reaped").
func (s *Supervisor) shutdown(ctx context.Context, client *Client, proc sidecarProcess) {
	s.setState(nil, nil, false)
	if client != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		_ = client.Shutdown(shutdownCtx)
		cancel()
		_ = client.Close()
	}
	if proc == nil {
		_ = os.Remove(s.cfg.SocketPath)
		return
	}
	if waitOrTimeout(proc, s.cfg.StopGrace) {
		_ = os.Remove(s.cfg.SocketPath)
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	if waitOrTimeout(proc, s.cfg.StopGrace) {
		_ = os.Remove(s.cfg.SocketPath)
		return
	}
	_ = proc.Kill()
	<-proc.Wait()
	_ = os.Remove(s.cfg.SocketPath)
}

func waitOrTimeout(proc sidecarProcess, d time.Duration) bool {
	select {
	case <-proc.Wait():
		return true
	case <-time.After(d):
		return false
	}
}

// Stop ends supervision: the current sidecar is shut down (by PID) and Run
// returns. Stop blocks until Run has returned. Calling Stop more than once
// is safe.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

func pruneRestarts(restarts []time.Time, window time.Duration) []time.Time {
	cutoff := time.Now().Add(-window)
	kept := restarts[:0]
	for _, t := range restarts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// backoffDelay is 2^attempt * min, capped at max.
func backoffDelay(attempt int, min, max time.Duration) time.Duration {
	d := min
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}
