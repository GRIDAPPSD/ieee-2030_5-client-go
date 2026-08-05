package main

// Test coverage for walkDERProgramTree, the FSAList -> DERProgramList ->
// DERProgram subtree walk in main() at cmd/inverterclient/main.go. Direct
// integration testing through the production SEP2 client is
// straightforward: spin a gotls listener, build a real *inverter.SEP2Client
// pointed at it, drive the walker, assert on the cache shape and per-FSA
// behavior.
//
// Cases covered:
//
//  1. TestWalkDERProgramTree_ThreeFSAEnumeration: httptest server returns a
//     3-FSA list, each with a distinct DERProgramListLink; walker traverses
//     all three.
//
//  2. TestWalkDERProgramTree_SixDERProgramsAcrossThreeFSAs: each FSA
//     returns 2 DERPrograms; walker walks all 6, fetches
//     DefaultDERControl + DERControlList + DERCurveList per program.
//     Cache-shape assertion: `len(out) == sum(DERPrograms per FSA)`. The
//     walker preserves all enumerated programs unfiltered (selection
//     happens downstream).
//
//  3. TestWalkDERProgramTree_AbsentDERProgramListLinkSkipsFSA: one FSA has
//     a nil DERProgramListLink; walker skips that FSA and walks the other
//     two.
//
//  4. TestWalkDERProgramTree_DeepFSATopologyFlattens: CSIP V1.2 CORE-010
//     7-level topology. The walker does NOT model topology depth: it
//     walks the FSAs the server presents. Verify a deep-level FSA
//     flattened into a single FSAList is walked correctly with all
//     DERPrograms cached.
//
//  5. TestWalkDERProgramTree_MissingSubtreeLinksTolerated: DERProgram with
//     absent DefaultDERControlLink / DERControlListLink / DERCurveListLink:
//     walker skips that GET, still records the program in the cache. Same
//     continue-on-nil branching as case 3, at a different level.
//
//  6. TestWalkDERProgramTree_GetProgramListErrorWraps: per-FSA
//     transport-error wrapping: when GetDERProgramList fails (404 from
//     the server), walkDERProgramTree returns a non-nil error wrapped
//     with `FSA mRID=%s DERProgramList: %w`. The outer loop in main()
//     converts this to log.Fatalf; exercising the walker directly lets us
//     assert on the wrap shape without terminating the test process.
//     Sibling DefaultDERControl / DERControlList / DERCurveList wraps are
//     not exercised individually because the walker bails on the first
//     error (fail-fast).
//
// The empty-FSAList idle-loop and pollRate-throttling-on-empty-set cases
// live in the Phase 2c outer for-loop inside main(), not here: that loop
// has no function seam yet to drive cfg.CSIP / cfg.AllowUnregistered /
// dcap.PollRate state space directly.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	certs "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

// derWalkTestEnv is the main-package mirror of internal/inverter/ccmTestEnv:
// CA + server cert + device cert written to t.TempDir so a production
// SEP2Client can load them and a production gotls listener can serve them.
// We replicate the env here because the inverter_test package's helper is
// not exported (small duplication accepted per Pike rule 2 : duplication
// across modules is reported; duplication across test files in different
// packages of the same module is allowed when the cost of exporting test
// scaffolding outweighs the dedup benefit).
type derWalkTestEnv struct {
	serverCertPath string
	serverKeyPath  string
	deviceCertPath string
	deviceKeyPath  string
	caCertPath     string
}

