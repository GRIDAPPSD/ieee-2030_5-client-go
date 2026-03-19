package inverter_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"net/http"
	"testing"
	"time"

	"github.com/craig8/ieee-2030_5-go/internal/certs"
	"github.com/craig8/ieee-2030_5-go/internal/config"
	"github.com/craig8/ieee-2030_5-go/internal/inverter"
	"github.com/craig8/ieee-2030_5-go/internal/server"
	sepTLS "github.com/craig8/ieee-2030_5-go/internal/tls"
	"github.com/craig8/ieee-2030_5-go/pkg/sep2"
	"github.com/craig8/ieee-2030_5-go/pkg/store/memory"
)

// TestEndToEndInverterLifecycle runs the full IEEE 2030.5 protocol lifecycle:
// discovery → registration → DER setup → metering → status reporting.
// Uses a real TLS server with generated certs.
func TestEndToEndInverterLifecycle(t *testing.T) {
	// Generate all certificates
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

	// Write certs to temp files for the client
	tmpDir := t.TempDir()
	writeFile(t, tmpDir+"/ca.crt", caCertPEM)
	writeFile(t, tmpDir+"/device.crt", deviceCertPEM)
	writeFile(t, tmpDir+"/device.key", deviceKeyPEM)

	// Start TLS server
	serverTLSCfg, err := sepTLS.NewServerTLSConfigFromPEM(serverCertPEM, serverKeyPEM, caCertPEM)
	if err != nil {
		t.Fatal(err)
	}

	stores := &server.Stores{
		EndDevices:          memory.NewEndDeviceStore(),
		MirrorUsagePoints:   memory.NewStore[sep2.MirrorUsagePoint](),
		MirrorMeterReadings: memory.NewScopedStore[sep2.MirrorMeterReading](),
		DERs:               memory.NewScopedStore[sep2.DER](),
		DERCapabilities:    memory.NewScopedStore[sep2.DERCapability](),
		DERSettings:        memory.NewScopedStore[sep2.DERSettings](),
		DERStatuses:        memory.NewScopedStore[sep2.DERStatus](),
		DERAvailabilities:  memory.NewScopedStore[sep2.DERAvailability](),
		DERPrograms:        memory.NewScopedStore[sep2.DERProgram](),
		DERControls:        memory.NewScopedStore[sep2.DERControl](),
		DefaultDERControls: memory.NewScopedStore[sep2.DefaultDERControl](),
		DERCurves:          memory.NewStore[sep2.DERCurve](),
	}

	cfg := &config.Config{
		TZOffset:    -28800,
		TimeQuality: sep2.TimeQualityNTP,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	tlsListener := tls.NewListener(listener, serverTLSCfg)
	router := server.NewRouter(cfg, stores, nil, "", "")
	srv := &http.Server{Handler: router}
	go srv.Serve(tlsListener)
	defer srv.Close()

	serverURL := "https://" + listener.Addr().String()

	// Create inverter client
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
			t.Errorf("dcap.Href = %q", dcap.Href)
		}
		if dcap.TimeLink == nil || dcap.TimeLink.Href != "/tm" {
			t.Error("missing TimeLink")
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
		edev, err := client.Register(ctx)
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

		err := client.PutDERCapability(ctx, edevID, derID, sep2.DERCapability{
			RTGMaxW:        &maxW,
			RTGMaxVar:      &maxVAr,
			ModesSupported: &modes,
			Type:           &dtype,
		})
		if err != nil {
			t.Fatalf("PutDERCapability: %v", err)
		}

		// Verify it was stored by reading back
		var cap sep2.DERCapability
		err = client.Get(ctx, "/edev/"+edevID+"/der/"+derID+"/dercap", &cap)
		if err != nil {
			t.Fatalf("GET dercap: %v", err)
		}
		if cap.RTGMaxW == nil || cap.RTGMaxW.Value != 10000 {
			t.Errorf("RTGMaxW = %v, want 10000", cap.RTGMaxW)
		}
	})

	t.Run("put_der_settings", func(t *testing.T) {
		setMaxW := sep2.ActivePower{Value: 10000}
		err := client.PutDERSettings(ctx, edevID, derID, sep2.DERSettings{
			SetMaxW:     &setMaxW,
			UpdatedTime: time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("PutDERSettings: %v", err)
		}
	})

	// Phase 4: DER Status Reporting
	t.Run("put_der_status", func(t *testing.T) {
		err := client.PutDERStatus(ctx, edevID, derID, sep2.DERStatus{
			GenConnectStatus: &sep2.ConnectStatusType{
				DateTime: time.Now().Unix(),
				Value:    1,
			},
			ReadingTime: time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("PutDERStatus: %v", err)
		}

		// Read back
		var status sep2.DERStatus
		err = client.Get(ctx, "/edev/"+edevID+"/der/"+derID+"/ders", &status)
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
		loc, err := client.CreateMirrorUsagePoint(ctx, sep2.MirrorUsagePoint{
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
		err := client.PostMeterReading(ctx, mupID, sep2.MirrorMeterReading{
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
		err := client.Get(ctx, "/tm", &tm)
		if err != nil {
			t.Fatalf("GET /tm: %v", err)
		}
		if tm.CurrentTime == 0 {
			t.Error("CurrentTime should not be 0")
		}
	})

	// Phase 7: Verify EndDevice list shows our device
	t.Run("list_end_devices", func(t *testing.T) {
		var list sep2.EndDeviceList
		err := client.Get(ctx, "/edev", &list)
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

	// Phase 8: Duplicate registration returns existing
	t.Run("duplicate_register", func(t *testing.T) {
		edev2, err := client.Register(ctx)
		if err != nil {
			t.Fatalf("duplicate Register: %v", err)
		}
		// Should return the same device, not create a new one
		if edev2.SFDI != client.SFDI() {
			t.Error("duplicate registration should return same SFDI")
		}
	})

	// Phase 9: DefaultDERControl (empty default when no data)
	t.Run("get_default_der_control", func(t *testing.T) {
		var dderc sep2.DefaultDERControl
		// This path may not have data seeded, but should return an empty default (200)
		err := client.Get(ctx, "/edev/"+edevID+"/fsa/1/derp/1/dderc", &dderc)
		if err != nil {
			t.Fatalf("GET dderc: %v", err)
		}
		// Should return 200 with empty/default resource
		t.Logf("DefaultDERControl: href=%s", dderc.Href)
	})

	t.Logf("=== End-to-end lifecycle complete: %d phases passed ===", 9)
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
