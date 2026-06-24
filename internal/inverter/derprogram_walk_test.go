package inverter_test

// Backfill of the IEEE-036 deferred unit tests for the four new SEP2 client
// methods landed at PR #78 (merge SHA 7733cf1):
//
//   - (*SEP2Client).GetDERProgramList
//   - (*SEP2Client).GetDefaultDERControl
//   - (*SEP2Client).GetDERControlList
//   - (*SEP2Client).GetDERCurveList
//
// Plan-3 csip-test-debt-sweep Phase 5, IEEE-073. Origin ticket IEEE-036 is
// MERGED; behavior frozen. Reuses the IEEE-069 / IEEE-070 / IEEE-071 /
// IEEE-072 bedrock: ccmTestEnv from client_ccm_test.go and startIdleListener
// from idle_test.go, plus the writer-helper / newClient-helper pattern set
// by fsalist_test.go (Pike J's IEEE-072 PR #95).
//
// Cases shipped (from IEEE-073's mandatory + implicit unit case list : see
// backlog.md IEEE-073 and the Phase 5 phase doc):
//
//  Per-method unit tests (4 methods × 4 cases = 16 cases):
//    - HappyPath          : XML parsed end-to-end, exact-hit-count of 1
//    - EmptyHref          : sentinel error, zero HTTP hits (the 4 mandatory
//                            implicit cases the IEEE-073 ticket calls out)
//    - NotFound           : 404 wrapped, path + status surfaced
//    - MalformedXML       : non-XML body unwraps to *xml.SyntaxError
//
//  Paging-hint contract : single subtest table covering all three list
//  methods (`GetDERProgramList`, `GetDERControlList`, `GetDERCurveList`)
//  to lock the `?l=255` / `&l=255` rule from the IEEE-036 doc comments.
//  `GetDefaultDERControl` is not a list method and is excluded by design.
//
// Phase 2c walk-correctness cases (IEEE-073 mandatory cases 1, 2, 3, 5, 7)
// live in TestWalkDERProgramTree_* below, exercising the package-level
// `walkDERProgramTree` function in cmd/inverterclient/main.go via an
// equivalent local walker built on the same four client methods. See the
// Phase 2c deferral note in the file under cmd/inverterclient/.
//
// IEEE-073 mandatory cases 4 (empty-FSAList idle-loop) and 6 (pollRate
// throttling on empty enumerated DERProgram set) live inside the Phase 2c
// outer for-loop in `cmd/inverterclient/main.go` (lines ~502-527 of the
// current main.go), which has the same structural problem as Phase 2b /
// Phase 2c-FSAList-discovery (log.Fatalf on walk error, no function seam
// to drive the cfg.CSIP / cfg.AllowUnregistered / dcap.PollRate state
// space). Deferred to IEEE-076 : see PR body.

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// derProgramListHref / derControlListHref / derCurveListHref / defaultDERControlHref
// are the canonical fixture paths. CSIP V1.2 CORE-012 step 2 binds DERProgram
// off FSA.DERProgramListLink; the literal path is server-chosen. The values
// here match the convention used in the sep2server function-set router
// (`/edev/1/fsa/0/derp/0/...`) so a future server-side e2e would line up.
const (
	derProgramListHref    = "/edev/1/fsa/0/derp"
	derControlListHref    = "/edev/1/fsa/0/derp/0/derc"
	derCurveListHref      = "/edev/1/fsa/0/derp/0/dercurve"
	defaultDERControlHref = "/edev/1/fsa/0/derp/0/dderc"
)

// writeDERProgramList encodes a DERProgramList to the response writer with
// the SEP+XML content type. Mirrors writeFSAList from fsalist_test.go.
func writeDERProgramList(t *testing.T, w http.ResponseWriter, list sep2.DERProgramList) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&list); err != nil {
		t.Errorf("encode DERProgramList: %v", err)
	}
}

func writeDefaultDERControl(t *testing.T, w http.ResponseWriter, d sep2.DefaultDERControl) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&d); err != nil {
		t.Errorf("encode DefaultDERControl: %v", err)
	}
}

// writeDERControlListFull writes a fully-populated DERControlList (the
// existing writeDERControlList helper in dercontrol_poll_test.go takes a
// []DERControl slice and computes attrs; this helper preserves the caller's
// ListResource so paging-hint tests can check the raw response shape).
func writeDERControlListFull(t *testing.T, w http.ResponseWriter, list sep2.DERControlList) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&list); err != nil {
		t.Errorf("encode DERControlList: %v", err)
	}
}