func newDERWalkTestEnv(t *testing.T) *derWalkTestEnv {
	t.Helper()

	caCertPEM, caKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "DERWalk Test CA",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	caCert, err := certs.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caKey, err := certs.ParseKeyPEM(caKeyPEM)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}

	serverCertPEM, serverKeyPEM, err := certs.GenerateServerCert(caCert, caKey, certs.ServerCertOptions{
		Hosts:      []string{"127.0.0.1", "localhost"},
		CommonName: "DERWalk Test Server",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("generate server cert: %v", err)
	}

	deviceCertPEM, deviceKeyPEM, err := certs.GenerateDeviceCert(caCert, caKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "DERWALK-TEST-001",
	})
	if err != nil {
		t.Fatalf("generate device cert: %v", err)
	}

	tmpDir := t.TempDir()
	srvCert := filepath.Join(tmpDir, "server.crt")
	srvKey := filepath.Join(tmpDir, "server.key")
	devCert := filepath.Join(tmpDir, "device.crt")
	devKey := filepath.Join(tmpDir, "device.key")
	caPath := filepath.Join(tmpDir, "ca.crt")
	for _, w := range []struct {
		path string
		data []byte
	}{
		{srvCert, serverCertPEM},
		{srvKey, serverKeyPEM},
		{devCert, deviceCertPEM},
		{devKey, deviceKeyPEM},
		{caPath, caCertPEM},
	} {
		if err := os.WriteFile(w.path, w.data, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.path, err)
		}
	}

	return &derWalkTestEnv{
		serverCertPath: srvCert,
		serverKeyPath:  srvKey,
		deviceCertPath: devCert,
		deviceKeyPath:  devKey,
		caCertPath:     caPath,
	}
}

// startDERWalkListener boots a gotls-backed HTTPS server with the handler.
// Mirrors startIdleListener in internal/inverter/idle_test.go. Cleanup is
// wired through t.Cleanup so callers do not need to call the returned stop.
func startDERWalkListener(t *testing.T, env *derWalkTestEnv, handler http.Handler) (serverURL string, stop func()) {
	t.Helper()

	cfg, err := sepTLS.NewCCMServerConfig(env.serverCertPath, env.serverKeyPath, env.caCertPath)
	if err != nil {
		t.Fatalf("NewCCMServerConfig: %v", err)
	}

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	tlsL := gotls.NewListener(tcpL, cfg)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})

	return "https://" + tlsL.Addr().String(), func() {
		_ = srv.Close()
		_ = tlsL.Close()
	}
}

// newDERWalkClient builds a production SEP2Client against the test
// listener.
func newDERWalkClient(t *testing.T, env *derWalkTestEnv, serverURL string) *inverter.SEP2Client {
	t.Helper()
	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  env.deviceCertPath,
		KeyFile:   env.deviceKeyPath,
		CAFile:    env.caCertPath,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}
	return client
}

// writeSepXML encodes any sep2 value to the response writer with the
// production sep+xml MIME type. Single small helper that all fixture
// writers share : keeps the fixture surface focused on the test body,
// not the boilerplate.
func writeSepXML(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode %T: %v", v, err)
	}
}

// derProgramFixturePaths describes the canonical path layout used by the
// fixture handlers: every DERProgram has its own sub-tree under a
// per-FSA root path. Documenting the mRID-uniqueness invariant inline
// (per the Phase 5 phase-doc risk note): tests MUST use unique mRIDs;
// the cache map silently collapses duplicates, which would mask a
// regression as a cache-size mismatch with no clear cause.
type derProgramFixture struct {
	mRID                  string
	defaultDERControlPath string
	derControlListPath    string
	derCurveListPath      string
}

type fsaFixture struct {
	mRID               string
	derProgramListPath string // empty -> no DERProgramListLink on the FSA
	programs           []derProgramFixture
}

// mountFSAFixture installs handlers for one FSA's DERProgramList and the
// per-program subtree GETs, counting hits via the supplied counter map.
// The caller mounts the FSAList itself separately. hitsByPath records how
// many GETs landed on each path (path -> *atomic.Int32).
func mountFSAFixture(t *testing.T, mux *http.ServeMux, fsa fsaFixture, hitsByPath map[string]*atomic.Int32) {
	t.Helper()

	// FSAs without a DERProgramListLink contribute no handlers : the
	// walker skips them via the `fsa.DERProgramListLink == nil` branch
	// without ever calling the network.
	if fsa.derProgramListPath == "" {
		return
	}

	progList := sep2.DERProgramList{
		ListResource: sep2.ListResource{
			All:     uint32(len(fsa.programs)),
			Results: uint32(len(fsa.programs)),
		},
	}
	for _, p := range fsa.programs {
		prog := sep2.DERProgram{
			MRID:    p.mRID,
			Primacy: 1,
		}
		if p.defaultDERControlPath != "" {
			prog.DefaultDERControlLink = &sep2.Link{Href: p.defaultDERControlPath}
		}
		if p.derControlListPath != "" {
			prog.DERControlListLink = &sep2.ListLink{Href: p.derControlListPath}
		}
		if p.derCurveListPath != "" {
			prog.DERCurveListLink = &sep2.ListLink{Href: p.derCurveListPath}
		}
		progList.DERProgram = append(progList.DERProgram, prog)
	}

	registerHandler(t, mux, hitsByPath, fsa.derProgramListPath, func(w http.ResponseWriter, _ *http.Request) {
		writeSepXML(t, w, &progList)
	})

	for _, p := range fsa.programs {
		if p.defaultDERControlPath != "" {
			registerHandler(t, mux, hitsByPath, p.defaultDERControlPath, func(w http.ResponseWriter, _ *http.Request) {
				writeSepXML(t, w, &sep2.DefaultDERControl{MRID: p.mRID + "-DDERC"})
			})
		}
		if p.derControlListPath != "" {
			registerHandler(t, mux, hitsByPath, p.derControlListPath, func(w http.ResponseWriter, _ *http.Request) {
				writeSepXML(t, w, &sep2.DERControlList{})
			})
		}
		if p.derCurveListPath != "" {
			registerHandler(t, mux, hitsByPath, p.derCurveListPath, func(w http.ResponseWriter, _ *http.Request) {
				writeSepXML(t, w, &sep2.DERCurveList{})
			})
		}
	}
}

