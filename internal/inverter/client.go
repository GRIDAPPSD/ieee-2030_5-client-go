package inverter

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-go/internal/tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-go/internal/tls/gotls"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// ErrEndDeviceNotFound is returned by LookupOwnEndDevice when the server's
// EndDeviceList does not contain an entry whose LFDI matches the client's.
// Callers in CSIP mode treat this as a transient "not yet provisioned"
// condition and re-poll the list rather than aborting. See IEEE-029.
var ErrEndDeviceNotFound = errors.New("end device not found in server list")

// ErrResponseTransient is the sentinel error returned by PostResponse when
// the server reports a transient failure (5xx, or a transport-level error
// that survives the one-shot in-call retry). Phase 6 / IEEE-045 will
// `errors.Is`-match against this to drive the exponential-backoff +
// dead-letter retry policy without coupling to status-code parsing. The
// sentinel is intentionally a bare leaf error; PostResponse wraps it with
// %w plus contextual detail (status code, URL) so callers retain both the
// pattern-match handle and the diagnostic chain. See IEEE-043 / IEEE-045.
var ErrResponseTransient = errors.New("response POST transient failure")

const (
	contentTypeSEPXML = "application/sep+xml"
	keepAliveTimeout  = "timeout=30, max=1000"
)

// SEP2Client is an IEEE 2030.5 HTTP client with mTLS and persistent connections.
//
// serverTimeOffsetNanos holds the signed nanosecond offset (server_now -
// local_now) discovered by the time-sync goroutine. atomic.Int64 lets the
// sync goroutine Store while the simulation/reporter Load with no lock.
// Zero offset (the zero value) is the safe default — Now() degrades to
// time.Now() before the first sync completes or when no TimeLink is
// advertised. See IEEE-031.
type SEP2Client struct {
	httpClient            *http.Client
	baseURL               string
	sfdi                  string
	lfdi                  string
	serverTimeOffsetNanos atomic.Int64
}

// NewSEP2Client creates a client with mTLS persistent connections per IEEE 2030.5.
//
// The client uses the vendored gotls fork rather than stdlib crypto/tls so it
// can negotiate TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 (0xC0AE), the IEEE 2030.5
// mandatory cipher. By default GCM is left in the cipher list as a fallback
// so the simulator keeps working against permissive peers; setting
// cfg.CSIPStrict drops the GCM entry and forces the handshake to fail
// loudly against non-CSIP-conformant peers.
func NewSEP2Client(cfg SimConfig) (*SEP2Client, error) {
	certPEM, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("read client cert %q: %w", cfg.CertFile, err)
	}
	keyPEM, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read client key %q: %w", cfg.KeyFile, err)
	}
	cert, err := gotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client cert %q: %w", cfg.CertFile, err)
	}

	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", cfg.CAFile, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA cert %q: no PEM data", cfg.CAFile)
	}

	cipherSuites := []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8}
	if !cfg.CSIPStrict {
		// 0xC02B is TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256. Mirrors the
		// fallback the server registers in internal/tls/ccmserver.go so
		// `make run-inverter` against `make run-ccm` keeps interoperating.
		cipherSuites = append(cipherSuites, gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
	}

	tlsCfg := &gotls.Config{
		Certificates: []gotls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   gotls.VersionTLS12,
		MaxVersion:   gotls.VersionTLS12,
		CipherSuites: cipherSuites,
		// IEEE 2030.5 / CSIP §6.11 server certs carry a critical
		// HardwareModuleName SAN (otherName OID 1.3.6.1.5.5.7.8.4) that the
		// stdlib x509 parser leaves in UnhandledCriticalExtensions. The
		// gotls client-side handshake always runs stdlib Verify before
		// invoking VerifyPeerCertificate (handshake_client.go:985-1002), so
		// merely adding the hook is not enough — stdlib's pre-verify is
		// what trips `unhandled critical extension`. Set InsecureSkipVerify
		// to bypass that pre-verify, and do the chain walk ourselves in the
		// hook via the shared HMN-tolerant helper. This is NOT
		// `--insecure-skip-verify`; the hook performs full chain validation
		// against RootCAs. CSIP §6.11 device-profile certs have empty
		// Subject and an otherName-only SAN, so stdlib hostname
		// verification cannot succeed against them in any case; the helper
		// matches the existing server-side enforcement scope. See IEEE-027.
		InsecureSkipVerify: true, //nolint:gosec // see comment above
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return sepTLS.VerifyPeerCertWithHardwareModuleSAN(rawCerts, caPool)
		},
		CurvePreferences: []gotls.CurveID{gotls.CurveP256},
	}

	// Derive SFDI/LFDI from the leaf cert that X509KeyPair already parsed.
	// Reading the cert file a second time and re-decoding the PEM (the
	// previous behavior) was redundant and discarded errors from both
	// os.ReadFile and pem.Decode, leaving a nil-pointer deref on the next
	// line if either failed (IEEE-008).
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("client cert %q has no leaf certificate", cfg.CertFile)
	}
	parsedCert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse client cert %q for identity: %w", cfg.CertFile, err)
	}

	transport := &http.Transport{
		// gotls.Conn implements net.Conn so this composes cleanly with the
		// stdlib http.Transport. We deliberately do NOT set TLSClientConfig
		// here — stdlib's transport would try to use crypto/tls against it.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &gotls.Dialer{Config: tlsCfg}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
	}

	return &SEP2Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		baseURL: cfg.ServerURL,
		sfdi:    sepTLS.SFDI(parsedCert),
		lfdi:    sepTLS.LFDI(parsedCert),
	}, nil
}