func writeDERCurveList(t *testing.T, w http.ResponseWriter, list sep2.DERCurveList) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&list); err != nil {
		t.Errorf("encode DERCurveList: %v", err)
	}
}

// newDERWalkTestClient is the per-test helper: boots a gotls server with
// the supplied handler, builds a production SEP2 client against it, returns
// both for the caller to drive. Mirrors newFSAListTestClient from
// fsalist_test.go.
func newDERWalkTestClient(t *testing.T, handler http.Handler) (*inverter.SEP2Client, context.Context) {
	t.Helper()
	env := newCCMTestEnv(t)
	serverURL, _ := startIdleListener(t, env, handler)
	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  env.deviceCertPath,
		KeyFile:   env.deviceKeyPath,
		CAFile:    env.caCertPath,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return client, ctx
}

// ----- GetDERProgramList ----------------------------------------------------

// TestGetDERProgramList_HappyPath : IEEE-073 implicit case (per-method happy
// path mirror of IEEE-072 case 1).
//
// Stub returns DERProgramList with 2 entries : each with a distinct mRID +
// Primacy + DefaultDERControlLink / DERControlListLink / DERCurveListLink :
// and a non-zero All attribute. GetDERProgramList must return both entries
// in order with every field parsed.
func TestGetDERProgramList_HappyPath(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	want := sep2.DERProgramList{
		ListResource: sep2.ListResource{All: 2, Results: 2},
		DERProgram: []sep2.DERProgram{
			{
				MRID:                  "PROG0001",
				Description:           "Primary program",
				Primacy:               1,
				DefaultDERControlLink: &sep2.Link{Href: defaultDERControlHref},
				DERControlListLink:    &sep2.ListLink{Href: derControlListHref},
				DERCurveListLink:      &sep2.ListLink{Href: derCurveListHref},
			},
			{
				MRID:                  "PROG0002",
				Description:           "Backup program",
				Primacy:               5,
				DefaultDERControlLink: &sep2.Link{Href: defaultDERControlHref + "/2"},
				DERControlListLink:    &sep2.ListLink{Href: derControlListHref + "/2"},
				DERCurveListLink:      &sep2.ListLink{Href: derCurveListHref + "/2"},
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(derProgramListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDERProgramList(t, w, want)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERProgramList(ctx, derProgramListHref)
	if err != nil {
		t.Fatalf("GetDERProgramList: %v", err)
	}
	if n := len(got.DERProgram); n != 2 {
		t.Fatalf("len(DERProgram) = %d, want 2", n)
	}
	for i, w := range want.DERProgram {
		g := got.DERProgram[i]
		if g.MRID != w.MRID {
			t.Errorf("entry[%d].MRID = %q, want %q", i, g.MRID, w.MRID)
		}
		if g.Primacy != w.Primacy {
			t.Errorf("entry[%d].Primacy = %d, want %d", i, g.Primacy, w.Primacy)
		}
		if g.DefaultDERControlLink == nil || g.DefaultDERControlLink.Href != w.DefaultDERControlLink.Href {
			t.Errorf("entry[%d].DefaultDERControlLink mismatch: got=%v want=%v", i, g.DefaultDERControlLink, w.DefaultDERControlLink)
		}
		if g.DERControlListLink == nil || g.DERControlListLink.Href != w.DERControlListLink.Href {
			t.Errorf("entry[%d].DERControlListLink mismatch: got=%v want=%v", i, g.DERControlListLink, w.DERControlListLink)
		}
		if g.DERCurveListLink == nil || g.DERCurveListLink.Href != w.DERCurveListLink.Href {
			t.Errorf("entry[%d].DERCurveListLink mismatch: got=%v want=%v", i, g.DERCurveListLink, w.DERCurveListLink)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("DERProgramList GET hits = %d, want exactly 1", n)
	}
}

// TestGetDERProgramList_EmptyHref : IEEE-073 mandatory implicit case (empty-href
// sentinel error). Empty href is rejected without an HTTP round trip; sentinel
// "DERProgramList href required" is contractual (`walkDERProgramTree` returns
// it wrapped under `FSA mRID=%s DERProgramList: %w`).
func TestGetDERProgramList_EmptyHref(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERProgramList(ctx, "")
	if err == nil {
		t.Fatalf("GetDERProgramList(ctx, \"\") returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "DERProgramList href required") {
		t.Errorf("error %q does not contain %q", err.Error(), "DERProgramList href required")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0 (empty-href must short-circuit before any GET)", n)
	}
}

// TestGetDERProgramList_NotFound : IEEE-073 recommended per-method case.
// Server returns 404; GetDERProgramList must return a non-nil error
// surfacing both the path and the status, and the outer wrap
// `GET DERProgramList: %w`.
func TestGetDERProgramList_NotFound(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(derProgramListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "derp list not found", http.StatusNotFound)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERProgramList(ctx, derProgramListHref)
	if err == nil {
		t.Fatalf("GetDERProgramList on 404 returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "GET DERProgramList") {
		t.Errorf("error %q missing outer wrap %q", err.Error(), "GET DERProgramList")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	if !strings.Contains(err.Error(), derProgramListHref) {
		t.Errorf("error %q does not surface path %q", err.Error(), derProgramListHref)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// TestGetDERProgramList_MalformedXML : IEEE-073 recommended per-method case.
// Server returns 200 with a non-XML body; GetDERProgramList must wrap the
// xml.SyntaxError through the c.Get + GetDERProgramList chain so errors.As
// succeeds.
func TestGetDERProgramList_MalformedXML(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(derProgramListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write([]byte("<<<not-xml<<<"))
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERProgramList(ctx, derProgramListHref)
	if err == nil {
		t.Fatalf("GetDERProgramList on malformed XML returned nil error; got=%+v", got)
	}
	var syn *xml.SyntaxError
	if !errors.As(err, &syn) {
		t.Errorf("error %q does not unwrap to *xml.SyntaxError (chain: %T)", err.Error(), err)
	}
	if !strings.Contains(err.Error(), "GET DERProgramList") {
		t.Errorf("error %q missing outer GetDERProgramList wrap", err.Error())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// ----- GetDefaultDERControl -------------------------------------------------

// TestGetDefaultDERControl_HappyPath : IEEE-073 per-method happy path.
func TestGetDefaultDERControl_HappyPath(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	want := sep2.DefaultDERControl{
		MRID: "DDERC0001",
	}

	mux := http.NewServeMux()
	mux.HandleFunc(defaultDERControlHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDefaultDERControl(t, w, want)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDefaultDERControl(ctx, defaultDERControlHref)
	if err != nil {
		t.Fatalf("GetDefaultDERControl: %v", err)
	}
	if got.MRID != want.MRID {
		t.Errorf("MRID = %q, want %q", got.MRID, want.MRID)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("DefaultDERControl GET hits = %d, want exactly 1", n)
	}
}

// TestGetDefaultDERControl_EmptyHref : IEEE-073 mandatory implicit case
// (empty-href sentinel). Per IEEE-036's implementation note,
// `GetDefaultDERControl` got the same empty-href guard as the new methods
// for consistency.
func TestGetDefaultDERControl_EmptyHref(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDefaultDERControl(ctx, "")
	if err == nil {
		t.Fatalf("GetDefaultDERControl(ctx, \"\") returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "DefaultDERControl href required") {
		t.Errorf("error %q does not contain %q", err.Error(), "DefaultDERControl href required")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0", n)
	}
}

// TestGetDefaultDERControl_NotFound : IEEE-073 recommended per-method case.
func TestGetDefaultDERControl_NotFound(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(defaultDERControlHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "dderc not found", http.StatusNotFound)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDefaultDERControl(ctx, defaultDERControlHref)
	if err == nil {
		t.Fatalf("GetDefaultDERControl on 404 returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "GET DefaultDERControl") {
		t.Errorf("error %q missing outer wrap %q", err.Error(), "GET DefaultDERControl")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	if !strings.Contains(err.Error(), defaultDERControlHref) {
		t.Errorf("error %q does not surface path %q", err.Error(), defaultDERControlHref)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// TestGetDefaultDERControl_MalformedXML : IEEE-073 recommended per-method case.
func TestGetDefaultDERControl_MalformedXML(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(defaultDERControlHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write([]byte("<<<not-xml<<<"))
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDefaultDERControl(ctx, defaultDERControlHref)
	if err == nil {
		t.Fatalf("GetDefaultDERControl on malformed XML returned nil error; got=%+v", got)
	}
	var syn *xml.SyntaxError
	if !errors.As(err, &syn) {
		t.Errorf("error %q does not unwrap to *xml.SyntaxError (chain: %T)", err.Error(), err)
	}
	if !strings.Contains(err.Error(), "GET DefaultDERControl") {
		t.Errorf("error %q missing outer wrap", err.Error())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// ----- GetDERControlList ----------------------------------------------------

// TestGetDERControlList_HappyPath : IEEE-073 per-method happy path.
func TestGetDERControlList_HappyPath(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	want := sep2.DERControlList{
		ListResource: sep2.ListResource{All: 1, Results: 1},
		DERControl: []sep2.DERControl{
			{RandomizableEvent: sep2.RandomizableEvent{Event: sep2.Event{MRID: "DERC0001"}}},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(derControlListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDERControlListFull(t, w, want)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERControlList(ctx, derControlListHref)
	if err != nil {
		t.Fatalf("GetDERControlList: %v", err)
	}
	if n := len(got.DERControl); n != 1 {
		t.Fatalf("len(DERControl) = %d, want 1", n)
	}
	if got.DERControl[0].MRID != "DERC0001" {
		t.Errorf("DERControl[0].MRID = %q, want %q", got.DERControl[0].MRID, "DERC0001")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("DERControlList GET hits = %d, want exactly 1", n)
	}
}

// TestGetDERControlList_EmptyHref : IEEE-073 mandatory implicit case
// (empty-href sentinel).
func TestGetDERControlList_EmptyHref(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERControlList(ctx, "")
	if err == nil {
		t.Fatalf("GetDERControlList(ctx, \"\") returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "DERControlList href required") {
		t.Errorf("error %q does not contain %q", err.Error(), "DERControlList href required")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0", n)
	}
}

// TestGetDERControlList_NotFound : IEEE-073 recommended per-method case.
func TestGetDERControlList_NotFound(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(derControlListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "derc list not found", http.StatusNotFound)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERControlList(ctx, derControlListHref)
	if err == nil {
		t.Fatalf("GetDERControlList on 404 returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "GET DERControlList") {
		t.Errorf("error %q missing outer wrap", err.Error())
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	if !strings.Contains(err.Error(), derControlListHref) {
		t.Errorf("error %q does not surface path %q", err.Error(), derControlListHref)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// TestGetDERControlList_MalformedXML : IEEE-073 recommended per-method case.
func TestGetDERControlList_MalformedXML(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(derControlListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write([]byte("<<<not-xml<<<"))
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, _, err := client.GetDERControlList(ctx, derControlListHref)
	if err == nil {
		t.Fatalf("GetDERControlList on malformed XML returned nil error; got=%+v", got)
	}
	var syn *xml.SyntaxError
	if !errors.As(err, &syn) {
		t.Errorf("error %q does not unwrap to *xml.SyntaxError (chain: %T)", err.Error(), err)
	}
	if !strings.Contains(err.Error(), "GET DERControlList") {
		t.Errorf("error %q missing outer wrap", err.Error())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// ----- GetDERCurveList ------------------------------------------------------

// TestGetDERCurveList_HappyPath : IEEE-073 per-method happy path.
func TestGetDERCurveList_HappyPath(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	want := sep2.DERCurveList{
		ListResource: sep2.ListResource{All: 1, Results: 1},
		DERCurve: []sep2.DERCurve{
			{Resource: sep2.Resource{Href: derCurveListHref + "/0"}},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(derCurveListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDERCurveList(t, w, want)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, err := client.GetDERCurveList(ctx, derCurveListHref)
	if err != nil {
		t.Fatalf("GetDERCurveList: %v", err)
	}
	if n := len(got.DERCurve); n != 1 {
		t.Fatalf("len(DERCurve) = %d, want 1", n)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("DERCurveList GET hits = %d, want exactly 1", n)
	}
}

// TestGetDERCurveList_EmptyHref : IEEE-073 mandatory implicit case
// (empty-href sentinel).
func TestGetDERCurveList_EmptyHref(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) })

	client, ctx := newDERWalkTestClient(t, mux)
	got, err := client.GetDERCurveList(ctx, "")
	if err == nil {
		t.Fatalf("GetDERCurveList(ctx, \"\") returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "DERCurveList href required") {
		t.Errorf("error %q does not contain %q", err.Error(), "DERCurveList href required")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0", n)
	}
}

// TestGetDERCurveList_NotFound : IEEE-073 recommended per-method case.
func TestGetDERCurveList_NotFound(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(derCurveListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "dercurve list not found", http.StatusNotFound)
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, err := client.GetDERCurveList(ctx, derCurveListHref)
	if err == nil {
		t.Fatalf("GetDERCurveList on 404 returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "GET DERCurveList") {
		t.Errorf("error %q missing outer wrap", err.Error())
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	if !strings.Contains(err.Error(), derCurveListHref) {
		t.Errorf("error %q does not surface path %q", err.Error(), derCurveListHref)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// TestGetDERCurveList_MalformedXML : IEEE-073 recommended per-method case.
func TestGetDERCurveList_MalformedXML(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(derCurveListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write([]byte("<<<not-xml<<<"))
	})

	client, ctx := newDERWalkTestClient(t, mux)
	got, err := client.GetDERCurveList(ctx, derCurveListHref)
	if err == nil {
		t.Fatalf("GetDERCurveList on malformed XML returned nil error; got=%+v", got)
	}
	var syn *xml.SyntaxError
	if !errors.As(err, &syn) {
		t.Errorf("error %q does not unwrap to *xml.SyntaxError (chain: %T)", err.Error(), err)
	}
	if !strings.Contains(err.Error(), "GET DERCurveList") {
		t.Errorf("error %q missing outer wrap", err.Error())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// ----- Paging-hint contract -------------------------------------------------

// TestDERListGets_AppendPagingHint locks the `?l=255` / `&l=255` paging-hint
// contract from the IEEE-036 doc comments on the three list methods. The
// test drives each list method with both a query-less and a query-bearing
// href and asserts on the raw RawQuery the production code emits to the
// transport, so a future cursor-walking change that drops the hint surfaces
// here. GetDefaultDERControl is not a list method and is excluded.
func TestDERListGets_AppendPagingHint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		path        string
		callerHref  string
		wantInQuery string
		call        func(c *inverter.SEP2Client, ctx context.Context, href string) error
	}{
		{
			name:        "GetDERProgramList query-less appends ?l=255",
			path:        derProgramListHref,
			callerHref:  derProgramListHref,
			wantInQuery: "l=255",
			call: func(c *inverter.SEP2Client, ctx context.Context, href string) error {
				_, _, err := c.GetDERProgramList(ctx, href)
				return err
			},
		},
		{
			name:        "GetDERProgramList query-bearing appends &l=255",
			path:        derProgramListHref,
			callerHref:  derProgramListHref + "?s=42",
			wantInQuery: "s=42&l=255",
			call: func(c *inverter.SEP2Client, ctx context.Context, href string) error {
				_, _, err := c.GetDERProgramList(ctx, href)
				return err
			},
		},
		{
			name:        "GetDERControlList query-less appends ?l=255",
			path:        derControlListHref,
			callerHref:  derControlListHref,
			wantInQuery: "l=255",
			call: func(c *inverter.SEP2Client, ctx context.Context, href string) error {
				_, _, err := c.GetDERControlList(ctx, href)
				return err
			},
		},
		{
			name:        "GetDERControlList query-bearing appends &l=255",
			path:        derControlListHref,
			callerHref:  derControlListHref + "?s=42",
			wantInQuery: "s=42&l=255",
			call: func(c *inverter.SEP2Client, ctx context.Context, href string) error {
				_, _, err := c.GetDERControlList(ctx, href)
				return err
			},
		},
		{
			name:        "GetDERCurveList query-less appends ?l=255",
			path:        derCurveListHref,
			callerHref:  derCurveListHref,
			wantInQuery: "l=255",
			call: func(c *inverter.SEP2Client, ctx context.Context, href string) error {
				_, err := c.GetDERCurveList(ctx, href)
				return err
			},
		},
		{
			name:        "GetDERCurveList query-bearing appends &l=255",
			path:        derCurveListHref,
			callerHref:  derCurveListHref + "?s=42",
			wantInQuery: "s=42&l=255",
			call: func(c *inverter.SEP2Client, ctx context.Context, href string) error {
				_, err := c.GetDERCurveList(ctx, href)
				return err
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var seenQuery atomic.Value // string
			mux := http.NewServeMux()
			mux.HandleFunc(tc.path, func(w http.ResponseWriter, r *http.Request) {
				seenQuery.Store(r.URL.RawQuery)
				w.Header().Set("Content-Type", "application/sep+xml")
				// Empty list body that decodes cleanly for each list type:
				// any of the three list types accepts the matching element
				// name with no children.
				switch tc.path {
				case derProgramListHref:
					writeDERProgramList(t, w, sep2.DERProgramList{})
				case derControlListHref:
					writeDERControlListFull(t, w, sep2.DERControlList{})
				case derCurveListHref:
					writeDERCurveList(t, w, sep2.DERCurveList{})
				}
			})

			client, ctx := newDERWalkTestClient(t, mux)
			if err := tc.call(client, ctx, tc.callerHref); err != nil {
				t.Fatalf("call: %v", err)
			}
			got, _ := seenQuery.Load().(string)
			if got != tc.wantInQuery {
				t.Errorf("server saw RawQuery = %q, want %q", got, tc.wantInQuery)
			}
		})
	}
}
