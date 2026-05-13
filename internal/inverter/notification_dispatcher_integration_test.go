// IEEE-051 notification dispatcher — integration test.
//
// End-to-end exercise: a real *SEP2Client points at an httptest SEP2
// backend serving a DERControlList. The dispatcher is wired with the
// client + cache + listHref. We invoke the dispatcher directly with a
// Notification (rather than spinning a /notify listener — that wiring is
// covered in notify_test.go's TestNotifyHandler_DispatcherInvoked, which
// already proves the listener → dispatcher seam). This test covers the
// dispatcher → SEP2 GET → cache.Refresh seam end-to-end.
//
// The combined /notify-listener + dispatcher path is implicitly covered
// because the listener invokes the dispatcher synchronously with a
// parsed Notification (IEEE-049 contract): any test that exercises
// dispatcher.Dispatch with a real Notification + real client equates to
// the full chain.

package inverter

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

func TestPhaseStateDispatcher_Integration_EndToEnd(t *testing.T) {
	t.Parallel()

	const dercListHref = "/edev/1/derp/1/derc"

	// Programmable list contents — first call returns one entry, second
	// returns two, so we can assert the cache picks up additions.
	var callN atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callN.Add(1)
		// Server should see ?l=255 paging param appended by GetDERControlList.
		if r.URL.Query().Get("l") != "255" {
			t.Errorf("expected ?l=255 paging param, got query %q", r.URL.RawQuery)
		}
		var list sep2.DERControlList
		switch callN.Load() {
		case 1:
			list.DERControl = mkControls("ctrl-1")
		default:
			list.DERControl = mkControls("ctrl-1", "ctrl-2")
		}
		body, err := xml.Marshal(&list)
		if err != nil {
			t.Errorf("marshal: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	client := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	cache := NewDERControlCache()

	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(client, cache, dercListHref); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// First notification — Resource.Href on the changed list.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: dercListHref},
		SubscribedResource: dercListHref,
		Status:             sep2.NotificationStatusChanged,
	})
	if cache.Len() != 1 {
		t.Fatalf("after first dispatch: cache len = %d, want 1", cache.Len())
	}

	// Second notification — server now publishes two controls.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: dercListHref + "/ctrl-2"},
		SubscribedResource: dercListHref,
		Status:             sep2.NotificationStatusChanged,
	})
	if cache.Len() != 2 {
		t.Fatalf("after second dispatch: cache len = %d, want 2", cache.Len())
	}

	// Cache contains the expected mRIDs.
	snap := cache.Snapshot()
	if _, ok := snap["ctrl-1"]; !ok {
		t.Errorf("snapshot missing ctrl-1")
	}
	if _, ok := snap["ctrl-2"]; !ok {
		t.Errorf("snapshot missing ctrl-2")
	}

	if got := callN.Load(); got != 2 {
		t.Fatalf("server saw %d GETs, want 2", got)
	}
}

// TestPhaseStateDispatcher_Integration_BadResponseSurvives proves the
// dispatcher does not panic / crash when the SEP2 server returns a
// non-2xx response. The state-machine cache MUST remain untouched and
// the polling loop is what eventually recovers.
func TestPhaseStateDispatcher_Integration_BadResponseSurvives(t *testing.T) {
	t.Parallel()

	const dercListHref = "/edev/1/derp/1/derc"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	t.Cleanup(srv.Close)

	client := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(client, cache, dercListHref); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: dercListHref},
		SubscribedResource: dercListHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if cache.Len() != 0 {
		t.Fatalf("cache mutated on 500: len = %d, want 0", cache.Len())
	}
}
