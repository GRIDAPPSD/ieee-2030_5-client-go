package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// A withdrawal of the replaced request that the server refuses is kept and
// retried on later polls, until the server accepts it.
func TestTrigger_RefusedWithdrawalOfTheReplacedRequestIsRetriedOnLaterPolls(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	first := r.cur.mrid
	buf := captureLog(t)
	ctx := context.Background()

	f.putErr = errors.New("503")
	r.Trigger(ctx)
	second := r.cur.mrid
	if second == first || f.putTries != 1 || len(f.puts) != 0 {
		t.Fatalf("following %q, %d PUT tries, %d accepted; want the new request, 1 try, 0 accepted", r.cur.mrid, f.putTries, len(f.puts))
	}

	// Inside the poll interval nothing is retried.
	f.putErr = nil
	clk.t = clk.t.Add(10 * time.Second)
	r.Poll(ctx)
	if f.putTries != 1 {
		t.Fatalf("%d PUT tries 10 s after the failure, want still 1", f.putTries)
	}

	// A poll interval after the failure the server accepts it.
	clk.t = clk.t.Add(25 * time.Second)
	buf.Reset()
	r.Poll(ctx)
	if len(f.puts) != 1 || f.puts[0].MRID != first || f.putHref[0] != "/edev/1/frq/1" ||
		f.puts[0].RequestStatus.RequestStatus != sep2.RequestStatusCancelled {
		t.Fatalf("PUTs %+v at %v, want the first request %q cancelled at its own href", f.puts, f.putHref, first)
	}
	if !strings.Contains(buf.String(), "request "+first) {
		t.Errorf("the retry is not logged with the request mRID %q: %q", first, buf.String())
	}

	// Accepted once: no further attempts.
	clk.t = clk.t.Add(10 * time.Minute)
	r.Poll(ctx)
	if f.putTries != 2 {
		t.Errorf("%d PUT tries after the accepted retry, want 2", f.putTries)
	}
}

// Retries back off: the wait doubles from the poll interval (30 s) and stops
// growing at 5 minutes.
func TestTrigger_WithdrawalRetriesBackOffFromThePollIntervalToFiveMinutes(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	f.putErr = errors.New("503")
	trigger := clk.t
	r.Trigger(ctx)

	var gaps []time.Duration
	last, tries := trigger, f.putTries
	for clk.t.Before(trigger.Add(1500 * time.Second)) {
		clk.t = clk.t.Add(time.Second)
		r.Poll(ctx)
		if f.putTries != tries {
			gaps = append(gaps, clk.t.Sub(last))
			last, tries = clk.t, f.putTries
		}
	}
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second, 300 * time.Second}
	if len(gaps) < len(want) {
		t.Fatalf("retry gaps %v, want to start %v", gaps, want)
	}
	for i, w := range want {
		if gaps[i] != w {
			t.Errorf("gap %d is %v, want %v (all gaps %v)", i, gaps[i], w, gaps)
		}
	}
}

// A replaced request whose window and grace have passed no longer needs
// withdrawing: the retries stop.
func TestTrigger_WithdrawalRetriesStopAfterTheReplacedRequestsWindow(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	f.putErr = errors.New("503")
	old := r.cur
	r.Trigger(ctx)
	clk.t = old.end.Add(r.grace())
	tries := f.putTries
	r.Poll(ctx)
	if f.putTries != tries {
		t.Errorf("retried a request whose window and grace had passed")
	}
	f.putErr = nil
	clk.t = clk.t.Add(time.Hour)
	r.Poll(ctx)
	if f.putTries != tries || len(f.puts) != 0 {
		t.Errorf("PUT after the window: %d tries (was %d), %d accepted", f.putTries, tries, len(f.puts))
	}
}

