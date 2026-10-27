package main

import (
	"context"
	"errors"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// postMirrorUsagePoint builds and POSTs the MirrorUsagePoint for lfdi and
// returns its Location. An empty Location is an error, since no reading can
// be posted without it. The caller supplies ctx already scoped to the acting
// device, and the description that tells the mirrors apart.
func postMirrorUsagePoint(ctx context.Context, client *inverter.SEP2Client, listHref, lfdi, description string) (string, error) {
	loc, err := client.CreateMirrorUsagePoint(ctx, listHref, inverter.DeviceLFDI(lfdi), sep2.MirrorUsagePoint{
		MRID:                inverter.MirrorUsagePointMRID(lfdi),
		Description:         description,
		RoleFlags:           inverter.MirrorRoleFlags,
		ServiceCategoryKind: 0,
		Status:              1,
	})
	if err != nil {
		return "", err
	}
	if loc == "" {
		return "", errors.New("MirrorUsagePoint POST returned empty Location")
	}
	return loc, nil
}

// mirrorRetry is the backoff both mirror owners share. Every failure to have
// a working mirror, a failed create or a mirror the server lost, widens the
// wait before the next create; only a reading accepted by the mirror resets
// it, so a server that keeps losing a mirror it just created is not asked for
// a new one every report interval.
type mirrorRetry struct {
	tries int
	next  time.Time
}

// ready reports whether the backoff has elapsed.
func (r *mirrorRetry) ready(now time.Time) bool { return !now.Before(r.next) }

// failed records one failure and sets the wait before the next create.
func (r *mirrorRetry) failed(now time.Time, base time.Duration) {
	r.tries++
	r.next = now.Add(mirrorBackoff(base, r.tries))
}

// worked clears the backoff after the mirror accepted a reading.
func (r *mirrorRetry) worked() { *r = mirrorRetry{} }
