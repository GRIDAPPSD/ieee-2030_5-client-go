package inverter_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
	certs "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2cert"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2srv/assembly"
	sepTLS "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/store/memory"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// deviceIdentityKey is a package-private context key used by the test-only
// identity middleware to store the device LFDI/SFDI derived from the TLS peer
// certificate.  A private type prevents accidental collision with any other
// context key in the call chain.
type deviceIdentityKey struct{}

// deviceIdentity holds the pair extracted from the peer cert.
type deviceIdentity struct {
	lfdi string
	sfdi string
}

// buildTestAuthPolicy returns an assembly.AuthPolicy suitable for the
// integration test:
//
//   - Wrap installs middleware that reads the TLS peer certificate from the
//     request, derives LFDI and SFDI via sepTLS helpers, and stashes the pair
//     in the request context.  Requests without a TLS peer cert are rejected
//     with 403 Forbidden.
//   - Identity reads the pair back from context (ok=false when absent).
//   - SFDIPrefix returns the first 8 characters of the SFDI, matching the
//     auth.ExtractSFDIPrefix rule (IEEE-014).
func buildTestAuthPolicy() assembly.AuthPolicy {
	return assembly.AuthPolicy{
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
					http.Error(w, "client certificate required", http.StatusForbidden)
					return
				}
				cert := r.TLS.PeerCertificates[0]
				id := deviceIdentity{
					lfdi: sepTLS.LFDI(cert),
					sfdi: sepTLS.SFDI(cert),
				}
				ctx := context.WithValue(r.Context(), deviceIdentityKey{}, id)
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		},
		Identity: func(ctx context.Context) (lfdi, sfdi string, ok bool) {
			id, ok := ctx.Value(deviceIdentityKey{}).(deviceIdentity)
			if !ok {
				return "", "", false
			}
			return id.lfdi, id.sfdi, true
		},
		SFDIPrefix: func(sfdi string) (string, error) {
			if len(sfdi) < 8 {
				return "", fmt.Errorf("SFDI %q too short: need at least 8 chars", sfdi)
			}
			return sfdi[:8], nil
		},
	}
}