// SFDI returns the client's Short Form Device Identifier.
func (c *SEP2Client) SFDI() string { return c.sfdi }

// LFDI returns the client's Long Form Device Identifier.
func (c *SEP2Client) LFDI() string { return c.lfdi }

// Get performs a GET request and unmarshals the XML response.
func (c *SEP2Client) Get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", contentTypeSEPXML)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", keepAliveTimeout)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, string(body))
	}

	if out != nil {
		if err := xml.Unmarshal(body, out); err != nil {
			return fmt.Errorf("unmarshal %s: %w", path, err)
		}
	}
	return nil
}

// Post performs a POST request with XML body and returns the Location header.
func (c *SEP2Client) Post(ctx context.Context, path string, body any) (string, error) {
	data, err := xml.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentTypeSEPXML)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", keepAliveTimeout)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // drain for connection reuse

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: %d", path, resp.StatusCode)
	}

	return resp.Header.Get("Location"), nil
}

// Put performs a PUT request with XML body.
func (c *SEP2Client) Put(ctx context.Context, path string, body any) error {
	data, err := xml.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentTypeSEPXML)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", keepAliveTimeout)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s: %d", path, resp.StatusCode)
	}
	return nil
}

// Discover fetches the DeviceCapability (entry point).
func (c *SEP2Client) Discover(ctx context.Context) (sep2.DeviceCapability, error) {
	var dcap sep2.DeviceCapability
	err := c.Get(ctx, "/dcap", &dcap)
	return dcap, err
}

// Register creates an EndDevice on the server by POSTing to the
// EndDeviceList href advertised in DeviceCapability. The href is passed in
// rather than baked in as a constant — per IEEE 2030.5 §10.3 / CSIP §6.6 a
// client MUST traverse the link graph reachable from /dcap and never assume
// URL shapes. See IEEE-030.
//
// IEEE-030 tests deferred per Craig override 2026-05-12 (time crunch).
// Required-but-deferred coverage:
//  1. Register(ctx, edevListHref) POSTs to the exact passed href (httptest
//     assertion on req.URL.Path), returns the parsed EndDevice from the
//     Location response.
//  2. Empty href argument → error, no HTTP call.
func (c *SEP2Client) Register(ctx context.Context, edevListHref string) (sep2.EndDevice, error) {
	if edevListHref == "" {
		return sep2.EndDevice{}, fmt.Errorf("edev list href required")
	}

	edev := sep2.EndDevice{SFDI: c.sfdi, LFDI: c.lfdi}
	enabled := true
	edev.Enabled = &enabled

	loc, err := c.Post(ctx, edevListHref, &edev)
	if err != nil {
		return sep2.EndDevice{}, err
	}

	// Read back the registered device
	var registered sep2.EndDevice
	if loc != "" {
		err = c.Get(ctx, loc, &registered)
	}
	return registered, err
}

