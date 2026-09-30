// Item 4: replaces the fire-and-forget goroutine loop (and its own
// log.Fatalf on any Run error) that used to start every fleet's
// Supervisor. startFleets waits for every fleet's first start to finish
// before main proceeds into certificates and the network, so an
// initial refusal is caught and reported before any of that work begins,
// matching Decision 2's "fail closed, at startup, through the normal
// shutdown path" for a refused first start, and Decision 2's "the fleet
// is DOWN, the process keeps running" for a restart budget exhausted
// later.

package main

import (
	"context"
	"sync"
)

// fleetSupervisor is the small interface startFleets needs from a fleet's
// Supervisor. Production passes *gridlabd.Supervisor, which satisfies it
// via its existing Run, WaitStarted, Stop and Fleet methods; a test passes
// a fake whose Stop can be made to block until released, so "stop returns
// only after every fake's Stop has finished" can be forced and observed.
type fleetSupervisor interface {
	Run(ctx context.Context) error
	WaitStarted(ctx context.Context) error
	Stop()
	Fleet() string
}

// startFleets starts every fleet's supervisor and waits for each one's
// first start to finish (healthy, or refused). On any refusal it stops
// every fleet that did start (including ones that started successfully)
// and returns the refusing fleet's error; if the outer ctx was already
// done by the time that refusal surfaced, ctx's own error is returned
// instead, so a signal arriving during this wait is reported as a
// cancellation, never as a refusal the caller never asked about (exit
// path table, Decision 4).
//
// Once every fleet is healthy, startFleets returns at once. A LATER Run
// error (the restart budget exhausted) does not stop any other fleet or
// make the returned stop report an error: Decision 2's "a DOWN fleet
// stops only itself." startFleets does NOT log that transition itself:
// Supervisor.Run already reached its own terminal Down state and logged
// "fleet <name> DOWN: <reason>" exactly once through its own cfg.Log
// before returning here, and a second log line from this layer would
// double it (closing coverage MEDIUM / error-handling LOW: the two
// layers used to log the same transition independently). Supervisor is
// the one layer that owns this line, since it is the one place a
// same-fleet's later restart attempts and its final state are already
// serialized; startFleets, driving several fleets at once, is not.
//
// The returned stop is idempotent (safe to call more than once, from more
// than one goroutine) and returns only once every fleet's own Stop call
// has itself returned.
func startFleets(ctx context.Context, fleets []fleetSupervisor) (stop func(), err error) {
	if len(fleets) == 0 {
		return func() {}, nil
	}

	var runWG sync.WaitGroup
	for _, f := range fleets {
		runWG.Add(1)
		go func(f fleetSupervisor) {
			defer runWG.Done()
			_ = f.Run(ctx)
		}(f)
	}

	var stopOnce sync.Once
	stopFn := func() {
		stopOnce.Do(func() {
			var stopWG sync.WaitGroup
			for _, f := range fleets {
				stopWG.Add(1)
				go func(f fleetSupervisor) {
					defer stopWG.Done()
					f.Stop()
				}(f)
			}
			stopWG.Wait()
		})
		runWG.Wait()
	}

	waitResults := make(chan error, len(fleets))
	for _, f := range fleets {
		go func(f fleetSupervisor) {
			waitResults <- f.WaitStarted(ctx)
		}(f)
	}
	var firstErr error
	for range fleets {
		if werr := <-waitResults; werr != nil && firstErr == nil {
			firstErr = werr
		}
	}
	if firstErr != nil {
		stopFn()
		if ctx.Err() != nil {
			return func() {}, ctx.Err()
		}
		return func() {}, firstErr
	}

	return stopFn, nil
}
