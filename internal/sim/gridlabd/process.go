package gridlabd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// sidecarProcess abstracts a running sidecar process so Supervisor's
// restart, health and stop-by-PID logic is testable without spawning a
// real OS process for every case. execProcess is the production
// implementation; tests inject a fake one.
type sidecarProcess interface {
	Pid() int
	// Wait returns a channel that receives exactly once, when the process
	// exits (nil error for a clean exit).
	Wait() <-chan error
	Signal(os.Signal) error
	Kill() error
}

// processLauncher starts a sidecar for one fleet and returns a handle to
// it. ctx bounds only the launch itself, not the process's lifetime.
type processLauncher func(ctx context.Context, args []string) (sidecarProcess, error)

type execProcess struct {
	cmd  *exec.Cmd
	done chan error
}

// startExecProcess execs command (interpreter plus args, e.g.
// {"python3","-m","gldsidecar","--socket",sock,"--fleet-file",path}),
// inheriting this process's environment. See startExecProcessWithEnv for
// the env-overriding form a test uses to add PYTHONPATH.
//
// The process's lifetime is intentionally NOT tied to ctx: ctx bounds only
// this call, and the returned process must keep running after a short
// startup context expires. Supervisor owns stopping it (Stop, by PID).
func startExecProcess(ctx context.Context, command []string, stderr io.Writer) (*execProcess, error) {
	return startExecProcessWithEnv(ctx, command, nil, stderr)
}

// startExecProcessWithEnv is startExecProcess with an explicit
// environment; env nil means inherit this process's environment (Cmd's
// own default), matching exec.Command's usual behavior. Pdeathsig is set
// on Linux so an unclean parent exit still ends the child. Stderr is
// captured to stderr (the sidecar logs there only, server.py's own
// contract), never stdout, which carries the protocol.
func startExecProcessWithEnv(_ context.Context, command []string, env []string, stderr io.Writer) (*execProcess, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("gridlabd: empty sidecar command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	cmd.Stderr = stderr
	cmd.SysProcAttr = deathSigAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sidecar %v: %w", command, err)
	}
	p := &execProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

func (p *execProcess) Pid() int                 { return p.cmd.Process.Pid }
func (p *execProcess) Wait() <-chan error       { return p.done }
func (p *execProcess) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
func (p *execProcess) Kill() error              { return p.cmd.Process.Kill() }
