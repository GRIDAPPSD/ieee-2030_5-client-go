// IEEE-050 cmd-side wiring tests: registerSubscriptions + notifyURLForReceiver.
//
// Covers the graceful-bypass policy contract from subscription_register.go.
// PostSubscription itself is tested in internal/inverter : this file only
// exercises the per-resource loop, error-class dispatch, and the
// nil-receiver / missing-link short circuits.

package main

import (
	"context"
	"errors"
	"testing"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// fakeSubPoster records PostSubscription calls and returns programmed
// (href, err) responses keyed by the subscribed-resource href.
type fakeSubPoster struct {
	calls    []subPostCall
	respond  map[string]subPostResp
	fallback subPostResp // returned when the subscribed href is not in `respond`
}

type subPostCall struct {
	listHref, subscribedHref, notifyURL string
}

type subPostResp struct {
	href string
	err  error
}

func (f *fakeSubPoster) PostSubscription(_ context.Context, listHref, subscribedHref, notifyURL string) (string, error) {
	f.calls = append(f.calls, subPostCall{listHref, subscribedHref, notifyURL})
	if r, ok := f.respond[subscribedHref]; ok {
		return r.href, r.err
	}
	return f.fallback.href, f.fallback.err
}

func makeEdev(withSubList, withFSA, withDER bool) sep2.EndDevice {
	e := sep2.EndDevice{
		SFDI: "111",
	}
	if withSubList {
		e.SubscriptionListLink = &sep2.ListLink{Href: "/edev/1/sub"}
	}
	if withFSA {
		e.FunctionSetAssignmentsListLink = &sep2.ListLink{Href: "/edev/1/fsa"}
	}
	if withDER {
		e.DERListLink = &sep2.ListLink{Href: "/edev/1/der"}
	}
	return e
}

func TestRegisterSubscriptions_HappyPathBothResources(t *testing.T) {
	t.Parallel()
	p := &fakeSubPoster{
		respond: map[string]subPostResp{
			"/edev/1/fsa": {href: "/edev/1/sub/sub-FSA"},
			"/edev/1/der": {href: "/edev/1/sub/sub-DER"},
		},
	}
	out := registerSubscriptions(context.Background(), p, makeEdev(true, true, true), "https://inv/notify")
	if len(p.calls) != 2 {
		t.Fatalf("PostSubscription calls = %d, want 2", len(p.calls))
	}
	if got, ok := out.Lookup("/edev/1/fsa"); !ok || got != "/edev/1/sub/sub-FSA" {
		t.Errorf("FSA sub href = %q (ok=%v), want /edev/1/sub/sub-FSA", got, ok)
	}
	if got, ok := out.Lookup("/edev/1/der"); !ok || got != "/edev/1/sub/sub-DER" {
		t.Errorf("DER sub href = %q (ok=%v), want /edev/1/sub/sub-DER", got, ok)
	}
	// Notify URL passed through verbatim
	if p.calls[0].notifyURL != "https://inv/notify" {
		t.Errorf("notifyURL = %q, want https://inv/notify", p.calls[0].notifyURL)
	}
}

func TestRegisterSubscriptions_NoNotifyURLDisabled(t *testing.T) {
	t.Parallel()
	p := &fakeSubPoster{}
	out := registerSubscriptions(context.Background(), p, makeEdev(true, true, true), "")
	if len(p.calls) != 0 {
		t.Errorf("PostSubscription calls = %d, want 0 when notifyURL empty", len(p.calls))
	}
	if out.Len() != 0 {
		t.Errorf("returned registry len = %d, want 0", out.Len())
	}
}

func TestRegisterSubscriptions_NoSubscriptionListLinkDisabled(t *testing.T) {
	t.Parallel()
	p := &fakeSubPoster{}
	out := registerSubscriptions(context.Background(), p, makeEdev(false, true, true), "https://inv/notify")
	if len(p.calls) != 0 {
		t.Errorf("PostSubscription calls = %d, want 0 without SubscriptionListLink", len(p.calls))
	}
	if out.Len() != 0 {
		t.Errorf("returned registry len = %d, want 0", out.Len())
	}
}

func TestRegisterSubscriptions_NoChildLinksDisabled(t *testing.T) {
	t.Parallel()
	p := &fakeSubPoster{}
	out := registerSubscriptions(context.Background(), p, makeEdev(true, false, false), "https://inv/notify")
	if len(p.calls) != 0 {
		t.Errorf("PostSubscription calls = %d, want 0 with no FSA/DER links", len(p.calls))
	}
	if out.Len() != 0 {
		t.Errorf("returned registry len = %d, want 0", out.Len())
	}
}

func TestRegisterSubscriptions_405AbortsLoop(t *testing.T) {
	t.Parallel()
	// First resource returns 405; loop should abort BEFORE calling for the
	// second resource (server-wide capability negation).
	p := &fakeSubPoster{
		respond: map[string]subPostResp{
			"/edev/1/fsa": {err: inverter.ErrMethodNotAllowed},
		},
		fallback: subPostResp{href: "/edev/1/sub/should-not-reach"},
	}
	out := registerSubscriptions(context.Background(), p, makeEdev(true, true, true), "https://inv/notify")
	if len(p.calls) != 1 {
		t.Errorf("PostSubscription calls = %d, want 1 (loop aborted on 405)", len(p.calls))
	}
	if out.Len() != 0 {
		t.Errorf("returned registry len = %d, want 0 on 405", out.Len())
	}
}

func TestRegisterSubscriptions_PerResourceErrorContinues(t *testing.T) {
	t.Parallel()
	// First resource fails with a generic error (not 405, not ctx); loop
	// should continue and try the second.
	p := &fakeSubPoster{
		respond: map[string]subPostResp{
			"/edev/1/fsa": {err: errors.New("transient blip")},
			"/edev/1/der": {href: "/edev/1/sub/sub-DER"},
		},
	}
	out := registerSubscriptions(context.Background(), p, makeEdev(true, true, true), "https://inv/notify")
	if len(p.calls) != 2 {
		t.Errorf("PostSubscription calls = %d, want 2", len(p.calls))
	}
	if _, ok := out.Lookup("/edev/1/fsa"); ok {
		t.Errorf("failed resource leaked into registry")
	}
	if got, ok := out.Lookup("/edev/1/der"); !ok || got != "/edev/1/sub/sub-DER" {
		t.Errorf("successful resource missing: %q (ok=%v)", got, ok)
	}
}

func TestRegisterSubscriptions_ContextCancelAborts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakeSubPoster{
		respond: map[string]subPostResp{
			"/edev/1/fsa": {err: context.Canceled},
		},
		fallback: subPostResp{href: "/edev/1/sub/should-not-reach"},
	}
	out := registerSubscriptions(ctx, p, makeEdev(true, true, true), "https://inv/notify")
	if len(p.calls) != 1 {
		t.Errorf("PostSubscription calls = %d, want 1 (aborted on ctx)", len(p.calls))
	}
	if out.Len() != 0 {
		t.Errorf("returned registry len = %d, want 0", out.Len())
	}
}

