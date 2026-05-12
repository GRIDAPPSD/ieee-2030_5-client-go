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
	"os"
	"strings"
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

const (
	contentTypeSEPXML = "application/sep+xml"
	keepAliveTimeout  = "timeout=30, max=1000"
)

// SEP2Client is an IEEE 2030.5 HTTP client with mTLS and persistent connections.
type SEP2Client struct {
	httpClient *http.Client
	baseURL    string
	sfdi       string
	lfdi       string
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

// GetDefaultDERControl fetches the default DER control for a program.
func (c *SEP2Client) GetDefaultDERControl(ctx context.Context, path string) (sep2.DefaultDERControl, error) {
	var dderc sep2.DefaultDERControl
	err := c.Get(ctx, path, &dderc)
	return dderc, err
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
