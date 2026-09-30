package gridlabd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFleetFile(t *testing.T, dir string, ff map[string]any) string {
	t.Helper()
	b, err := json.Marshal(ff)
	if err != nil {
		t.Fatalf("marshal fleet file: %v", err)
	}
	path := filepath.Join(dir, "test.fleet.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write fleet file: %v", err)
	}
	return path
}

func validFleetFile() map[string]any {
	return map[string]any{
		"fleet":            "probe",
		"gridlabd_version": "6.0.0a1",
		"aggregator_pen":   "12345678",
		"glm":              "probe.glm",
		"devices": []map[string]any{
			{
				"name": "probe-000",
				"lfdi": "0fa437fcd2bdadda3ef8fefaff4a3d1612345678",
				"objects": map[string]string{
					"inverter": "probe_bat0_inv",
					"battery":  "probe_bat0",
				},
			},
			{
				"name": "probe-001",
				"lfdi": "AD882EC29CFC0A0B500B663AD841670E12345678",
				"objects": map[string]string{
					"inverter": "probe_bat1_inv",
					"battery":  "probe_bat1",
				},
			},
		},
	}
}

func TestLoadFleetFile_Valid(t *testing.T) {
	dir := t.TempDir()
	path := writeFleetFile(t, dir, validFleetFile())

	ff, err := LoadFleetFile(path)
	if err != nil {
		t.Fatalf("LoadFleetFile: %v", err)
	}
	if ff.Fleet != "probe" {
		t.Errorf("Fleet = %q, want probe", ff.Fleet)
	}
	if len(ff.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2", len(ff.Devices))
	}
	// Mixed-case input is normalized to uppercase (#35's rule).
	if ff.Devices[0].LFDI != "0FA437FCD2BDADDA3EF8FEFAFF4A3D1612345678" {
		t.Errorf("Devices[0].LFDI = %q, want normalized uppercase", ff.Devices[0].LFDI)
	}
	wantGLM := filepath.Join(dir, "probe.glm")
	if got := ff.GLMPath(); got != wantGLM {
		t.Errorf("GLMPath() = %q, want %q", got, wantGLM)
	}
}

func TestLoadFleetFile_RejectsShortLFDI(t *testing.T) {
	ff := validFleetFile()
	devices := ff["devices"].([]map[string]any)
	devices[0]["lfdi"] = "not-40-hex-chars"
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on a non-40-hex LFDI, got nil")
	}
}

func TestLoadFleetFile_RejectsDuplicateLFDI(t *testing.T) {
	ff := validFleetFile()
	devices := ff["devices"].([]map[string]any)
	devices[1]["lfdi"] = devices[0]["lfdi"]
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on a duplicate LFDI, got nil")
	}
}

func TestLoadFleetFile_RejectsDuplicateObject(t *testing.T) {
	ff := validFleetFile()
	devices := ff["devices"].([]map[string]any)
	devices[1]["objects"] = map[string]string{"inverter": devices[0]["objects"].(map[string]string)["inverter"]}
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error when two devices map to the same object, got nil")
	}
}

func TestLoadFleetFile_RejectsEmptyObjectName(t *testing.T) {
	ff := validFleetFile()
	devices := ff["devices"].([]map[string]any)
	devices[0]["objects"] = map[string]string{"inverter": ""}
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on an empty object name, got nil")
	}
}

func TestLoadFleetFile_RejectsNoDevices(t *testing.T) {
	ff := validFleetFile()
	ff["devices"] = []map[string]any{}
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on a fleet with no devices, got nil")
	}
}