// LookupOwnEndDevice GETs the EndDeviceList at edevListHref and returns the
// EndDevice whose LFDI matches the client's. This is the CSIP discovery
// path: per CSIP §6.7 / IEEE 2030.5 §10.5, CSIP devices are pre-allowlisted
// out-of-band by LFDI; the device's job is to find its own EndDevice in the
// server's list, not to POST one. Use this instead of Register when the
// server provisions devices ahead of time.
//
// Returns ErrEndDeviceNotFound when the list does not contain the client's
// LFDI (callers idle and re-poll in that case — see cmd/inverterclient).
// All underlying transport/decode failures are wrapped with %w.
//
// LFDI match is exact case-sensitive string equality on the upper-hex 40-char
// form produced by internal/tls.LFDI (`fmt.Sprintf("%X", ...)`); the server
// stores LFDIs the same way.
//
// First-cut paging: appends `?l=255` to fetch the first page. Cursor walking
// for lists larger than 255 entries is deferred to a follow-up ticket
// (noted in IEEE-029).
//
// IEEE-029 tests deferred per Craig override 2026-05-12 (time crunch).
// Required-but-deferred coverage (must be written before next backlog sweep):
//  1. --csip on, server list contains our LFDI → succeeds; Phase 3 fires once.
//  2. --csip on, server list empty → ErrEndDeviceNotFound; caller idle-loops;
//     zero PUTs/POSTs on /edev/{id}/* or /mup while idling.
//  3. --csip on, server list contains other LFDIs but not ours → ErrEndDeviceNotFound.
//  4. --csip off → existing Register POST still fires /edev (no regression).
//  5. Cursor paging: list > 255 entries (follow-up ticket; not filed yet).
func (c *SEP2Client) LookupOwnEndDevice(ctx context.Context, edevListHref string) (sep2.EndDevice, error) {
	if edevListHref == "" {
		return sep2.EndDevice{}, fmt.Errorf("edev list href required")
	}

	// First-cut paging: limit=255 on the first page.
	sep := "?"
	if strings.Contains(edevListHref, "?") {
		sep = "&"
	}
	path := edevListHref + sep + "l=255"

	var list sep2.EndDeviceList
	if err := c.Get(ctx, path, &list); err != nil {
		return sep2.EndDevice{}, fmt.Errorf("get edev list: %w", err)
	}

	for _, ed := range list.EndDevice {
		if ed.LFDI == c.lfdi {
			return ed, nil
		}
	}
	return sep2.EndDevice{}, ErrEndDeviceNotFound
}

// GetRegistration GETs the server-provided Registration resource at the given
// href and decodes it. The Registration resource carries the server-assigned
// pIN that CSIP V1.2 BASIC-001 step 5 / IEEE 2030.5 §10 expect the device to
// match against its out-of-band provisioned PIN. IEEE-032 lands the read;
// IEEE-033 will add mismatch enforcement and idle-retry on a not-yet-
// provisioned PIN; IEEE-034 will add strict-mode missing-RegistrationLink
// behavior.
//
// IEEE-032 tests deferred per Craig override 2026-05-12 (time crunch).
// Required-but-deferred coverage:
//   - happy path: stub server returns Registration with known PIN; method
//     returns it parsed.
//   - empty href: returns error matching "registration href required".
//   - server returns 404: error wrapped via c.Get, no panic.
//   - malformed XML: error wrapped via c.Get, no panic.
func (c *SEP2Client) GetRegistration(ctx context.Context, registrationHref string) (sep2.Registration, error) {
	if registrationHref == "" {
		return sep2.Registration{}, fmt.Errorf("registration href required")
	}
	var rg sep2.Registration
	if err := c.Get(ctx, registrationHref, &rg); err != nil {
		return sep2.Registration{}, fmt.Errorf("GET registration: %w", err)
	}
	return rg, nil
}

// GetFSAList GETs the FunctionSetAssignmentsList at the given href and decodes
// it. The FSAList carries the function-set assignments (DERProgramListLink,
// UsagePointListLink, DemandResponseProgramListLink) the server has bound to
// this EndDevice. CSIP V1.2 CORE-012 step 1 — first move after the device is
// confirmed commissioned. The tree walk per FSA (DERProgramList enumeration)
// and Primacy + mRID program selection are deferred to IEEE-036 and IEEE-037
// respectively (plan-1-csip-client-conformance phase 4).
//
// First-cut paging: appends `?l=255` to fetch the first page; cursor walking
// for lists larger than 255 entries is deferred to a follow-up. Mirrors the
// paging pattern in LookupOwnEndDevice (IEEE-029).
//
// IEEE-035 tests deferred per Craig override 2026-05-12. Required coverage:
//  1. Happy path: stub server returns FSAList with N entries; method returns
//     the parsed list intact.
//  2. Empty href: returns error matching "FSAList href required"; no HTTP call.
//  3. Server 404: error wrapped via c.Get, no panic.
//  4. Malformed XML: error wrapped via c.Get, no panic.
//  5. Pagination cap: list with > 255 entries — first 255 returned, rest
//     deferred to cursor follow-up (no silent drop documented).
func (c *SEP2Client) GetFSAList(ctx context.Context, fsaListHref string) (sep2.FunctionSetAssignmentsList, error) {
	if fsaListHref == "" {
		return sep2.FunctionSetAssignmentsList{}, fmt.Errorf("FSAList href required")
	}
	sep := "?"
	if strings.Contains(fsaListHref, "?") {
		sep = "&"
	}
	path := fsaListHref + sep + "l=255"
	var list sep2.FunctionSetAssignmentsList
	if err := c.Get(ctx, path, &list); err != nil {
		return sep2.FunctionSetAssignmentsList{}, fmt.Errorf("GET FSAList: %w", err)
	}
	return list, nil
}