// A withdrawn request never dispatches again, even when a later read still
// shows its grant.
func TestPoll_AWithdrawnRequestIsNeverDispatchedAgain(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	s := r.cur.start
	f.list = listOf(resp("G", r.cur.mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = s.Add(time.Minute)
	r.Poll(ctx)
	if r.disp.terms == nil {
		t.Fatal("grant not installed")
	}

	r.Withdraw(ctx)
	if r.disp.terms != nil {
		t.Fatal("Withdraw left the grant installed")
	}
	clk.t = s.Add(2 * time.Minute)
	r.Poll(ctx) // the server has not applied the cancellation: the grant still reads as live
	if r.disp.terms != nil {
		t.Errorf("a poll after the withdrawal reinstalled the grant %+v", r.disp.terms)
	}
}

// The withdrawal gets a timeout of its own, not what is left of the post's.
func TestTrigger_TheReplacedRequestsWithdrawalHasItsOwnTimeout(t *testing.T) {
	r, f, _ := startedReserver(t, nil)
	f.postDelay = 30 * time.Millisecond
	r.Trigger(context.Background())
	if f.putTries != 1 {
		t.Fatalf("%d PUT tries, want 1", f.putTries)
	}
	if f.postDeadline.IsZero() || f.putDeadline.IsZero() {
		t.Fatalf("post deadline %v, put deadline %v: both calls must carry a timeout", f.postDeadline, f.putDeadline)
	}
	if !f.putDeadline.After(f.postDeadline) {
		t.Errorf("the withdrawal's deadline %v is not after the post's %v: it shares the post's timeout", f.putDeadline, f.postDeadline)
	}
}

// A request that has finished is not withdrawn by its replacement.
func TestTrigger_DoesNotWithdrawAFinishedRequest(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	ctx := context.Background()
	clk.t = r.cur.end.Add(r.grace())
	r.Poll(ctx)
	if !r.cur.done {
		t.Fatal("the request did not finish")
	}
	r.Trigger(ctx)
	if f.putTries != 0 {
		t.Errorf("%d PUTs for a finished request, want 0", f.putTries)
	}
}

// The server gave no Location: there is nothing to PUT to, so nothing is
// sent and the reason is logged.
func TestWithdraw_WithoutALocationSendsNothingAndSaysWhy(t *testing.T) {
	f := &fakeFrq{noLocation: true}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	buf := captureLog(t)
	ctx := context.Background()
	r.Trigger(ctx)
	r.Withdraw(ctx)
	r.Trigger(ctx)
	if f.putTries != 0 {
		t.Errorf("%d PUTs with no Location, want 0", f.putTries)
	}
	if !strings.Contains(buf.String(), "cannot be withdrawn") {
		t.Errorf("no log line says why nothing was sent: %q", buf.String())
	}
	if r.cur.withdrawn {
		t.Error("a request that was never withdrawn is marked withdrawn")
	}
}

// The body names the resource it replaces: its href is the Location.
func TestWithdraw_BodyCarriesTheRequestsHref(t *testing.T) {
	r, f, _ := startedReserver(t, nil)
	f.location = "/edev/1/frq/9"
	r.Trigger(context.Background())
	f.puts, f.putHref = nil, nil
	r.Withdraw(context.Background())
	if len(f.puts) != 1 || f.puts[0].Href != "/edev/1/frq/9" || f.putHref[0] != "/edev/1/frq/9" {
		t.Fatalf("PUTs %+v at %v, want one with Href and target /edev/1/frq/9", f.puts, f.putHref)
	}
}

// A state with no response behind it says so in its answer-change line:
// pending (after an answer disappears) and expired.
func TestAnswerChanges_PendingAndExpiredNameNoResponse(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	buf := captureLog(t)
	ctx := context.Background()
	s := r.cur.start
	mrid := r.cur.mrid

	f.list = listOf(resp("G", mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = s.Add(-2000 * time.Second)
	r.Poll(ctx)

	buf.Reset()
	f.list = listOf()
	clk.t = s.Add(-1000 * time.Second)
	r.Poll(ctx)
	if got := buf.String(); !strings.Contains(got, "request "+mrid+" is pending (no response)") {
		t.Errorf("pending line = %q, want it to say pending (no response)", got)
	}

	buf.Reset()
	clk.t = s.Add(r.grace())
	r.Poll(ctx)
	if got := buf.String(); !strings.Contains(got, "request "+mrid+" is not granted") || !strings.Contains(got, "(no response)") {
		t.Errorf("expired line = %q, want it to say not granted (no response)", got)
	}
}

// A denial is logged with the request mRID and the denying response's mRID.
func TestAnswerChanges_DenialIsLoggedWithRequestAndResponseMRIDs(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	buf := captureLog(t)
	s := r.cur.start
	denial := resp("D1", r.cur.mrid, s.Unix()-1000, sep2.EventStatusScheduled,
		&sep2.DateTimeInterval{Start: s.Unix(), Duration: 0}, 0, 0)
	f.list = listOf(denial)
	clk.t = s.Add(-time.Minute)
	r.Poll(context.Background())
	if got := buf.String(); !strings.Contains(got, "request "+r.cur.mrid+" is denied (response D1)") {
		t.Errorf("denial line = %q, want request, denied and response D1", got)
	}
}
