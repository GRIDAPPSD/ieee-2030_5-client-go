package gridlabd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestLoadFleetFile_RejectsCaseDifferingDuplicateLFDI is the
// mutation-killing case for "norm := dev.LFDI" (dropping ToUpper):
// TestLoadFleetFile_RejectsDuplicateLFDI uses byte-identical strings, so
// it cannot tell a normalizing compare from a literal one. Two LFDIs
// differing only in case can.
func TestLoadFleetFile_RejectsCaseDifferingDuplicateLFDI(t *testing.T) {
	ff := validFleetFile()
	devices := ff["devices"].([]map[string]any)
	original := devices[0]["lfdi"].(string)
	devices[1]["lfdi"] = strings.ToUpper(original)
	if devices[0]["lfdi"] == devices[1]["lfdi"] {
		t.Fatal("test setup: the two LFDIs must differ only in case, not be identical")
	}
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on two LFDIs differing only in case, got nil")
	}
}

// TestLoadFleetFile_LFDILengthBoundary is the mutation-killing case for
// "{40}" loosened to "{1,40}": 39 and 41 genuinely-hex characters (no
// non-hex byte to trip a weaker check first) must both be refused, and 40
// must be accepted.
func TestLoadFleetFile_LFDILengthBoundary(t *testing.T) {
	base := "0FA437FCD2BDADDA3EF8FEFAFF4A3D1612345678" // 40 hex chars (verified: len() below)
	if len(base) != 40 {
		t.Fatalf("test setup: base is %d characters, want 40", len(base))
	}
	tests := []struct {
		name    string
		lfdi    string
		wantErr bool
	}{
		{"39 hex chars", base[:39], true},
		{"40 hex chars", base, false},
		{"41 hex chars", base + "8", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ff := validFleetFile()
			devices := ff["devices"].([]map[string]any)
			devices[0]["lfdi"] = tt.lfdi
			devices[1]["lfdi"] = "AD882EC29CFC0A0B500B663AD841670E12345678" // unaffected, unique
			dir := t.TempDir()
			path := writeFleetFile(t, dir, ff)

			_, err := LoadFleetFile(path)
			if tt.wantErr && err == nil {
				t.Errorf("LoadFleetFile(%d hex chars): want error, got nil", len(tt.lfdi))
			}
			if !tt.wantErr && err != nil {
				t.Errorf("LoadFleetFile(%d hex chars): %v, want success", len(tt.lfdi), err)
			}
		})
	}
}

func TestLoadFleetFile_RejectsUnsafeFleetName(t *testing.T) {
	tests := []string{
		"../../escape",
		"a/b",
		"a\\b",
		"",
		strings.Repeat("a", 33), // 33 chars, over the 32 limit
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			ff := validFleetFile()
			ff["fleet"] = name
			dir := t.TempDir()
			path := writeFleetFile(t, dir, ff)
			if _, err := LoadFleetFile(path); err == nil {
				t.Errorf("LoadFleetFile(fleet=%q): want error, got nil", name)
			}
		})
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

// Q2 (operator decision 2026-09-30): the fleet file names its own
// interpreter, validated at load.

func TestLoadFleetFile_InterpreterOmittedIsValid(t *testing.T) {
	ff := validFleetFile() // no "interpreter" key
	dir := t.TempDir()
	path := writeFleetFile(t, dir, ff)

	loaded, err := LoadFleetFile(path)
	if err != nil {
		t.Fatalf("LoadFleetFile: %v", err)
	}
	if loaded.Interpreter != "" {
		t.Errorf("Interpreter = %q, want empty (PATH default)", loaded.Interpreter)
	}
}

func TestLoadFleetFile_AcceptsAbsoluteExecutableInterpreter(t *testing.T) {
	dir := t.TempDir()
	interp := filepath.Join(dir, "python3")
	if err := os.WriteFile(interp, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write fake interpreter: %v", err)
	}
	ff := validFleetFile()
	ff["interpreter"] = interp
	path := writeFleetFile(t, dir, ff)

	loaded, err := LoadFleetFile(path)
	if err != nil {
		t.Fatalf("LoadFleetFile: %v", err)
	}
	if loaded.Interpreter != interp {
		t.Errorf("Interpreter = %q, want %q", loaded.Interpreter, interp)
	}
}

func TestLoadFleetFile_RejectsRelativeInterpreter(t *testing.T) {
	// The relative name resolves to an executable file from the process
	// cwd, so the stat and the exec-bit checks would both pass: only the
	// IsAbs check can refuse it. A relative interpreter would otherwise
	// mean something different per launch directory.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "py"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write interpreter: %v", err)
	}
	t.Chdir(dir)
	ff := validFleetFile()
	ff["interpreter"] = "py"
	path := writeFleetFile(t, dir, ff)

	_, err := LoadFleetFile(path)
	if err == nil {
		t.Fatal("LoadFleetFile: want error on a relative interpreter path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want the absolute-path refusal", err)
	}
}

func TestLoadFleetFile_RejectsMissingInterpreter(t *testing.T) {
	dir := t.TempDir()
	ff := validFleetFile()
	ff["interpreter"] = filepath.Join(dir, "does-not-exist")
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on a nonexistent interpreter path, got nil")
	}
}

func TestLoadFleetFile_RejectsNonExecutableInterpreter(t *testing.T) {
	dir := t.TempDir()
	interp := filepath.Join(dir, "python3")
	if err := os.WriteFile(interp, []byte("not a script"), 0o600); err != nil { // 0600: no +x bit
		t.Fatalf("write non-executable interpreter: %v", err)
	}
	ff := validFleetFile()
	ff["interpreter"] = interp
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error on a non-executable interpreter, got nil")
	}
}

func TestLoadFleetFile_RejectsInterpreterThatIsADirectory(t *testing.T) {
	dir := t.TempDir()
	interpDir := filepath.Join(dir, "python3")
	if err := os.Mkdir(interpDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ff := validFleetFile()
	ff["interpreter"] = interpDir
	path := writeFleetFile(t, dir, ff)

	if _, err := LoadFleetFile(path); err == nil {
		t.Fatal("LoadFleetFile: want error when interpreter names a directory, got nil")
	}
}
