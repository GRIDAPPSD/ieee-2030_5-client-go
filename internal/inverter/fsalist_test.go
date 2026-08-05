package inverter_test

// Package inverter_test covers
// (*SEP2Client).GetFSAList. This behavior is frozen.
//
// Reuses the ccmTestEnv from
// client_ccm_test.go and startIdleListener from idle_test.go. The test does
// not need TLS-cipher negotiation coverage
// (GRIDAPPSD/ieee-2030_5-server-go#62 owns that); it needs
// FSAList XML parsing, empty-href sentinel error, 404 / malformed-XML
// wrapping, and the `?l=255` paging-hint contract.
//
// Cases covered:
//
//  1. TestGetFSAList_HappyPath          : stub returns
//                                          FSAList with 3 entries; method
//                                          returns the parsed value.
//  2. TestGetFSAList_EmptyHref          : empty href returns
//                                          error matching "FSAList href
//                                          required", no GET attempted.
//  3. TestGetFSAList_NotFound           : server 404
//                                          produces a wrapped error (path +
//                                          status surfaced), no panic.
//  4. TestGetFSAList_MalformedXML       : server emits
//                                          non-XML body; GetFSAList returns
//                                          an unwrappable xml.SyntaxError
//                                          (via %w from c.Get).
//  5. TestGetFSAList_AppendsPagingHint  : doc-comment contract:
//                                          method appends `?l=255` on a
//                                          query-less href and `&l=255` on
//                                          a query-bearing href.
//
// Phase 2c block behavior (empty-list idle, missing-link
// fatal, --allow-unregistered bypass, ctx-cancel cleanliness) lives inside
// `cmd/inverterclient/main.go`'s `main()` body where it is not addressable
// from a test binary (the fatal-link path calls `log.Fatalf` and there is no
// function seam through which a test can drive the FunctionSetAssignmentsListLink
// / cfg.CSIP / cfg.AllowUnregistered state space).

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// fsaListHref is the canonical fixture path for the FSAList resource. CSIP
// V1.2 CORE-012 step 1 binds FSAList off EndDevice.FunctionSetAssignmentsListLink;
// the literal path is server-chosen : `/edev/1/fsa` matches the convention
// used in the sep2server function-set router.
const fsaListHref = "/edev/1/fsa"

// writeFSAList encodes a FunctionSetAssignmentsList to the response writer
// with the SEP+XML content type. Mirrors writeRegistration from
// registration_test.go and writeDcap from idle_test.go.
func writeFSAList(t *testing.T, w http.ResponseWriter, list sep2.FunctionSetAssignmentsList) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&list); err != nil {
		t.Errorf("encode FSAList: %v", err)
	}
}

