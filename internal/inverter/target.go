package inverter

import "context"

type targetKey struct{}

// WithTarget returns a context whose requests the client's guard judges as
// actions for the EndDevice with the given LFDI. Without it a request is
// judged as an action for the client's own EndDevice, which is how the der
// role always acts. A manager sets it once per managed device and every
// Get, Post, Put and PostResponse made with the context carries it, so no
// request can reach the network without the guard having seen its target.
func WithTarget(ctx context.Context, lfdi string) context.Context {
	return context.WithValue(ctx, targetKey{}, lfdi)
}

// targetFrom returns the LFDI set by WithTarget, or "" for the client's own
// EndDevice.
func targetFrom(ctx context.Context) string {
	s, _ := ctx.Value(targetKey{}).(string)
	return s
}