// registerHandler wraps mux.HandleFunc with hit-counting via an atomic
// counter keyed by the path. Centralizes the hit-counting idiom so the
// per-test body asserts on counts without restating the wrapper.
func registerHandler(t *testing.T, mux *http.ServeMux, hitsByPath map[string]*atomic.Int32, path string, h http.HandlerFunc) {
	t.Helper()
	hitsByPath[path] = new(atomic.Int32)
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		hitsByPath[path].Add(1)
		h(w, r)
	})
}

// buildFSAList constructs a sep2.FunctionSetAssignmentsList from the
// fixture descriptors. Used by tests to feed walkDERProgramTree directly
// (bypassing the FSAList GET: that path is covered by fsalist_test.go).
func buildFSAList(fsas []fsaFixture) sep2.FunctionSetAssignmentsList {
	out := sep2.FunctionSetAssignmentsList{
		ListResource: sep2.ListResource{
			All:     uint32(len(fsas)),
			Results: uint32(len(fsas)),
		},
	}
	for _, f := range fsas {
		fsa := sep2.FunctionSetAssignments{MRID: f.mRID}
		if f.derProgramListPath != "" {
			fsa.DERProgramListLink = &sep2.ListLink{Href: f.derProgramListPath}
		}
		out.FunctionSetAssignments = append(out.FunctionSetAssignments, fsa)
	}
	return out
}

// drive runs walkDERProgramTree against the constructed fixture, returning
// the cache and any walk error. ctx has a 5s timeout : sufficient for any
// integration test in this file (each handler is sub-millisecond).
func drive(t *testing.T, fsaList sep2.FunctionSetAssignmentsList, mux *http.ServeMux) (map[string]sep2.DERProgram, error) {
	t.Helper()
	env := newDERWalkTestEnv(t)
	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	out := make(map[string]sep2.DERProgram)
	err := walkDERProgramTree(ctx, client, fsaList, out)
	return out, err
}

// ----- Case 1: 3-FSA enumeration -----------------------------------

// TestWalkDERProgramTree_ThreeFSAEnumeration : case 1.
//
// httptest server returns a 3-FSA list, each with a DERProgramListLink to
// a distinct path. Each FSA's DERProgramList contains a single DERProgram
// with no subtree links. The walker MUST GET all three DERProgramLists
// (one hit per FSA) and cache 3 DERPrograms total.
func TestWalkDERProgramTree_ThreeFSAEnumeration(t *testing.T) {
	t.Parallel()

	fsas := []fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
			programs: []derProgramFixture{
				{mRID: "PROG-A1"},
			},
		},
		{
			mRID:               "FSA-BRAVO",
			derProgramListPath: "/edev/1/fsa/1/derp",
			programs: []derProgramFixture{
				{mRID: "PROG-B1"},
			},
		},
		{
			mRID:               "FSA-CHARLIE",
			derProgramListPath: "/edev/1/fsa/2/derp",
			programs: []derProgramFixture{
				{mRID: "PROG-C1"},
			},
		},
	}

	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	for _, fsa := range fsas {
		mountFSAFixture(t, mux, fsa, hitsByPath)
	}

	out, err := drive(t, buildFSAList(fsas), mux)
	if err != nil {
		t.Fatalf("walkDERProgramTree: %v", err)
	}
	if got, want := len(out), 3; got != want {
		t.Fatalf("cache size = %d, want %d (one DERProgram per FSA)", got, want)
	}
	for _, fsa := range fsas {
		if n := hitsByPath[fsa.derProgramListPath].Load(); n != 1 {
			t.Errorf("DERProgramList GET hits[%s] = %d, want 1", fsa.derProgramListPath, n)
		}
	}
	for _, want := range []string{"PROG-A1", "PROG-B1", "PROG-C1"} {
		if _, ok := out[want]; !ok {
			t.Errorf("cache missing DERProgram mRID=%q", want)
		}
	}
}