// newFSAListTestClient is a small helper that boots a gotls server with the
// supplied handler, builds a production SEP2 client against it, and returns
// both for the caller to drive. Cleanup is wired through t.Cleanup via
// startIdleListener (server) and the client (no explicit Close needed : it
// shares the test's CA pool). Mirrors newRegistrationTestClient.
func newFSAListTestClient(t *testing.T, handler http.Handler) (*inverter.SEP2Client, context.Context) {
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

// TestGetFSAList_HappyPath : case 1.
//
// Stub returns FunctionSetAssignmentsList with 3 FSA entries : each with a
// distinct mRID + description + DERProgramListLink : and a non-zero All
// attribute on the ListResource. GetFSAList must return all three entries
// in order with every field parsed.
func TestGetFSAList_HappyPath(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	want := sep2.FunctionSetAssignmentsList{
		ListResource: sep2.ListResource{
			All:     3,
			Results: 3,
		},
		FunctionSetAssignments: []sep2.FunctionSetAssignments{
			{
				MRID:               "AAAA0000",
				Description:        "FSA Alpha",
				DERProgramListLink: &sep2.ListLink{Href: "/edev/1/fsa/0/derp"},
			},
			{
				MRID:               "BBBB0001",
				Description:        "FSA Bravo",
				DERProgramListLink: &sep2.ListLink{Href: "/edev/1/fsa/1/derp"},
			},
			{
				MRID:               "CCCC0002",
				Description:        "FSA Charlie",
				DERProgramListLink: &sep2.ListLink{Href: "/edev/1/fsa/2/derp"},
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(fsaListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeFSAList(t, w, want)
	})

	client, ctx := newFSAListTestClient(t, mux)
	got, _, err := client.GetFSAList(ctx, fsaListHref)
	if err != nil {
		t.Fatalf("GetFSAList: %v", err)
	}
	if n := len(got.FunctionSetAssignments); n != 3 {
		t.Fatalf("len(FunctionSetAssignments) = %d, want 3", n)
	}
	for i, w := range want.FunctionSetAssignments {
		g := got.FunctionSetAssignments[i]
		if g.MRID != w.MRID {
			t.Errorf("entry[%d].MRID = %q, want %q", i, g.MRID, w.MRID)
		}
		if g.Description != w.Description {
			t.Errorf("entry[%d].Description = %q, want %q", i, g.Description, w.Description)
		}
		if g.DERProgramListLink == nil {
			t.Errorf("entry[%d].DERProgramListLink = nil, want non-nil", i)
			continue
		}
		if g.DERProgramListLink.Href != w.DERProgramListLink.Href {
			t.Errorf("entry[%d].DERProgramListLink.Href = %q, want %q",
				i, g.DERProgramListLink.Href, w.DERProgramListLink.Href)
		}
	}
	if got.All != want.All {
		t.Errorf("All = %d, want %d", got.All, want.All)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("FSAList GET hits = %d, want exactly 1", n)
	}
}

// TestGetFSAList_EmptyHref : case 2.
//
// An empty href is rejected at the call site without an HTTP round trip.
// The sentinel error string is contractual: the Phase 2c block in main()
// surfaces this exact form via `log.Fatalf("GET FSAList: %v", err)` when
// FunctionSetAssignmentsListLink resolves to an empty href. Future callers
// grepping this message must keep it intact.
func TestGetFSAList_EmptyHref(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	})

	client, ctx := newFSAListTestClient(t, mux)
	got, _, err := client.GetFSAList(ctx, "")
	if err == nil {
		t.Fatalf("GetFSAList(ctx, \"\") returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "FSAList href required") {
		t.Errorf("error %q does not contain %q", err.Error(), "FSAList href required")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server hits = %d, want 0 (empty-href must short-circuit before any GET)", n)
	}
}

// TestGetFSAList_NotFound : case 3.
//
// Server returns 404 with a small body. GetFSAList must return a non-nil
// error that surfaces both the path and the status code. The outer wrap
// added by GetFSAList is `GET FSAList: %w`; the inner wrap from c.Get is
// `GET %s: %d %s` (not %w on the status : see registration_test.go's
// matching case for context). Assert on "wrapped error, no panic":
// the path AND outer wrap are present and the test binary survives.
func TestGetFSAList_NotFound(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(fsaListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "fsa list not found", http.StatusNotFound)
	})

	client, ctx := newFSAListTestClient(t, mux)
	got, _, err := client.GetFSAList(ctx, fsaListHref)
	if err == nil {
		t.Fatalf("GetFSAList on 404 returned nil error; got=%+v", got)
	}
	if !strings.Contains(err.Error(), "GET FSAList") {
		t.Errorf("error %q missing outer wrap %q", err.Error(), "GET FSAList")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	if !strings.Contains(err.Error(), fsaListHref) {
		t.Errorf("error %q does not surface path %q", err.Error(), fsaListHref)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// TestGetFSAList_MalformedXML : case 4.
//
// Server returns 200 OK with non-XML body. GetFSAList must wrap the
// xml.SyntaxError via the chain
//   - c.Get:       fmt.Errorf("unmarshal %s: %w", path, err)
//   - GetFSAList:  fmt.Errorf("GET FSAList: %w", err)
//
// so errors.As to *xml.SyntaxError succeeds and no panic crashes the test.
func TestGetFSAList_MalformedXML(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(fsaListHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write([]byte("<<<not-xml<<<"))
	})

	client, ctx := newFSAListTestClient(t, mux)
	got, _, err := client.GetFSAList(ctx, fsaListHref)
	if err == nil {
		t.Fatalf("GetFSAList on malformed XML returned nil error; got=%+v", got)
	}
	var syn *xml.SyntaxError
	if !errors.As(err, &syn) {
		t.Errorf("error %q does not unwrap to *xml.SyntaxError (chain: %T)", err.Error(), err)
	}
	if !strings.Contains(err.Error(), "GET FSAList") {
		t.Errorf("error %q missing outer GetFSAList wrap", err.Error())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want exactly 1", n)
	}
}

// TestGetFSAList_AppendsPagingHint locks in the `?l=255` paging-hint contract
// from the GetFSAList doc comment ("First-cut paging: appends `?l=255` to fetch
// the first page; cursor walking for lists larger than 255 entries is
// deferred to a follow-up"). Two cases via subtests:
//
//   - query-less href ("/edev/1/fsa") → server receives "?l=255"
//   - query-bearing href ("/edev/1/fsa?s=42") → server receives "&l=255"
//
// Both subtests assert on the raw req.URL.RawQuery the production code emits
// so a future cursor-walking change that drops the hint surfaces here.
func TestGetFSAList_AppendsPagingHint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		callerHref  string
		wantInQuery string
	}{
		{
			name:        "query-less href appends ?l=255",
			callerHref:  fsaListHref,
			wantInQuery: "l=255",
		},
		{
			name:        "query-bearing href appends &l=255",
			callerHref:  fsaListHref + "?s=42",
			wantInQuery: "s=42&l=255",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var seenQuery atomic.Value // string
			mux := http.NewServeMux()
			mux.HandleFunc(fsaListHref, func(w http.ResponseWriter, r *http.Request) {
				seenQuery.Store(r.URL.RawQuery)
				writeFSAList(t, w, sep2.FunctionSetAssignmentsList{})
			})

			client, ctx := newFSAListTestClient(t, mux)
			if _, _, err := client.GetFSAList(ctx, tc.callerHref); err != nil {
				t.Fatalf("GetFSAList: %v", err)
			}
			got, _ := seenQuery.Load().(string)
			if got != tc.wantInQuery {
				t.Errorf("server saw RawQuery = %q, want %q", got, tc.wantInQuery)
			}
		})
	}
}
