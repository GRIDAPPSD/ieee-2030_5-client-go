package guard

import (
	"context"
	"fmt"
	"net/http"
)

// Transport is the minimal request surface a Guarded wraps. Its methods
// mirror inverter.SEP2Client's Get/Post/Put signatures so a production
// caller can adapt the real client with no translation code.
type Transport interface {
	Get(ctx context.Context, path string, out any) (newHref string, err error)
	Post(ctx context.Context, path string, body any) (location string, newHref string, err error)
	Put(ctx context.Context, path string, body any) (newHref string, err error)
}

// LFDIBearing is implemented by a request body that names the EndDevice it
// concerns (a MirrorUsagePoint's DeviceLFDI, a Response's endDeviceLFDI).
// When a caller's body implements it, Guarded refuses a mismatch between
// the body's own LFDI and the Action's TargetLFDI: a caller label may
// narrow, but the body can never widen it past what was declared. An empty
// ActingForLFDI() is read as "no signal" and is not checked.
type LFDIBearing interface {
	ActingForLFDI() string
}

// Guarded wraps a Transport so every call is classified by a Guard before
// it reaches the network. A refused Action returns the guard's error and
// never calls the underlying Transport: nothing is sent.
type Guarded struct {
	transport Transport
	guard     *Guard
}

// NewGuarded builds a Guarded transport that checks every call against
// guard before delegating to transport.
func NewGuarded(transport Transport, guard *Guard) *Guarded {
	return &Guarded{transport: transport, guard: guard}
}

// bodyLFDIMismatch reports whether body implements LFDIBearing and names
// an LFDI that disagrees with the action's effective target (TargetLFDI,
// or the empty-string self convention). It never widens what Allow already
// decided; it only catches a body silently naming a different device than
// the Action declared.
func bodyLFDIMismatch(body any, a Action) bool {
	bearing, ok := body.(LFDIBearing)
	if !ok {
		return false
	}
	bodyLFDI := bearing.ActingForLFDI()
	return bodyLFDI != "" && bodyLFDI != a.TargetLFDI
}

func (g *Guarded) Get(ctx context.Context, a Action, path string, out any) (string, error) {
	if err := g.guard.Allow(http.MethodGet, a); err != nil {
		return "", err
	}
	return g.transport.Get(ctx, path, out)
}

func (g *Guarded) Post(ctx context.Context, a Action, path string, body any) (string, string, error) {
	if err := g.guard.Allow(http.MethodPost, a); err != nil {
		return "", "", err
	}
	if bodyLFDIMismatch(body, a) {
		return "", "", fmt.Errorf("guard: POST body names a different EndDevice than the declared target %q", a.TargetLFDI)
	}
	return g.transport.Post(ctx, path, body)
}

func (g *Guarded) Put(ctx context.Context, a Action, path string, body any) (string, error) {
	if err := g.guard.Allow(http.MethodPut, a); err != nil {
		return "", err
	}
	if bodyLFDIMismatch(body, a) {
		return "", fmt.Errorf("guard: PUT body names a different EndDevice than the declared target %q", a.TargetLFDI)
	}
	return g.transport.Put(ctx, path, body)
}
