package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// The operator's withdrawal that the server refuses is retried like a
// replaced request's, until the server accepts it, and then the grant stops.
func TestWithdraw_AFailedOperatorWithdrawalIsRetriedUntilAccepted(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	s := r.cur.start
	f.list = listOf(resp("G", r.cur.mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = s.Add(time.Minute)
	r.Poll(ctx)
	if r.disp.terms == nil {
		t.Fatal("grant not installed")
	}

	f.putErr = errors.New("503")
	r.Withdraw(ctx)
	if f.putTries != 1 || r.cur.withdrawn {
		t.Fatalf("%d tries, withdrawn %v; want 1 failed try", f.putTries, r.cur.withdrawn)
	}
	if r.disp.terms == nil {
		t.Error("a refused withdrawal dropped the grant")
	}

	f.putErr = nil
	clk.t = clk.t.Add(35 * time.Second)
	r.Poll(ctx)
	if len(f.puts) != 1 || f.puts[0].MRID != r.cur.mrid || f.puts[0].RequestStatus.RequestStatus != sep2.RequestStatusCancelled {
		t.Fatalf("PUTs %+v after the retry, want the followed request cancelled", f.puts)
	}
	if !r.cur.withdrawn || r.disp.terms != nil {
		t.Errorf("withdrawn %v, grant %+v; want withdrawn and no grant", r.cur.withdrawn, r.disp.terms)
	}
	if len(r.stale) != 0 {
		t.Errorf("%d withdrawals still waiting after the accepted retry, want 0", len(r.stale))
	}
}

// A second refused SIGUSR2 for the same request does not queue it twice.
func TestWithdraw_ARequestIsQueuedForRetryOnce(t *testing.T) {
	r, f, _ := startedReserver(t, nil)
	ctx := context.Background()
	f.putErr = errors.New("503")
	r.Withdraw(ctx)
	r.Withdraw(ctx)
	if len(r.stale) != 1 {
		t.Errorf("%d queued withdrawals for one request, want 1", len(r.stale))
	}
}

// A refusal that will not change (400, 404, or the client's own off-server
// refusal) is dropped with one line naming it; a transient failure stays.
func TestWithdraw_APermanentRefusalIsNotRetried(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"400", fmt.Errorf("PUT FlowReservationRequest: %w", inverter.ErrBadRequest), "400"},
		{"404", fmt.Errorf("PUT FlowReservationRequest: %w", inverter.ErrNotFound), "404"},
		{"off server", fmt.Errorf("PUT FlowReservationRequest: href %q: %w", "x", inverter.ErrHrefOffServer), "not on the configured server"},
	}
	for _, tc := range cases {
		for _, via := range []string{"replaced", "operator", "retry"} {
			t.Run(tc.name+" "+via, func(t *testing.T) {
				r, f, clk := startedReserver(t, nil)
				ctx := context.Background()
				first := r.cur.mrid
				buf := captureLog(t)
				switch via {
				case "replaced":
					f.putErr = tc.err
					r.Trigger(ctx)
				case "operator":
					f.putErr = tc.err
					r.Withdraw(ctx)
				case "retry":
					f.putErr = errors.New("503")
					r.Trigger(ctx)
					f.putErr = tc.err
					clk.t = clk.t.Add(35 * time.Second)
					r.Poll(ctx)
				}
				tries := f.putTries
				if len(r.stale) != 0 {
					t.Errorf("%d withdrawals kept for retry, want 0", len(r.stale))
				}
				var lines []string
				for _, l := range strings.Split(buf.String(), "\n") {
					if strings.Contains(l, "not retrying") {
						lines = append(lines, l)
					}
				}
				if len(lines) != 1 || !strings.Contains(lines[0], first) || !strings.Contains(lines[0], tc.want) {
					t.Errorf("drop lines %q, want one naming %q and %q", lines, first, tc.want)
				}
				clk.t = clk.t.Add(10 * time.Minute)
				r.Poll(ctx)
				if f.putTries != tries {
					t.Errorf("%d tries after the refusal, was %d: it was retried", f.putTries, tries)
				}
			})
		}
	}
}