// TestEndToEndInverterLifecycle runs the full IEEE 2030.5 protocol lifecycle:
// discovery, registration, DER setup, metering, status reporting.
// Uses a real TLS server with generated certs and drives the real inverter
// client over the wire.
func TestEndToEndInverterLifecycle(t *testing.T) {
	// Generate all certificates.
	caCertPEM, caKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "E2E Test CA",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	caCert, caKey := parsePEMPair(t, caCertPEM, caKeyPEM)

	serverCertPEM, serverKeyPEM, err := certs.GenerateServerCert(caCert, caKey, certs.ServerCertOptions{
		Hosts:      []string{"127.0.0.1", "localhost"},
		CommonName: "E2E Server",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	deviceCertPEM, deviceKeyPEM, err := certs.GenerateDeviceCert(caCert, caKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "E2E-INV-001",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Write certs to temp files for the client.
	tmpDir := t.TempDir()
	writeFile(t, tmpDir+"/ca.crt", caCertPEM)
	writeFile(t, tmpDir+"/device.crt", deviceCertPEM)
	writeFile(t, tmpDir+"/device.key", deviceKeyPEM)

	// Build the in-process server.
	serverTLSCfg, err := sepTLS.NewServerTLSConfigFromPEM(serverCertPEM, serverKeyPEM, caCertPEM)
	if err != nil {
		t.Fatal(err)
	}

	stores := &assembly.Stores{
		EndDevices:               memory.NewEndDeviceStore(),
		Registrations:            memory.NewRegistrationStore(),
		MirrorUsagePoints:        memory.NewStore[sep2.MirrorUsagePoint](),
		MirrorMeterReadings:      memory.NewScopedStore[sep2.MirrorMeterReading](),
		DERs:                     memory.NewScopedStore[sep2.DER](),
		DERCapabilities:          memory.NewScopedStore[sep2.DERCapability](),
		DERSettings:              memory.NewScopedStore[sep2.DERSettings](),
		DERStatuses:              memory.NewScopedStore[sep2.DERStatus](),
		DERAvailabilities:        memory.NewScopedStore[sep2.DERAvailability](),
		DERPrograms:              memory.NewDERProgramStore(),
		DERControls:              memory.NewScopedStore[sep2.DERControl](),
		DefaultDERControls:       memory.NewScopedStore[sep2.DefaultDERControl](),
		DERCurves:                memory.NewStore[sep2.DERCurve](),
		FSAs:                     memory.NewScopedStore[sep2.FunctionSetAssignments](),
		Subscriptions:            memory.NewSubscriptionStore(),
		UsagePoints:              memory.NewStore[sep2.UsagePoint](),
		MeterReadings:            memory.NewScopedStore[sep2.MeterReading](),
		Readings:                 memory.NewScopedStore[sep2.Reading](),
		ReadingTypes:             memory.NewStore[sep2.ReadingType](),
		Configurations:           memory.NewScopedStore[sep2.Configuration](),
		DeviceStatuses:           memory.NewScopedStore[sep2.DeviceStatus](),
		LogEvents:                memory.NewScopedStore[sep2.LogEvent](),
		PowerStatuses:            memory.NewScopedStore[sep2.PowerStatus](),
		MessagingPrograms:        memory.NewStore[sep2.MessagingProgram](),
		TextMessages:             memory.NewScopedStore[sep2.TextMessage](),
		FlowReservationRequests:  memory.NewScopedStore[sep2.FlowReservationRequest](),
		FlowReservationResponses: memory.NewScopedStore[sep2.FlowReservationResponse](),
		ResponseSets:             memory.NewStore[sep2.ResponseSet](),
		Responses:                memory.NewScopedStore[sep2.Response](),
	}

	routerCfg := assembly.RouterConfig{
		TZOffset:    -28800,
		TimeQuality: sep2.TimeQualityNTP,
	}
	authPolicy := buildTestAuthPolicy()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	tlsListener := tls.NewListener(listener, serverTLSCfg)
	router, _ := assembly.BuildProtocolRouter(routerCfg, stores, authPolicy, "", "", nil)
	srv := &http.Server{Handler: router}
	go func() { _ = srv.Serve(tlsListener) }()
	defer func() { _ = srv.Close() }()

	serverURL := "https://" + listener.Addr().String()

	// Create the inverter client.
	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  tmpDir + "/device.crt",
		KeyFile:   tmpDir + "/device.key",
		CAFile:    tmpDir + "/ca.crt",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// Phase 1: Discovery
	t.Run("discover", func(t *testing.T) {
		dcap, err := client.Discover(ctx)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if dcap.Href != "/dcap" {
			t.Errorf("dcap.Href = %q, want /dcap", dcap.Href)
		}
		if dcap.TimeLink == nil || dcap.TimeLink.Href != "/tm" {
			t.Error("missing TimeLink href /tm")
		}
		if dcap.EndDeviceListLink == nil {
			t.Error("missing EndDeviceListLink")
		}
		if dcap.MirrorUsagePointListLink == nil {
			t.Error("missing MirrorUsagePointListLink")
		}
	})

	// Phase 2: Registration
	var edevID string
	t.Run("register", func(t *testing.T) {
		// IEEE-030: Register takes the EndDeviceList href, not a baked-in
		// constant.  The test server mounts the list at /edev (which is what
		// dcap.EndDeviceListLink.Href advertises), so we pass that literal.
		edev, _, err := client.Register(ctx, "/edev")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if edev.SFDI == "" {
			t.Error("registered SFDI should not be empty")
		}
		if edev.SFDI != client.SFDI() {
			t.Errorf("SFDI mismatch: registered=%q client=%q", edev.SFDI, client.SFDI())
		}
		if edev.Href == "" {
			t.Fatal("registered Href should not be empty")
		}
		edevID = extractLastSegment(edev.Href)
		t.Logf("Registered device: %s (SFDI: %s)", edev.Href, edev.SFDI)
	})

	if edevID == "" {
		t.Fatal("registration failed, cannot continue")
	}

	// Phase 3: DER Setup
	derID := "1"
	t.Run("put_der_capability", func(t *testing.T) {
		maxW := sep2.ActivePower{Value: 10000}
		maxVAr := sep2.ReactivePower{Value: 4400}
		modes := uint32(0xFF)
		dtype := uint8(4)

		// IEEE-030: PutDERCapability takes the DERCapabilityLink href directly.
		err := client.PutDERCapability(ctx, "/edev/"+edevID+"/der/"+derID+"/dercap", sep2.DERCapability{
			RTGMaxW:        &maxW,
			RTGMaxVar:      &maxVAr,
			ModesSupported: &modes,
			Type:           &dtype,
		})
		if err != nil {
			t.Fatalf("PutDERCapability: %v", err)
		}

		// Verify stored value via a GET (data-invariants Rule 1: assert field values).
		var cap sep2.DERCapability
		_, err = client.Get(ctx, "/edev/"+edevID+"/der/"+derID+"/dercap", &cap)
		if err != nil {
			t.Fatalf("GET dercap: %v", err)
		}
		if cap.RTGMaxW == nil || cap.RTGMaxW.Value != 10000 {
			t.Errorf("RTGMaxW = %v, want 10000", cap.RTGMaxW)
		}
		if cap.RTGMaxVar == nil || cap.RTGMaxVar.Value != 4400 {
			t.Errorf("RTGMaxVar = %v, want 4400", cap.RTGMaxVar)
		}
	})

	t.Run("put_der_settings", func(t *testing.T) {
		setMaxW := sep2.ActivePower{Value: 10000}
		// IEEE-030: pass the DERSettingsLink href explicitly.
		err := client.PutDERSettings(ctx, "/edev/"+edevID+"/der/"+derID+"/derg", sep2.DERSettings{
			SetMaxW:     &setMaxW,
			UpdatedTime: time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("PutDERSettings: %v", err)
		}

		// Verify stored value (data-invariants Rule 1).
		var derg sep2.DERSettings
		_, err = client.Get(ctx, "/edev/"+edevID+"/der/"+derID+"/derg", &derg)
		if err != nil {
			t.Fatalf("GET derg: %v", err)
		}
		if derg.SetMaxW == nil || derg.SetMaxW.Value != 10000 {
			t.Errorf("DERSettings.SetMaxW = %v, want 10000", derg.SetMaxW)
		}
	})

	// Phase 4: DER Status Reporting
	t.Run("put_der_status", func(t *testing.T) {
		// IEEE-030: pass the DERStatusLink href explicitly.
		err := client.PutDERStatus(ctx, "/edev/"+edevID+"/der/"+derID+"/ders", sep2.DERStatus{
			GenConnectStatus: &sep2.ConnectStatusType{
				DateTime: time.Now().Unix(),
				Value:    1,
			},
			ReadingTime: time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("PutDERStatus: %v", err)
		}

		// Verify stored value (data-invariants Rule 1).
		var status sep2.DERStatus
		_, err = client.Get(ctx, "/edev/"+edevID+"/der/"+derID+"/ders", &status)
		if err != nil {
			t.Fatalf("GET ders: %v", err)
		}
		if status.GenConnectStatus == nil || status.GenConnectStatus.Value != 1 {
			t.Errorf("GenConnectStatus = %v, want connected(1)", status.GenConnectStatus)
		}
	})

	// Phase 5: Metering
	var mupID string
	t.Run("create_mirror_usage_point", func(t *testing.T) {
		// IEEE-030: pass the MirrorUsagePointList href explicitly.
		loc, err := client.CreateMirrorUsagePoint(ctx, "/mup", sep2.MirrorUsagePoint{
			MRID:                "mup-e2e-test",
			Description:         "E2E Test Inverter",
			ServiceCategoryKind: 0,
			Status:              1,
		})
		if err != nil {
			t.Fatalf("CreateMirrorUsagePoint: %v", err)
		}
		if loc == "" {
			t.Fatal("MUP Location should not be empty")
		}
		mupID = extractLastSegment(loc)
		t.Logf("MirrorUsagePoint: %s (id: %s)", loc, mupID)
	})

	t.Run("post_meter_reading", func(t *testing.T) {
		if mupID == "" {
			t.Skip("no MUP ID")
		}

		uomW := sep2.UomWatts
		val := int64(8500)
		// IEEE-030: pass the MirrorMeterReadingList href explicitly.  The
		// server mounts the list at /mup/{mupID}/mr.
		err := client.PostMeterReading(ctx, "/mup/"+mupID+"/mr", sep2.MirrorMeterReading{
			MRID:           "mmr-e2e-001",
			Description:    "Active Power",
			LastUpdateTime: time.Now().Unix(),
			ReadingType:    &sep2.ReadingType{Uom: &uomW},
			Reading: &sep2.Reading{
				Value: &val,
				TimePeriod: &sep2.DateTimeInterval{
					Start:    time.Now().Unix(),
					Duration: 1,
				},
			},
		})
		if err != nil {
			t.Fatalf("PostMeterReading: %v", err)
		}
	})

	// Phase 6: Time resource
	t.Run("get_time", func(t *testing.T) {
		var tm sep2.Time
		_, err := client.Get(ctx, "/tm", &tm)
		if err != nil {
			t.Fatalf("GET /tm: %v", err)
		}
		if tm.CurrentTime == 0 {
			t.Error("CurrentTime should not be 0")
		}
		if tm.TzOffset != -28800 {
			t.Errorf("TzOffset = %d, want -28800", tm.TzOffset)
		}
	})

	// Phase 7: Verify EndDevice list shows our device
	t.Run("list_end_devices", func(t *testing.T) {
		var list sep2.EndDeviceList
		_, err := client.Get(ctx, "/edev", &list)
		if err != nil {
			t.Fatalf("GET /edev: %v", err)
		}
		if list.All < 1 {
			t.Error("EndDeviceList should have at least 1 device")
		}

		found := false
		for _, dev := range list.EndDevice {
			if dev.SFDI == client.SFDI() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("our device (SFDI=%s) not in EndDeviceList", client.SFDI())
		}
	})

	// Phase 8: Duplicate registration returns existing device
	t.Run("duplicate_register", func(t *testing.T) {
		// IEEE-030: pass the EndDeviceList href explicitly.
		edev2, _, err := client.Register(ctx, "/edev")
		if err != nil {
			t.Fatalf("duplicate Register: %v", err)
		}
		// Should return the same device, not create a new one.
		if edev2.SFDI != client.SFDI() {
			t.Errorf("duplicate registration SFDI = %q, want %q", edev2.SFDI, client.SFDI())
		}
	})

	// Phase 9: DefaultDERControl (empty default when no data seeded)
	t.Run("get_default_der_control", func(t *testing.T) {
		var dderc sep2.DefaultDERControl
		// This path may not have data seeded, but should return an empty default (200).
		_, err := client.Get(ctx, "/edev/"+edevID+"/fsa/1/derp/1/dderc", &dderc)
		if err != nil {
			t.Fatalf("GET dderc: %v", err)
		}
		// Should return 200 with empty/default resource.
		t.Logf("DefaultDERControl: href=%s", dderc.Href)
	})

	t.Logf("=== End-to-end lifecycle complete: 9 phases passed ===")
}

// helpers

func parsePEMPair(t *testing.T, certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	raw, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert, raw.(*ecdsa.PrivateKey)
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func extractLastSegment(href string) string {
	for i := len(href) - 1; i >= 0; i-- {
		if href[i] == '/' {
			return href[i+1:]
		}
	}
	return href
}