// ----- Cases 2 + 3: six DERPrograms + cache-shape invariant -------

// TestWalkDERProgramTree_SixDERProgramsAcrossThreeFSAs : cases 2
// and 3.
//
// Each of three FSAs returns 2 DERPrograms (6 total). Every DERProgram
// has all three subtree links (DefaultDERControl / DERControlList /
// DERCurveList). Assertions:
//
//   - case 2 (enumeration): 6 DERPrograms walked total.
//   - case 2 (subtree fetch): 6 DefaultDERControl GETs, 6 DERControlList
//     GETs, 6 DERCurveList GETs.
//   - case 3 (cache shape): len(out) == 6 : unfiltered, preserves all
//     enumerated programs so Primacy + mRID selection can run on
//     a complete set. mRID uniqueness across all 6 programs is the
//     fixture invariant; if a future edit accidentally reuses an mRID
//     the cache will silently collapse and this test will fail.
func TestWalkDERProgramTree_SixDERProgramsAcrossThreeFSAs(t *testing.T) {
	t.Parallel()

	makeProg := func(name string) derProgramFixture {
		// Unique paths per program : mRID uniqueness is the test invariant
		// the Phase 5 phase-doc calls out as a fixture-author footgun.
		return derProgramFixture{
			mRID:                  name,
			defaultDERControlPath: "/dderc/" + name,
			derControlListPath:    "/derc/" + name,
			derCurveListPath:      "/dercurve/" + name,
		}
	}

	fsas := []fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
			programs:           []derProgramFixture{makeProg("PROG-A1"), makeProg("PROG-A2")},
		},
		{
			mRID:               "FSA-BRAVO",
			derProgramListPath: "/edev/1/fsa/1/derp",
			programs:           []derProgramFixture{makeProg("PROG-B1"), makeProg("PROG-B2")},
		},
		{
			mRID:               "FSA-CHARLIE",
			derProgramListPath: "/edev/1/fsa/2/derp",
			programs:           []derProgramFixture{makeProg("PROG-C1"), makeProg("PROG-C2")},
		},
	}

	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	for _, fsa := range fsas {
		mountFSAFixture(t, mux, fsa, hitsByPath)
	}

	out, err := drive(t, buildFSAList(fsas), mux)
	if err != nil {
		t.Fatalf("walkDERProgramTree: %v", err)
	}

	// Case 2 enumeration + Case 3 cache shape:
	if got, want := len(out), 6; got != want {
		t.Fatalf("cache size = %d, want %d (sum DERPrograms per FSA : unfiltered)", got, want)
	}

	// Case 3 (per-mRID assertion : preserves all enumerated programs):
	wantMRIDs := []string{"PROG-A1", "PROG-A2", "PROG-B1", "PROG-B2", "PROG-C1", "PROG-C2"}
	for _, mrid := range wantMRIDs {
		if _, ok := out[mrid]; !ok {
			t.Errorf("cache missing DERProgram mRID=%q (case 3 cache-shape regression)", mrid)
		}
	}

	// Case 2 (FSA-list-level GETs : exactly one per FSA):
	for _, fsa := range fsas {
		if n := hitsByPath[fsa.derProgramListPath].Load(); n != 1 {
			t.Errorf("DERProgramList GET hits[%s] = %d, want 1", fsa.derProgramListPath, n)
		}
	}

	// Case 2 (per-program subtree GETs : one per program per subtree link):
	for _, fsa := range fsas {
		for _, p := range fsa.programs {
			if n := hitsByPath[p.defaultDERControlPath].Load(); n != 1 {
				t.Errorf("DefaultDERControl hits[%s] = %d, want 1", p.defaultDERControlPath, n)
			}
			if n := hitsByPath[p.derControlListPath].Load(); n != 1 {
				t.Errorf("DERControlList hits[%s] = %d, want 1", p.derControlListPath, n)
			}
			if n := hitsByPath[p.derCurveListPath].Load(); n != 1 {
				t.Errorf("DERCurveList hits[%s] = %d, want 1", p.derCurveListPath, n)
			}
		}
	}
}