// PutDERCapability PUTs the inverter's DER capability to the advertised
// DERCapabilityLink. The href is passed in rather than constructed by
// string formatting (no `/edev/{id}/der/{id}/dercap` literal). See IEEE-030.
//
// IEEE-030 tests deferred per Craig override 2026-05-12 (time crunch).
// Required-but-deferred coverage: assert PUT is issued to the exact passed
// href and not to a derived path; empty href → error, no HTTP call.
func (c *SEP2Client) PutDERCapability(ctx context.Context, dercapHref string, cap sep2.DERCapability) error {
	if dercapHref == "" {
		return fmt.Errorf("dercap href required")
	}
	return c.Put(ctx, dercapHref, &cap)
}

// PutDERSettings PUTs the inverter's DER settings to the advertised
// DERSettingsLink. See PutDERCapability for the link-derivation rationale.
//
// IEEE-030 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) PutDERSettings(ctx context.Context, dersettingsHref string, settings sep2.DERSettings) error {
	if dersettingsHref == "" {
		return fmt.Errorf("dersettings href required")
	}
	return c.Put(ctx, dersettingsHref, &settings)
}

// PutDERStatus PUTs the inverter's current DER status to the advertised
// DERStatusLink. See PutDERCapability for the link-derivation rationale.
//
// IEEE-030 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) PutDERStatus(ctx context.Context, derstatusHref string, status sep2.DERStatus) error {
	if derstatusHref == "" {
		return fmt.Errorf("derstatus href required")
	}
	return c.Put(ctx, derstatusHref, &status)
}

// GetDERProgramList GETs the DERProgramList at the given href and decodes
// it. CSIP V1.2 CORE-012 step 2 — for each FSA the EndDevice has been
// assigned, the device walks the FSA's DERProgramListLink to enumerate the
// DERPrograms bound to it. IEEE-036 lands ONLY the list GET + per-program
// subtree fetch in cmd/inverterclient/main.go; Primacy + mRID selection of
// the highest-priority DERProgram is deferred to IEEE-037 (the next ticket
// in plan-1 phase 4).
//
// First-cut paging: appends `?l=255` to fetch the first page; cursor walking
// for lists larger than 255 entries is deferred to a follow-up. Mirrors the
// paging pattern in LookupOwnEndDevice (IEEE-029) and GetFSAList (IEEE-035).
//
// IEEE-036 tests deferred per Craig override 2026-05-12. Required coverage:
//  1. Happy path: FSA returns DERProgramList with N entries; method returns
//     the parsed list intact.
//  2. Empty href: returns error matching "DERProgramList href required";
//     no HTTP call.
//  3. Server 404: error wrapped via c.Get, no panic.
//  4. Malformed XML: error wrapped via c.Get, no panic.
//  5. DERProgram with absent DefaultDERControlLink / DERControlListLink /
//     DERCurveListLink: walker skips those GETs, cache entry still recorded.
//  6. Pagination cap: list with > 255 entries — first 255 returned, rest
//     deferred to cursor follow-up.
//  7. Multi-FSA topology: each of 3 FSAs returns 2 DERPrograms; walker caches
//     6 unique programs keyed by mRID.
func (c *SEP2Client) GetDERProgramList(ctx context.Context, derProgramListHref string) (sep2.DERProgramList, error) {
	if derProgramListHref == "" {
		return sep2.DERProgramList{}, fmt.Errorf("DERProgramList href required")
	}
	sep := "?"
	if strings.Contains(derProgramListHref, "?") {
		sep = "&"
	}
	path := derProgramListHref + sep + "l=255"
	var list sep2.DERProgramList
	if err := c.Get(ctx, path, &list); err != nil {
		return sep2.DERProgramList{}, fmt.Errorf("GET DERProgramList: %w", err)
	}
	return list, nil
}