// What still waits for a retry is logged when the client shuts down.
func TestLogPendingWithdrawals_NamesTheRequestsStillWaiting(t *testing.T) {
	r, f, _ := startedReserver(t, nil)
	buf := captureLog(t)
	r.logPendingWithdrawals()
	if buf.Len() != 0 {
		t.Errorf("logged %q with nothing waiting", buf.String())
	}
	var nilReserver *reserver
	nilReserver.logPendingWithdrawals()

	first := r.cur.mrid
	f.putErr = errors.New("503")
	r.Trigger(context.Background())
	r.logPendingWithdrawals()
	if got := buf.String(); !strings.Contains(got, first) || !strings.Contains(got, "never withdrawn") {
		t.Errorf("shutdown line = %q, want it to name %q as never withdrawn", got, first)
	}
}

// After a successful withdrawal a read that still shows the grant is logged
// as withdrawn, and the request finishes as withdrawn.
func TestPoll_AWithdrawnRequestIsLoggedAsWithdrawn(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	s := r.cur.start
	mrid := r.cur.mrid
	r.Withdraw(ctx)

	// The server has not applied the cancellation yet: the read shows a grant.
	f.list = listOf(resp("G", mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	buf := captureLog(t)
	clk.t = s.Add(2 * time.Minute)
	r.Poll(ctx)
	got := buf.String()
	if strings.Contains(got, "is granted") || !strings.Contains(got, "request "+mrid+" is withdrawn") {
		t.Errorf("poll after the withdrawal logged %q, want withdrawn and no grant", got)
	}
	buf.Reset()
	clk.t = s.Add(3 * time.Minute)
	r.Poll(ctx)
	if buf.Len() != 0 {
		t.Errorf("an unchanged read after the withdrawal logged %q", buf.String())
	}

	buf.Reset()
	clk.t = r.cur.end.Add(r.grace())
	r.Poll(ctx)
	if got := buf.String(); !strings.Contains(got, "request "+mrid+" finished: withdrawn") {
		t.Errorf("finish line = %q, want finished: withdrawn", got)
	}
}

// A retry is attempted until the end of the window plus the grace, not only
// until the end of the window.
func TestRetryWithdrawals_RetriesInsideTheGraceAfterTheWindow(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	old := r.cur
	f.putErr = errors.New("503")
	r.Trigger(ctx)
	tries := f.putTries
	clk.t = old.end.Add(r.grace() / 2)
	r.Poll(ctx)
	if f.putTries != tries+1 {
		t.Errorf("%d tries inside the grace, want %d", f.putTries, tries+1)
	}
}

// Retries run on a poll whatever state the followed request is in.
func TestPoll_RetriesWithdrawalsEvenWhenTheFollowedRequestIsDone(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	f.putErr = errors.New("503")
	r.Trigger(ctx)
	r.cur.done = true
	f.putErr = nil
	clk.t = clk.t.Add(35 * time.Second)
	r.Poll(ctx)
	if len(f.puts) != 1 {
		t.Errorf("%d accepted PUTs, want the queued withdrawal retried", len(f.puts))
	}
}

// A request with nothing to withdraw is not queued: it is finished, already
// withdrawn, or the server gave no Location for it.
func TestRetryLater_QueuesOnlyWhatCanStillBeWithdrawn(t *testing.T) {
	t.Run("finished", func(t *testing.T) {
		r, _, clk := startedReserver(t, nil)
		clk.t = r.cur.end.Add(r.grace())
		r.Poll(context.Background())
		r.retryLater(r.cur, clk.t)
		if len(r.stale) != 0 {
			t.Errorf("%d queued for a finished request", len(r.stale))
		}
	})
	t.Run("already withdrawn", func(t *testing.T) {
		r, _, clk := startedReserver(t, nil)
		r.Withdraw(context.Background())
		r.retryLater(r.cur, clk.t)
		if len(r.stale) != 0 {
			t.Errorf("%d queued for a withdrawn request", len(r.stale))
		}
	})
	t.Run("no location", func(t *testing.T) {
		f := &fakeFrq{noLocation: true}
		clk := &clock{t0}
		r := testReserver(t, f, clk, nil)
		r.Trigger(context.Background())
		r.Trigger(context.Background())
		if len(r.stale) != 0 {
			t.Errorf("%d queued for a request with no href", len(r.stale))
		}
	})
}
