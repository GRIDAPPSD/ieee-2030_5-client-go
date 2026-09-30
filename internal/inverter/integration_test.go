package inverter_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	certs "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

// e2eServer is a minimal stateful IEEE 2030.5 server double for this test.
//
// Core v0.20.0 dropped pkg/sep2srv and pkg/store to server-go (they moved
// verbatim; core keeps only the surface a client and a server both need).
// Depending on server-go from here to get them back would be a new
// module dependency this change may not add, so this hand-rolls exactly
// the routes the lifecycle below exercises, mirroring the pattern every
// other test file in this package already uses (idle_test.go,
// client_ccm_test.go, derprogram_phase_test.go all serve a bespoke
// http.ServeMux rather than a real router).
type e2eServer struct {
	mu     sync.Mutex
	edev   *sep2.EndDevice
	dercap sep2.DERCapability
	derg   sep2.DERSettings
	ders   sep2.DERStatus
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/sep+xml")
	_ = xml.NewEncoder(w).Encode(v)
}

func (s *e2eServer) handleDcap(w http.ResponseWriter, _ *http.Request) {
	writeXML(w, &sep2.DeviceCapability{
		Resource:                 sep2.Resource{Href: "/dcap"},
		TimeLink:                 &sep2.Link{Href: "/tm"},
		EndDeviceListLink:        &sep2.ListLink{Href: "/edev"},
		MirrorUsagePointListLink: &sep2.ListLink{Href: "/mup"},
	})
}

func (s *e2eServer) handleEdevCollection(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		var edev sep2.EndDevice
		if err := xml.NewDecoder(r.Body).Decode(&edev); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		// Duplicate-tolerant: only one device registers in this test, so a
		// second POST returns the existing resource rather than creating one.
		if s.edev == nil {
			edev.Href = "/edev/1"
			edev.ChangedTime = time.Now().Unix()
			s.edev = &edev
		}
		w.Header().Set("Location", s.edev.Href)
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		list := sep2.EndDeviceList{}
		if s.edev != nil {
			list.All = 1
			list.EndDevice = []sep2.EndDevice{*s.edev}
		}
		writeXML(w, &list)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *e2eServer) handleEdevItem(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.edev == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeXML(w, s.edev)
}

func (s *e2eServer) handleDERCapability(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var cap sep2.DERCapability
		if err := xml.NewDecoder(r.Body).Decode(&cap); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		s.dercap = cap
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		writeXML(w, &s.dercap)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *e2eServer) handleDERSettings(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var settings sep2.DERSettings
		if err := xml.NewDecoder(r.Body).Decode(&settings); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		s.derg = settings
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		writeXML(w, &s.derg)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *e2eServer) handleDERStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var status sep2.DERStatus
		if err := xml.NewDecoder(r.Body).Decode(&status); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		s.ders = status
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		writeXML(w, &s.ders)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *e2eServer) handleMUPCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var mup sep2.MirrorUsagePoint
	if err := xml.NewDecoder(r.Body).Decode(&mup); err != nil {
		http.Error(w, "malformed", http.StatusBadRequest)
		return
	}
	w.Header().Set("Location", "/mup/1")
	w.WriteHeader(http.StatusCreated)
}

func (s *e2eServer) handleMeterReading(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var mmr sep2.MirrorMeterReading
	if err := xml.NewDecoder(r.Body).Decode(&mmr); err != nil {
		http.Error(w, "malformed", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *e2eServer) handleTime(w http.ResponseWriter, _ *http.Request) {
	writeXML(w, &sep2.Time{
		CurrentTime: time.Now().Unix(),
		TzOffset:    -28800,
	})
}

func (s *e2eServer) handleDefaultDERControl(w http.ResponseWriter, _ *http.Request) {
	writeXML(w, &sep2.DefaultDERControl{})
}

func (s *e2eServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", s.handleDcap)
	mux.HandleFunc("/edev", s.handleEdevCollection)
	mux.HandleFunc("/edev/1", s.handleEdevItem)
	mux.HandleFunc("/edev/1/der/1/dercap", s.handleDERCapability)
	mux.HandleFunc("/edev/1/der/1/derg", s.handleDERSettings)
	mux.HandleFunc("/edev/1/der/1/ders", s.handleDERStatus)
	mux.HandleFunc("/mup", s.handleMUPCollection)
	mux.HandleFunc("/mup/1/mr", s.handleMeterReading)
	mux.HandleFunc("/tm", s.handleTime)
	mux.HandleFunc("/edev/1/fsa/1/derp/1/dderc", s.handleDefaultDERControl)
	return mux
}

// TestEndToEndInverterLifecycle runs the full IEEE 2030.5 protocol lifecycle:
// discovery, registration, DER setup, metering, status reporting.
// Uses a real CCM-8 TLS server with generated certs and drives the real
// inverter client over the wire.
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

	// Build the in-process server. Core offers CCM-8 only (no stdlib
	// *tls.Config path survives), so the listener is gotls-backed.
	serverTLSCfg, err := sepTLS.NewCCMServerConfigFromPEM(serverCertPEM, serverKeyPEM, caCertPEM)
	if err != nil {
		t.Fatal(err)
	}

	srvState := &e2eServer{}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	tlsListener := gotls.NewListener(listener, serverTLSCfg)
	srv := &http.Server{Handler: srvState.mux()}
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
		// Register takes the EndDeviceList href, not a baked-in
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
		modes := sep2.DERControlType(0xFF)
		dtype := uint8(4)

		// PutDERCapability takes the DERCapabilityLink href directly.
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
		_, err = client.Get(ctx, guard.KindEndDeviceRead, "/edev/"+edevID+"/der/"+derID+"/dercap", &cap)
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
		// Pass the DERSettingsLink href explicitly.
		err := client.PutDERSettings(ctx, "/edev/"+edevID+"/der/"+derID+"/derg", sep2.DERSettings{
			SetMaxW:     &setMaxW,
			UpdatedTime: time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("PutDERSettings: %v", err)
		}

		// Verify stored value (data-invariants Rule 1).
		var derg sep2.DERSettings
		_, err = client.Get(ctx, guard.KindEndDeviceRead, "/edev/"+edevID+"/der/"+derID+"/derg", &derg)
		if err != nil {
			t.Fatalf("GET derg: %v", err)
		}
		if derg.SetMaxW == nil || derg.SetMaxW.Value != 10000 {
			t.Errorf("DERSettings.SetMaxW = %v, want 10000", derg.SetMaxW)
		}
	})

	// Phase 4: DER Status Reporting
	t.Run("put_der_status", func(t *testing.T) {
		// Pass the DERStatusLink href explicitly.
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
		_, err = client.Get(ctx, guard.KindEndDeviceRead, "/edev/"+edevID+"/der/"+derID+"/ders", &status)
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
		// Pass the MirrorUsagePointList href explicitly.
		loc, err := client.CreateMirrorUsagePoint(ctx, "/mup", inverter.DeviceLFDI(client.LFDI()), sep2.MirrorUsagePoint{
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
		// Pass the MirrorMeterReadingList href explicitly.  The
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
		_, err := client.Get(ctx, guard.KindEndDeviceRead, "/tm", &tm)
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
		_, err := client.Get(ctx, guard.KindEndDeviceRead, "/edev", &list)
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
		// Pass the EndDeviceList href explicitly.
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
		_, err := client.Get(ctx, guard.KindEndDeviceRead, "/edev/"+edevID+"/fsa/1/derp/1/dderc", &dderc)
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
