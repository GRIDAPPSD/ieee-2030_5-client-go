package guard

import "context"

// Transport is the minimal request surface a Guarded wraps. Its methods
// mirror inverter.SEP2Client's Get/Post/Put signatures so a production
// caller can adapt the real client with no translation code.
type Transport interface {
	Get(ctx context.Context, path string, out any) (newHref string, err error)
	Post(ctx context.Context, path string, body any) (location string, newHref string, err error)
	Put(ctx context.Context, path string, body any) (newHref string, err error)
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

func (g *Guarded) Get(ctx context.Context, a Action, path string, out any) (string, error) {
	if err := g.guard.Allow(a); err != nil {
		return "", err
	}
	return g.transport.Get(ctx, path, out)
}

func (g *Guarded) Post(ctx context.Context, a Action, path string, body any) (string, string, error) {
	if err := g.guard.Allow(a); err != nil {
		return "", "", err
	}
	return g.transport.Post(ctx, path, body)
}

func (g *Guarded) Put(ctx context.Context, a Action, path string, body any) (string, error) {
	if err := g.guard.Allow(a); err != nil {
		return "", err
	}
	return g.transport.Put(ctx, path, body)
}