// GetDefaultDERControl GETs the DefaultDERControl resource at the advertised
// href. CSIP V1.2 CORE-012 step 2 — each DERProgram surfaces a DefaultDERControl
// that the device applies as fallback when no active DERControl is in effect.
// IEEE-036 fetches and caches it; consumption in ApplyControls is Phase 5.
//
// Empty href returns a sentinel error so callers can distinguish "link absent"
// from a transport failure without inspecting wrapped errors.
//
// IEEE-036 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) GetDefaultDERControl(ctx context.Context, defaultDERControlHref string) (sep2.DefaultDERControl, error) {
	if defaultDERControlHref == "" {
		return sep2.DefaultDERControl{}, fmt.Errorf("DefaultDERControl href required")
	}
	var dderc sep2.DefaultDERControl
	if err := c.Get(ctx, defaultDERControlHref, &dderc); err != nil {
		return sep2.DefaultDERControl{}, fmt.Errorf("GET DefaultDERControl: %w", err)
	}
	return dderc, nil
}

// GetDERControlList GETs the DERControlList at the advertised href and decodes
// it. Mirrors GetDERProgramList's paging pattern (?l=255 first page; cursor
// walking deferred). IEEE-036 caches the result per-program; scheduling and
// application are Phase 5 (IEEE-038..).
//
// IEEE-036 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) GetDERControlList(ctx context.Context, derControlListHref string) (sep2.DERControlList, error) {
	if derControlListHref == "" {
		return sep2.DERControlList{}, fmt.Errorf("DERControlList href required")
	}
	sep := "?"
	if strings.Contains(derControlListHref, "?") {
		sep = "&"
	}
	path := derControlListHref + sep + "l=255"
	var list sep2.DERControlList
	if err := c.Get(ctx, path, &list); err != nil {
		return sep2.DERControlList{}, fmt.Errorf("GET DERControlList: %w", err)
	}
	return list, nil
}

// GetDERCurveList GETs the DERCurveList at the advertised href and decodes
// it. Mirrors GetDERProgramList's paging pattern. IEEE-036 caches the result
// per-program; curve lookup and interpolation are Phase 5.
//
// IEEE-036 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) GetDERCurveList(ctx context.Context, derCurveListHref string) (sep2.DERCurveList, error) {
	if derCurveListHref == "" {
		return sep2.DERCurveList{}, fmt.Errorf("DERCurveList href required")
	}
	sep := "?"
	if strings.Contains(derCurveListHref, "?") {
		sep = "&"
	}
	path := derCurveListHref + sep + "l=255"
	var list sep2.DERCurveList
	if err := c.Get(ctx, path, &list); err != nil {
		return sep2.DERCurveList{}, fmt.Errorf("GET DERCurveList: %w", err)
	}
	return list, nil
}

// CreateMirrorUsagePoint POSTs a MirrorUsagePoint registration to the
// MirrorUsagePointList href advertised by DeviceCapability. Returns the
// server-assigned Location of the new MirrorUsagePoint resource. See
// IEEE-030.
//
// IEEE-030 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) CreateMirrorUsagePoint(ctx context.Context, mupListHref string, mup sep2.MirrorUsagePoint) (string, error) {
	if mupListHref == "" {
		return "", fmt.Errorf("mup list href required")
	}
	mup.DeviceLFDI = c.lfdi
	return c.Post(ctx, mupListHref, &mup)
}

// PostMeterReading POSTs a metering data point to the MirrorMeterReadingList
// href advertised on the MirrorUsagePoint resource. See IEEE-030.
//
// IEEE-030 tests deferred per Craig override 2026-05-12.
func (c *SEP2Client) PostMeterReading(ctx context.Context, mmrListHref string, mmr sep2.MirrorMeterReading) error {
	if mmrListHref == "" {
		return fmt.Errorf("mmr list href required")
	}
	_, err := c.Post(ctx, mmrListHref, &mmr)
	return err
}

// resolveServerURL resolves a possibly-relative server-supplied href against
// c.baseURL. Absolute hrefs (carrying a scheme) are returned verbatim;
// relative hrefs are joined to baseURL so callers do not accidentally
// double-prefix. Matches CSIP §6.6 / IEEE 2030.5 §10.3 — devices MUST treat
// every advertised URI as opaque and resolve via RFC 3986, not by string
// concatenation. Currently only PostResponse needs the full resolution
// surface (Response.replyTo is the first href that the spec allows to be
// absolute); the GET/PUT helpers still string-concat with c.baseURL because
// every other advertised link in IEEE 2030.5 is a path under the server.
// See IEEE-043.
func (c *SEP2Client) resolveServerURL(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("parse href %q: %w", href, err)
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL %q: %w", c.baseURL, err)
	}
	return base.ResolveReference(u).String(), nil
}