func TestNotifyURLForReceiver_Nil(t *testing.T) {
	t.Parallel()
	if got := notifyURLForReceiver(nil); got != "" {
		t.Errorf("nil receiver URL = %q, want \"\"", got)
	}
}

// fakeAddrSource stubs the addrSource interface for notifyURLForAddrSource.
type fakeAddrSource struct {
	addr string
	err  error
}

func (f fakeAddrSource) Addr() (string, error) { return f.addr, f.err }

func TestNotifyURLForAddrSource_Happy(t *testing.T) {
	t.Parallel()
	got := notifyURLForAddrSource(fakeAddrSource{addr: "127.0.0.1:54321"})
	want := "https://127.0.0.1:54321/notify"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

func TestNotifyURLForAddrSource_AddrError(t *testing.T) {
	t.Parallel()
	got := notifyURLForAddrSource(fakeAddrSource{err: errors.New("not started")})
	if got != "" {
		t.Errorf("URL = %q, want \"\" on Addr error", got)
	}
}

func TestNotifyURLForAddrSource_EmptyAddr(t *testing.T) {
	t.Parallel()
	got := notifyURLForAddrSource(fakeAddrSource{addr: ""})
	if got != "" {
		t.Errorf("URL = %q, want \"\" on empty addr", got)
	}
}
