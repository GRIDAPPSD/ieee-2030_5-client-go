// Integration tests for dispatch.SimulatorDispatcher against a real HTTP
// server.
//
// These are relocated from internal/inverter/notification_dispatcher_integration_test.go
// (IEEESIM-004). The package is dispatch_test (external) to avoid a circular
// import: dispatch imports inverter, so internal-package tests would not be
// able to import both simultaneously.
//
// The test exercises the dispatcher-to-SEP2-GET-to-cache seam end-to-end
// using a plain httptest.Server and a minimal DERControlListFetcher shim,
// without spinning a real TLS listener or a full SEP2Client. The /notify
// listener seam is covered separately in internal/inverter/notify_test.go
// and the cancel-hook TLS integration test in
// internal/inverter/cancel_integration_test.go.

package dispatch_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter/dispatch"
)

// httpFetcher is a minimal DERControlListFetcher shim that GETs the href
// (relative to its baseURL) using a plain *http.Client and XML-decodes the
// response. Satisfies dispatch.DERControlListFetcher structurally.
type httpFetcher struct {
	baseURL string
	client  *http.Client
}

func (f *httpFetcher) GetDERControlList(ctx context.Context, href string) (sep2.DERControlList, string, error) {
	url := f.baseURL + href
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return sep2.DERControlList{}, "", fmt.Errorf("build request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return sep2.DERControlList{}, "", fmt.Errorf("GET %s: %w", href, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return sep2.DERControlList{}, "", fmt.Errorf("GET %s: status %d", href, resp.StatusCode)
	}
	var list sep2.DERControlList
	if err := xml.NewDecoder(resp.Body).Decode(&list); err != nil {
		return sep2.DERControlList{}, "", fmt.Errorf("decode DERControlList: %w", err)
	}
	return list, "", nil
}

// mkControlsInteg builds a minimal []sep2.DERControl with the given mRIDs.
func mkControlsInteg(mrids ...string) []sep2.DERControl {
	out := make([]sep2.DERControl, len(mrids))
	for i, mrid := range mrids {
		var c sep2.DERControl
		c.MRID = mrid
		out[i] = c
	}
	return out
}

// TestSimulatorDispatcher_Integration_EndToEnd exercises the
// dispatcher-to-HTTP-GET-to-cache path with a real httptest.Server.
// Two sequential notifications drive two GETs; the cache accumulates entries
// on each refresh.
func TestSimulatorDispatcher_Integration_EndToEnd(t *testing.T) {
	t.Parallel()

	const dercListHref = "/edev/1/derp/1/derc"

	// Programmable list contents: first call returns one entry, second returns
	// two, so we can assert the cache accumulates entries on refresh.
	var callN atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callN.Add(1)
		var list sep2.DERControlList
		switch callN.Load() {
		case 1:
			list.DERControl = mkControlsInteg("ctrl-1")
		default:
			list.DERControl = mkControlsInteg("ctrl-1", "ctrl-2")
		}
		body, err := xml.Marshal(&list)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	fetcher := &httpFetcher{baseURL: srv.URL, client: srv.Client()}
	cache := inverter.NewDERControlCache()

	d := dispatch.NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fetcher, cache, dercListHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	// First notification: resource href matches the registered DERControlList.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: dercListHref},
		SubscribedResource: dercListHref,
		Status:             sep2.NotificationStatusChanged,
	})
	if got := cache.Len(); got != 1 {
		t.Fatalf("after first dispatch: cache len = %d, want 1", got)
	}

	// Second notification: newResourceURI child triggers the same list refresh.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: dercListHref + "/ctrl-2"},
		SubscribedResource: dercListHref,
		Status:             sep2.NotificationStatusChanged,
	})
	if got := cache.Len(); got != 2 {
		t.Fatalf("after second dispatch: cache len = %d, want 2", got)
	}

	snap := cache.Snapshot()
	if _, ok := snap["ctrl-1"]; !ok {
		t.Errorf("snapshot missing ctrl-1; got %v", snap)
	}
	if _, ok := snap["ctrl-2"]; !ok {
		t.Errorf("snapshot missing ctrl-2; got %v", snap)
	}

	if got := callN.Load(); got != 2 {
		t.Fatalf("server saw %d GETs, want 2", got)
	}
}

// TestSimulatorDispatcher_Integration_BadResponseSurvives proves the
// dispatcher does not panic or crash when the SEP2 server returns a
// non-2xx response. The cache must remain untouched and the dispatcher
// must continue accepting subsequent notifications.
func TestSimulatorDispatcher_Integration_BadResponseSurvives(t *testing.T) {
	t.Parallel()

	const dercListHref = "/edev/1/derp/1/derc"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	t.Cleanup(srv.Close)

	fetcher := &httpFetcher{baseURL: srv.URL, client: srv.Client()}
	cache := inverter.NewDERControlCache()

	d := dispatch.NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fetcher, cache, dercListHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: dercListHref},
		SubscribedResource: dercListHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := cache.Len(); got != 0 {
		t.Fatalf("cache mutated on 500: len = %d, want 0", got)
	}
}