// isTransientResponseStatus reports whether an HTTP status code from a
// Response POST should be treated as a transient failure (i.e. retriable
// per IEEE-045's future policy and pattern-matchable via
// ErrResponseTransient). Anything in the 5xx band qualifies.
func isTransientResponseStatus(code int) bool {
	return code >= 500 && code <= 599
}

// PostResponse POSTs a DERControlResponse to the replyTo URI advertised on
// the server-issued DERControl, per CSIP V1.2 CORE-022 and IEEE 2030.5
// §10.10. The href may be relative (e.g. `/rsps/{rspSetID}/rsp`) or absolute
// (e.g. `https://server/rsps/...`); both forms resolve correctly against
// c.baseURL via resolveServerURL.
//
// Success criteria per CORE-022: the server returns 201 Created (typically
// with a Location header pointing at the new Response resource) or 204 No
// Content. Both are treated as success and the method returns nil. On 201
// the Location header is logged at debug level — useful for tracing
// duplicate-Response detection across retries, but not load-bearing for
// behavior.
//
// Failure handling is split by status class so the upcoming IEEE-044 state
// machine hook and IEEE-045 retry/dead-letter policy can pattern-match
// without re-parsing:
//
//   - 4xx: wrapped error containing the status code and URL. The response
//     body is NOT logged verbatim — it may echo XML that triggered the
//     rejection and leaking it raises XSS-via-log and PII concerns. The
//     wrapped error message is similarly status-only.
//   - 5xx and transport-level errors: one in-call retry. If the retry also
//     fails, the returned error wraps ErrResponseTransient so callers can
//     `errors.Is(err, ErrResponseTransient)` to drive the upstream
//     exponential-backoff policy (IEEE-045).
//   - Other (1xx / 3xx): wrapped non-transient error. These should not
//     occur in practice — IEEE 2030.5 servers do not redirect Response
//     POSTs — but a hostile or misconfigured peer should not crash the
//     client.
//   - Context cancellation: returned unchanged via %w; callers can
//     `errors.Is(err, context.Canceled)` / `errors.Is(err, context.DeadlineExceeded)`.
//
// The one-shot retry is intentionally surgical: full retry/backoff with
// dead-lettering is IEEE-045's scope. PostResponse ships only the safety
// net so a single dropped connection or 503 does not bubble straight to
// the state-machine hook in IEEE-044. See IEEE-043 (this ticket) and the
// `phase-6-response-function-set.md` plan doc.
func (c *SEP2Client) PostResponse(ctx context.Context, replyToHref string, resp sep2.DERControlResponse) error {
	if replyToHref == "" {
		return fmt.Errorf("replyTo href required")
	}

	target, err := c.resolveServerURL(replyToHref)
	if err != nil {
		return fmt.Errorf("resolve replyTo: %w", err)
	}

	body, err := xml.Marshal(&resp)
	if err != nil {
		return fmt.Errorf("marshal DERControlResponse: %w", err)
	}

	// One-shot retry: attempt + (optional) single retry on transient
	// failures. Keep the loop body straight-line — do not let it grow into
	// IEEE-045's territory.
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.postResponseOnce(ctx, target, body)
		if err == nil {
			return nil
		}
		// Honor cancellation: never retry, never wrap with sentinel —
		// ctx errors are the caller's signal.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("POST Response %s: %w", target, err)
		}
		lastErr = err
		if !errors.Is(err, ErrResponseTransient) {
			return err
		}
		// Transient — fall through to retry unless we are out of
		// attempts.
	}
	return lastErr
}

// postResponseOnce performs a single Response POST attempt and classifies
// the outcome. Transient failures (5xx, transport errors) are wrapped with
// ErrResponseTransient so PostResponse's retry loop can pattern-match.
// Non-transient failures are wrapped without the sentinel — callers must
// not retry them.
func (c *SEP2Client) postResponseOnce(ctx context.Context, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST Response request for %s: %w", target, err)
	}
	req.Header.Set("Content-Type", contentTypeSEPXML)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", keepAliveTimeout)

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		// Surface context cancellation unwrapped-by-sentinel so the
		// outer retry can short-circuit.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// All other transport-level failures are transient by
		// definition (connection refused, reset, TLS handshake mid-
		// connection, etc). Wrap with the sentinel.
		return fmt.Errorf("POST Response %s: %w: %v", target, ErrResponseTransient, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	// Drain the body for connection reuse, but DO NOT log it (see method
	// doc — body redaction is the policy).
	_, _ = io.Copy(io.Discard, httpResp.Body)

	switch {
	case httpResp.StatusCode == http.StatusCreated:
		if loc := httpResp.Header.Get("Location"); loc != "" {
			log.Printf("PostResponse: 201 Created at %s (location=%s)", target, loc)
		}
		return nil
	case httpResp.StatusCode == http.StatusNoContent:
		return nil
	case isTransientResponseStatus(httpResp.StatusCode):
		return fmt.Errorf("POST Response %s: status %d: %w", target, httpResp.StatusCode, ErrResponseTransient)
	case httpResp.StatusCode >= 400 && httpResp.StatusCode < 500:
		// 4xx — non-retriable. Body deliberately NOT logged.
		log.Printf("PostResponse: %d from %s (body redacted)", httpResp.StatusCode, target)
		return fmt.Errorf("POST Response %s: client error status %d", target, httpResp.StatusCode)
	default:
		return fmt.Errorf("POST Response %s: unexpected status %d", target, httpResp.StatusCode)
	}
}