// ----- Case 5: absent DERProgramListLink branches -----------------

// TestWalkDERProgramTree_AbsentDERProgramListLinkSkipsFSA : case 5.
//
// Three FSAs, but the middle FSA's DERProgramListLink is nil (the FSA-
// list-builder leaves the field nil when derProgramListPath == ""). The
// walker MUST skip that FSA without making any GET, and still walk the
// other two. Assertions:
//
//   - The middle FSA's nominal path is never hit (registerHandler was
//     never called for it; using the inverse : assert on the count of
//     keys in hitsByPath).
//   - 2 DERPrograms cached, NOT 3.
//   - Neither program in the cache is the missing-FSA's "phantom"
//     program (the fixture has no phantom : the FSA has no programs to
//     skip; this is the per-FSA branch only).
func TestWalkDERProgramTree_AbsentDERProgramListLinkSkipsFSA(t *testing.T) {
	t.Parallel()

	fsas := []fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
			programs:           []derProgramFixture{{mRID: "PROG-A1"}},
		},
		{
			mRID:               "FSA-BRAVO-NIL-LINK",
			derProgramListPath: "", // <-- nil DERProgramListLink
		},
		{
			mRID:               "FSA-CHARLIE",
			derProgramListPath: "/edev/1/fsa/2/derp",
			programs:           []derProgramFixture{{mRID: "PROG-C1"}},
		},
	}

	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	for _, fsa := range fsas {
		mountFSAFixture(t, mux, fsa, hitsByPath)
	}

	out, err := drive(t, buildFSAList(fsas), mux)
	if err != nil {
		t.Fatalf("walkDERProgramTree: %v", err)
	}
	if got, want := len(out), 2; got != want {
		t.Fatalf("cache size = %d, want %d (middle FSA contributes zero)", got, want)
	}
	if _, ok := out["PROG-A1"]; !ok {
		t.Errorf("cache missing PROG-A1 (first FSA still walked)")
	}
	if _, ok := out["PROG-C1"]; !ok {
		t.Errorf("cache missing PROG-C1 (third FSA still walked)")
	}
	// The middle FSA never registered a handler : the only way it could
	// have generated traffic is if walkDERProgramTree tried to GET a
	// blank href, which c.Get would have routed to the server's root.
	// Assert hitsByPath has exactly 2 entries (the two non-nil FSAs).
	if got, want := len(hitsByPath), 2; got != want {
		t.Errorf("registered handler count = %d, want %d (middle FSA must not register)", got, want)
	}
}

// ----- Case 7: deep-topology FSA flattens -------------------------

// TestWalkDERProgramTree_DeepFSATopologyFlattens : case 7.
//
// CSIP V1.2 CORE-010 7-level FSA topology is a server-side concept : the
// walker does not model topology depth (per Phase 5 phase-doc risk note,
// "the walk does NOT model the topology levels in code; it just walks the
// FSAs that the server presents"). The 7-level reference flattens to a
// single FSAList from the device's perspective.
//
// To exercise "deepest-level FSA walked correctly," construct a fixture
// with 7 FSAs (representing the deepest-level enumeration the server
// could present) and walk it. Each FSA contributes one DERProgram :
// 7 cached programs total.
func TestWalkDERProgramTree_DeepFSATopologyFlattens(t *testing.T) {
	t.Parallel()

	fsas := make([]fsaFixture, 7)
	for i := range fsas {
		fsas[i] = fsaFixture{
			mRID:               fmt.Sprintf("FSA-LEVEL-%d", i),
			derProgramListPath: fmt.Sprintf("/edev/1/fsa/%d/derp", i),
			programs: []derProgramFixture{
				{mRID: fmt.Sprintf("PROG-LEVEL-%d", i)},
			},
		}
	}

	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	for _, fsa := range fsas {
		mountFSAFixture(t, mux, fsa, hitsByPath)
	}

	out, err := drive(t, buildFSAList(fsas), mux)
	if err != nil {
		t.Fatalf("walkDERProgramTree: %v", err)
	}
	if got, want := len(out), 7; got != want {
		t.Fatalf("cache size = %d, want %d (deepest-level FSA flattens to 7-element list)", got, want)
	}
	for i := 0; i < 7; i++ {
		mrid := fmt.Sprintf("PROG-LEVEL-%d", i)
		if _, ok := out[mrid]; !ok {
			t.Errorf("cache missing %q (level-%d FSA must contribute its program)", mrid, i)
		}
	}
}

