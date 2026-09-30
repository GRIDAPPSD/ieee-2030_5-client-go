package gridlabd

import (
	"errors"
	"testing"
)

// TestNewManager_DERRoleWithFleetFile_Refuses is the construction-time
// gate: a fleet file given outside the aggregator role is a startup
// error, and nothing is built (no supervisor, no device); the der role
// never constructs the managed-set walk or the device mapping loader.
func TestNewManager_DERRoleWithFleetFile_Refuses(t *testing.T) {
	dir := t.TempDir()
	path := writeFleetFile(t, dir, validFleetFile())

	m, err := NewManager(ManagerConfig{
		Role:       "der",
		FleetFiles: []string{path},
		RunDir:     dir,
	})
	if !errors.Is(err, ErrNotAggregatorRole) {
		t.Fatalf("NewManager error = %v, want ErrNotAggregatorRole", err)
	}
	if m != nil {
		t.Errorf("NewManager returned a non-nil Manager in the der role: %+v", m)
	}
}

// TestNewManager_DERRoleNoFleetFiles_IsOrdinaryRun proves the der role
// with no fleet file is not an error: today's ordinary DER-client
// invocation must keep working unchanged.
func TestNewManager_DERRoleNoFleetFiles_IsOrdinaryRun(t *testing.T) {
	m, err := NewManager(ManagerConfig{Role: "der"})
	if err != nil {
		t.Fatalf("NewManager (der, no fleets): %v", err)
	}
	if m != nil {
		t.Errorf("NewManager (der, no fleets) = %+v, want nil", m)
	}
}

func TestNewManager_AggregatorRole_BuildsSupervisorsAndDevices(t *testing.T) {
	dir := t.TempDir()
	path := writeFleetFile(t, dir, validFleetFile())

	m, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{path},
		RunDir:     dir,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if len(m.Supervisors) != 1 {
		t.Fatalf("len(Supervisors) = %d, want 1", len(m.Supervisors))
	}
	if len(m.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2 (one per fleet device)", len(m.Devices))
	}
	gotLFDIs := map[string]bool{m.Devices[0].LFDI(): true, m.Devices[1].LFDI(): true}
	for _, want := range []string{"0FA437FCD2BDADDA3EF8FEFAFF4A3D1612345678", "AD882EC29CFC0A0B500B663AD841670E12345678"} {
		if !gotLFDIs[want] {
			t.Errorf("Devices missing LFDI %s", want)
		}
	}
}

func TestNewManager_AggregatorRole_InvalidFleetFileRefuses(t *testing.T) {
	dir := t.TempDir()
	ff := validFleetFile()
	ff["devices"].([]map[string]any)[0]["lfdi"] = "too-short"
	path := writeFleetFile(t, dir, ff)

	if _, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{path},
		RunDir:     dir,
	}); err == nil {
		t.Fatal("NewManager: want error on an invalid fleet file, got nil")
	}
}