// pollDuration maps a DeviceCapability pollRate (seconds) to a wait
// duration. Zero/unset pollRate falls back to 30s — a conservative default
// matching the example values in IEEE 2030.5 / CSIP. Exposed as a var so
// tests can compress polling cadence without faking time
// (see idle_export_test.go). See IEEE-028.
var pollDuration = func(pollRateSec uint32) time.Duration {
	if pollRateSec == 0 {
		return 30 * time.Second
	}
	return time.Duration(pollRateSec) * time.Second
}

// dcapHasAnyLink reports whether a DeviceCapability advertises at least one
// function-set link the inverter cares about for Phase 2+ progression.
// Per CSIP §6.6 / IEEE 2030.5 §10.3 a device MUST NOT proceed past
// discovery (registration, DER setup, metering) when the entry point
// advertises nothing — the server has not yet provisioned the device.
// See IEEE-028.
func dcapHasAnyLink(d sep2.DeviceCapability) bool {
	return d.EndDeviceListLink != nil ||
		d.TimeLink != nil ||
		d.SelfDeviceLink != nil ||
		d.MirrorUsagePointListLink != nil ||
		d.ResponseSetListLink != nil
}

// WaitForAdvertisedLinks blocks until DeviceCapability advertises at least
// one function-set link, re-polling /dcap every pollRate seconds (default
// 30s when unset). Honors ctx — cancellation returns ctx.Err() and exits
// the loop cleanly without re-polling. No timeout bound; callers control
// lifetime via ctx.
//
// IEEE-028: when the server returns a bare <DeviceCapability pollRate="N"/>
// with no children, the inverter must idle-re-poll rather than crash
// forward into Phase 2 (which would 404 on the unprovisioned /edev path).
func (c *SEP2Client) WaitForAdvertisedLinks(ctx context.Context, initial sep2.DeviceCapability) (sep2.DeviceCapability, error) {
	dcap := initial
	for !dcapHasAnyLink(dcap) {
		wait := pollDuration(dcap.PollRate)
		log.Printf("DeviceCapability advertises no function sets; re-polling every %s", wait)
		select {
		case <-ctx.Done():
			return dcap, ctx.Err()
		case <-time.After(wait):
		}
		next, err := c.Discover(ctx)
		if err != nil {
			return dcap, fmt.Errorf("re-discover after idle wait: %w", err)
		}
		dcap = next
	}
	return dcap, nil
}

// Server time sync (IEEE-031) =================================================
//
// CSIP / IEEE 2030.5 §10 require devices to source time from the server's Time
// resource (linked from DeviceCapability.TimeLink) and to use that time —
// not local wall-clock — for every timestamp the server consumes
// (DERSettings.UpdatedTime, MirrorMeterReading identifiers, etc.). Before
// IEEE-031 the inverter logged the TimeLink href and proceeded to use
// time.Now() everywhere.
//
// Design notes:
//
//   - Offset is stored as a signed int64 nanosecond delta (server_now -
//     local_now) on the client. Zero is a safe default — Now() degrades to
//     time.Now() before any sync has run or when no TimeLink is advertised.
//   - Reads and writes go through atomic.Int64 so the sync goroutine can
//     refresh the offset while the simulation/reporter loop reads it
//     without locking.
//   - RunTimeSync runs the periodic refresh loop. It does NOT spawn its own
//     goroutine — the caller decides whether to spawn (typically
//     `go client.RunTimeSync(ctx, href, pollRate)` from main). The loop
//     selects on ctx.Done() so it exits cleanly on shutdown (no leaked
//     goroutine).
//
// Tests deferred per Craig override 2026-05-12 (time crunch). Required
// follow-up coverage:
//   1. GetServerTime: httptest server returns Time XML; parsed correctly.
//   2. SyncServerTime updates the offset to round-trip-consistent value.
//   3. RunTimeSync: ctx cancel exits the goroutine; `go test -race` clean.
//   4. Now(): with offset=42s, client.Now() ~ time.Now()+42s.
//   5. Reporter outbound MRID derives from client.Now() (already verifiable
//      with a fixture-substituted offset).

