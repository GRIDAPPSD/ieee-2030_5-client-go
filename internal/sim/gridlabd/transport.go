package gridlabd

import (
	"context"
	"fmt"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
)

var _ device.FleetTransport = (*supervisorTransport)(nil)

// supervisorTransport implements device.FleetTransport over a Supervisor's
// current sidecar connection. It never caches the connection itself: every
// call reads it fresh through Supervisor.client, so a FleetDevice built
// once keeps working across a restart.
type supervisorTransport struct {
	sup *Supervisor
}

// Transport returns s's device.FleetTransport view, shared by every device
// of this fleet.
func (s *Supervisor) Transport() device.FleetTransport {
	return &supervisorTransport{sup: s}
}

func (t *supervisorTransport) Healthy() bool { return t.sup.Healthy() }

func (t *supervisorTransport) Generation() uint64 { return t.sup.generation() }

func (t *supervisorTransport) Set(ctx context.Context, items []device.FleetSetItem) ([]float64, error) {
	c := t.sup.client()
	if c == nil {
		return nil, fmt.Errorf("fleet %s: sidecar is down", t.sup.cfg.Fleet)
	}
	wireItems := make([]SetItem, len(items))
	for i, it := range items {
		wireItems[i] = SetItem{Object: it.Object, Property: it.Property, Value: FloatValue(it.Value)}
	}
	results, err := c.Set(ctx, wireItems)
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(results))
	for i, r := range results {
		f, err := r.Value.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", r.Object, r.Property, err)
		}
		out[i] = f
	}
	return out, nil
}

func (t *supervisorTransport) StepTo(ctx context.Context, target time.Time) (time.Time, error) {
	c := t.sup.client()
	if c == nil {
		return time.Time{}, fmt.Errorf("fleet %s: sidecar is down", t.sup.cfg.Fleet)
	}
	return c.StepTo(ctx, target)
}

// Get returns each item's value as a device.FleetValue rather than a bare
// float64: a phasor property (a phase voltage) and a scalar property
// (P_Out, rated_power) decode differently, and that choice belongs to the
// caller, which knows which property it asked for.
func (t *supervisorTransport) Get(ctx context.Context, items []device.FleetGetItem) ([]device.FleetValue, error) {
	c := t.sup.client()
	if c == nil {
		return nil, fmt.Errorf("fleet %s: sidecar is down", t.sup.cfg.Fleet)
	}
	wireItems := make([]GetItem, len(items))
	for i, it := range items {
		wireItems[i] = GetItem{Object: it.Object, Property: it.Property}
	}
	results, err := c.Get(ctx, wireItems)
	if err != nil {
		return nil, err
	}
	out := make([]device.FleetValue, len(results))
	for i, r := range results {
		re, im, err := r.Value.components()
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", r.Object, r.Property, err)
		}
		out[i] = device.FleetValue{Re: re, Im: im}
	}
	return out, nil
}
