package gridlabd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// waitDelayAfterExit bounds cmd.Wait() once the process itself has exited:
// without WaitDelay, Wait also blocks on the Stderr pipe reaching EOF, which
// a lingering child of the sidecar (holding the fd open after being
// reparented) can wedge indefinitely even though the sidecar we launched is
// long gone. At that point Wait is killed and its pipe read abandoned rather
// than hung forever.
const waitDelayAfterExit = 5 * time.Second

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

// execOptions is everything startExecProcess needs beyond the per-launch
// args, factored out so a test can override Env or Dir without a growing
// positional parameter list.
type execOptions struct {
	Command []string  // interpreter plus module, e.g. {"python3","-P","-m","gldsidecar"}
	Env     []string  // the sidecar's environment; nil means "inherit everything", which callers should avoid (see defaultEnv)
	Dir     string    // working directory; "" means exec.Cmd's own default (this process's cwd)
	Stderr  io.Writer // sidecar stderr destination; nil discards
}

// startExecProcess execs opts.Command plus args (e.g.
// {"--socket",sock,"--fleet-file",path}). Pdeathsig is set on Linux so an
// unclean parent exit still ends the child. Stderr is captured to stderr
// (the sidecar logs there only, server.py's own contract), never stdout,
// which carries the protocol.
//
// The process's lifetime is intentionally NOT tied to ctx: ctx bounds only
// this call, and the returned process must keep running after a short
// startup context expires. Supervisor owns stopping it (Stop, by PID).
func startExecProcess(_ context.Context, opts execOptions, args []string) (*execProcess, error) {
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("gridlabd: empty sidecar command")
	}
	full := append(append([]string{}, opts.Command...), args...)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Env = opts.Env
	cmd.Dir = opts.Dir
	cmd.Stderr = opts.Stderr
	cmd.SysProcAttr = deathSigAttr()
	cmd.WaitDelay = waitDelayAfterExit
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sidecar %v: %w", full, err)
	}
	p := &execProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

func (p *execProcess) Pid() int                 { return p.cmd.Process.Pid }
func (p *execProcess) Wait() <-chan error       { return p.done }
func (p *execProcess) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
func (p *execProcess) Kill() error              { return p.cmd.Process.Kill() }

// DefaultEnv is the sidecar's environment when the caller supplies none:
// PATH (to resolve the interpreter's own needs) and HOME (numpy/gridlabd
// may look for a writable config or cache location there), taken
// explicitly from this process's environment, not the whole of it.
// Exported so a caller building its own ManagerConfig.Env (main.go, to
// add PYTHONPATH) can extend this rather than reconstruct it.
func DefaultEnv() []string {
	var env []string
	if v, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+v)
	}
	if v, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+v)
	}
	return env
}