// Now returns the current wall-clock time adjusted by the server-time
// offset discovered via SyncServerTime / RunTimeSync. Before any sync has
// run (or when no TimeLink is advertised) the offset is zero and Now()
// returns local time. Use Now() for any timestamp the server consumes.
// See IEEE-031.
func (c *SEP2Client) Now() time.Time {
	return time.Now().Add(time.Duration(c.serverTimeOffsetNanos.Load()))
}

// GetServerTime GETs and parses the IEEE 2030.5 Time resource at timeHref.
// The href is advertised on DeviceCapability.TimeLink and MUST NOT be
// hardcoded by the caller — per IEEE 2030.5 §10.3 / CSIP §6.6 the server
// is free to host Time at any path.
func (c *SEP2Client) GetServerTime(ctx context.Context, timeHref string) (sep2.Time, error) {
	if timeHref == "" {
		return sep2.Time{}, fmt.Errorf("time href required")
	}
	var t sep2.Time
	if err := c.Get(ctx, timeHref, &t); err != nil {
		return sep2.Time{}, fmt.Errorf("get server time: %w", err)
	}
	return t, nil
}

// SyncServerTime fetches the server's Time resource once and atomically
// updates the client's offset. Returns the parsed Time for callers that
// want to log it (Phase 1b in main). Errors propagate unchanged.
func (c *SEP2Client) SyncServerTime(ctx context.Context, timeHref string) (sep2.Time, error) {
	t, err := c.GetServerTime(ctx, timeHref)
	if err != nil {
		return sep2.Time{}, err
	}
	// CurrentTime is epoch seconds. Compute the signed delta between the
	// server's reported instant and our local clock at the moment we
	// finished parsing. Network round-trip and parse cost are absorbed
	// into the offset — at typical sync cadences (minutes to hours) this
	// is well within IEEE 2030.5's tolerance for device clocks.
	offset := time.Unix(t.CurrentTime, 0).Sub(time.Now())
	c.serverTimeOffsetNanos.Store(int64(offset))
	return t, nil
}

// minTimeSyncPollRate is the floor for the time-sync poll interval. The
// ticket pins this at 60s as production hygiene — anything shorter
// hammers the Time endpoint without buying meaningful clock accuracy.
//
// Declared as a var (not const) solely so the IEEE-070 sweep tests can
// drive the loop body via SetMinTimeSyncPollRateForTesting; the
// production binary never writes to this. Mirrors the pollDuration
// testability seam pattern established by IEEE-028.
var minTimeSyncPollRate = 60 * time.Second

// DefaultTimeSyncPollRate is the fallback poll cadence when the caller
// has no Time-resource pollRate to thread through. sep2.Time inherits
// only Href from Resource; pollRate lives on ListResource/DeviceCapability,
// not on single-instance resources like Time. 30 minutes matches the
// "an hour is normal" sentiment in IEEE-031 and gives the sync goroutine
// a sane default when nothing else is advertised.
const DefaultTimeSyncPollRate = 30 * time.Minute

// RunTimeSync runs the periodic time-sync loop. It does NOT spawn its own
// goroutine — the caller is expected to invoke it as
// `go client.RunTimeSync(ctx, href, pollRate)`. The loop exits cleanly
// on ctx cancellation (selects on ctx.Done() between iterations).
//
// pollRate is clamped to minTimeSyncPollRate; zero/negative values fall
// back to DefaultTimeSyncPollRate. Sync failures are logged and skipped
// — a transient network blip should not stop the loop.
func (c *SEP2Client) RunTimeSync(ctx context.Context, timeHref string, pollRate time.Duration) {
	if timeHref == "" {
		log.Println("time sync: no TimeLink href; sync loop disabled")
		return
	}
	if pollRate <= 0 {
		pollRate = DefaultTimeSyncPollRate
	}
	if pollRate < minTimeSyncPollRate {
		pollRate = minTimeSyncPollRate
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollRate):
		}
		if _, err := c.SyncServerTime(ctx, timeHref); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("time sync: refresh failed: %v", err)
			}
		}
	}
}