// ----- Phase 5 phase-doc invariant: missing subtree links tolerated --------

// TestWalkDERProgramTree_MissingSubtreeLinksTolerated : Phase 5 phase-doc
// invariant.
//
// A DERProgram with all three subtree links nil (no DefaultDERControlLink,
// no DERControlListLink, no DERCurveListLink) still gets recorded in the
// cache. The walker MUST NOT short-circuit the program on a fully-
// linkless DERProgram and MUST NOT make any subtree GETs.
func TestWalkDERProgramTree_MissingSubtreeLinksTolerated(t *testing.T) {
	t.Parallel()

	fsas := []fsaFixture{
		{
			mRID:               "FSA-ALPHA",
			derProgramListPath: "/edev/1/fsa/0/derp",
			programs: []derProgramFixture{
				{mRID: "PROG-NO-LINKS"}, // all subtree paths empty
			},
		},
	}

	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	for _, fsa := range fsas {
		mountFSAFixture(t, mux, fsa, hitsByPath)
	}

	out, err := drive(t, buildFSAList(fsas), mux)
	if err != nil {
		t.Fatalf("walkDERProgramTree: %v", err)
	}
	if _, ok := out["PROG-NO-LINKS"]; !ok {
		t.Fatalf("cache missing PROG-NO-LINKS (linkless program must still be recorded)")
	}
	// Only the DERProgramList path itself should have been hit : no
	// subtree handler was registered.
	if got, want := len(hitsByPath), 1; got != want {
		t.Errorf("hit-counter map size = %d, want %d (only DERProgramList GET expected)", got, want)
	}
	if n := hitsByPath["/edev/1/fsa/0/derp"].Load(); n != 1 {
		t.Errorf("DERProgramList hits = %d, want 1", n)
	}
}

// ----- Walker error-wrap shape ---------------------------------------------

// TestWalkDERProgramTree_GetProgramListErrorWraps locks the
// `FSA mRID=%s DERProgramList: %w` wrap shape on transport failure. The
// outer Phase 2c for-loop in main() calls log.Fatalf with this error;
// the assertion here ensures the wrap message includes the failing FSA's
// mRID (so the operator can identify which FSA tripped the walk) plus
// the underlying status code from the client layer.
func TestWalkDERProgramTree_GetProgramListErrorWraps(t *testing.T) {
	t.Parallel()

	fsas := []fsaFixture{
		{
			mRID:               "FSA-WILL-404",
			derProgramListPath: "/edev/1/fsa/0/derp",
			// programs intentionally empty : the server will 404 the GET
		},
	}

	mux := http.NewServeMux()
	hitsByPath := map[string]*atomic.Int32{}
	registerHandler(t, mux, hitsByPath, "/edev/1/fsa/0/derp", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "derp list not found", http.StatusNotFound)
	})

	out, err := drive(t, buildFSAList(fsas), mux)
	if err == nil {
		t.Fatalf("walkDERProgramTree on 404 returned nil error; cache=%+v", out)
	}
	if !strings.Contains(err.Error(), "FSA mRID=FSA-WILL-404 DERProgramList") {
		t.Errorf("error %q missing per-FSA wrap `FSA mRID=FSA-WILL-404 DERProgramList`", err.Error())
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	// Confirm the wrap chain unwraps cleanly via errors.Unwrap : not
	// asserting on a specific sentinel type (none exists in the production
	// code), just that the chain is non-nil so callers can errors.As / Is.
	if errors.Unwrap(err) == nil {
		t.Errorf("error chain has no inner error (production code uses %%w, this should never happen)")
	}
}
