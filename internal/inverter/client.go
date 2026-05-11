package inverter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-go/internal/tls"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

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
func NewSEP2Client(cfg SimConfig) (*SEP2Client, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
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

	tlsCfg := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		RootCAs:          caPool,
		MinVersion:       tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{tls.CurveP256},
	}

	// Derive SFDI/LFDI from the leaf cert that LoadX509KeyPair already
	// parsed. Reading the cert file a second time and re-decoding the PEM
	// (the previous behavior) was redundant and discarded errors from both
	// os.ReadFile and pem.Decode, leaving a nil-pointer deref on the next
	// line if either failed (IEEE-008).
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("client cert %q has no leaf certificate", cfg.CertFile)
	}
	parsedCert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse client cert %q for identity: %w", cfg.CertFile, err)
	}

	return &SEP2Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConns:        1,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     30 * time.Second,
			},
			Timeout: 30 * time.Second,
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

// Register creates an EndDevice on the server.
func (c *SEP2Client) Register(ctx context.Context) (sep2.EndDevice, error) {
	edev := sep2.EndDevice{SFDI: c.sfdi, LFDI: c.lfdi}
	enabled := true
	edev.Enabled = &enabled

	loc, err := c.Post(ctx, "/edev", &edev)
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

// PutDERCapability reports the inverter's DER capability.
func (c *SEP2Client) PutDERCapability(ctx context.Context, edevID, derID string, cap sep2.DERCapability) error {
	return c.Put(ctx, fmt.Sprintf("/edev/%s/der/%s/dercap", edevID, derID), &cap)
}

// PutDERSettings reports the inverter's DER settings.
func (c *SEP2Client) PutDERSettings(ctx context.Context, edevID, derID string, settings sep2.DERSettings) error {
	return c.Put(ctx, fmt.Sprintf("/edev/%s/der/%s/derg", edevID, derID), &settings)
}

// PutDERStatus reports the inverter's current DER status.
func (c *SEP2Client) PutDERStatus(ctx context.Context, edevID, derID string, status sep2.DERStatus) error {
	return c.Put(ctx, fmt.Sprintf("/edev/%s/der/%s/ders", edevID, derID), &status)
}

// GetDefaultDERControl fetches the default DER control for a program.
func (c *SEP2Client) GetDefaultDERControl(ctx context.Context, path string) (sep2.DefaultDERControl, error) {
	var dderc sep2.DefaultDERControl
	err := c.Get(ctx, path, &dderc)
	return dderc, err
}

// CreateMirrorUsagePoint registers for metering data reporting.
func (c *SEP2Client) CreateMirrorUsagePoint(ctx context.Context, mup sep2.MirrorUsagePoint) (string, error) {
	mup.DeviceLFDI = c.lfdi
	return c.Post(ctx, "/mup", &mup)
}

// PostMeterReading sends a metering data point.
func (c *SEP2Client) PostMeterReading(ctx context.Context, mupID string, mmr sep2.MirrorMeterReading) error {
	_, err := c.Post(ctx, fmt.Sprintf("/mup/%s/mr", mupID), &mmr)
	return err
}
