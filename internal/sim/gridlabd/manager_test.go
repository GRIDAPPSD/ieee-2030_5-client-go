package gridlabd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		RunDir:     shortSockDir(t), // t.TempDir() here can exceed the AF_UNIX sun_path limit once joined with the fleet name and socket suffix
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

// secondFleetFile returns a second, independently valid fleet file
// (distinct device objects and, by default, a distinct fleet name and
// LFDIs from validFleetFile), for cross-file duplicate tests.
func secondFleetFile() map[string]any {
	return map[string]any{
		"fleet":            "other",
		"gridlabd_version": "6.0.0a1",
		"aggregator_pen":   "12345678",
		"glm":              "other.glm",
		"devices": []map[string]any{
			{
				"name": "other-000",
				"lfdi": "1111111111111111111111111111111111111111",
				"objects": map[string]string{
					"inverter": "other_bat0_inv",
					"battery":  "other_bat0",
				},
			},
		},
	}
}

func TestNewManager_CrossFileDuplicateFleetNameRefuses(t *testing.T) {
	dir := t.TempDir()
	ff1 := validFleetFile()
	pathA := writeNamedFleetFile(t, dir, "a.fleet.json", ff1)
	ff2 := secondFleetFile()
	ff2["fleet"] = ff1["fleet"] // same name as the first file
	pathB := writeNamedFleetFile(t, dir, "b.fleet.json", ff2)

	if _, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{pathA, pathB},
		RunDir:     dir,
	}); err == nil {
		t.Fatal("NewManager: want error on two fleet files sharing a fleet name, got nil")
	}
}

// TestNewManager_CrossFileDuplicateLFDIRefuses proves the compare is
// case-insensitive: the two files' LFDIs differ only in case.
func TestNewManager_CrossFileDuplicateLFDIRefuses(t *testing.T) {
	dir := t.TempDir()
	ff1 := validFleetFile()
	sharedLFDI := ff1["devices"].([]map[string]any)[0]["lfdi"].(string)
	pathA := writeNamedFleetFile(t, dir, "a.fleet.json", ff1)

	ff2 := secondFleetFile()
	ff2["devices"].([]map[string]any)[0]["lfdi"] = toggleCase(sharedLFDI)
	pathB := writeNamedFleetFile(t, dir, "b.fleet.json", ff2)

	if _, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{pathA, pathB},
		RunDir:     dir,
	}); err == nil {
		t.Fatal("NewManager: want error on the same LFDI (case-insensitively) used across two fleet files, got nil")
	}
}

func writeNamedFleetFile(t *testing.T, dir, name string, ff map[string]any) string {
	t.Helper()
	sub := filepath.Join(dir, name)
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}
	return writeFleetFile(t, sub, ff)
}

func toggleCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'F' {
			b[i] = c - 'A' + 'a'
		} else if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func TestPrepareRunDir_CreatesMode0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rundir")
	if err := prepareRunDir(dir); err != nil {
		t.Fatalf("prepareRunDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %o, want 0700", got)
	}
}

func TestPrepareRunDir_RefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges on windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := prepareRunDir(link); err == nil {
		t.Fatal("prepareRunDir(symlink): want error, got nil")
	}
}

func TestPrepareRunDir_RefusesGroupOrWorldWritable(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "rundir")
	if err := os.Mkdir(sub, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mkdir's mode is filtered by the process umask (typically 022), so
	// 0777 often lands as 0755 with no group/world write bit set; Chmod
	// is not filtered and proves the case regardless of umask.
	if err := os.Chmod(sub, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := prepareRunDir(sub); err == nil {
		t.Fatal("prepareRunDir(0777): want error, got nil")
	}
}

func TestPrepareRunDir_AcceptsExistingPrivateDir(t *testing.T) {
	dir := t.TempDir() // t.TempDir() itself is 0700
	if err := prepareRunDir(dir); err != nil {
		t.Errorf("prepareRunDir(existing 0700 dir): %v", err)
	}
}

// TestPrepareRunDir_RefusesGroupWritableOnly is the mutation-killing case
// for &0o022 loosened to &0o002: 0750 (group-readable and -executable,
// NOT group-writable) must pass, and 0770 (group-writable, not
// world-writable) must be refused on the group bit alone.
func TestPrepareRunDir_RefusesGroupWritableOnly(t *testing.T) {
	dir := t.TempDir()
	readOnlyGroup := filepath.Join(dir, "ro-group")
	if err := os.Mkdir(readOnlyGroup, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(readOnlyGroup, 0o750); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := prepareRunDir(readOnlyGroup); err != nil {
		t.Errorf("prepareRunDir(0750): %v, want nil (group-writable bit is not set)", err)
	}

	groupWritable := filepath.Join(dir, "rw-group")
	if err := os.Mkdir(groupWritable, 0o770); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(groupWritable, 0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := prepareRunDir(groupWritable); err == nil {
		t.Error("prepareRunDir(0770): want error, got nil (group-writable, not just world-writable)")
	}
}

// TestNewManager_AggregatorRoleNoFleetFiles_IsOrdinaryRun is the
// regression item 3 names: --client-role aggregator with no --fleet-file
// is an existing, valid invocation (#71) and must not now require RunDir.
func TestNewManager_AggregatorRoleNoFleetFiles_IsOrdinaryRun(t *testing.T) {
	m, err := NewManager(ManagerConfig{Role: AggregatorRole})
	if err != nil {
		t.Fatalf("NewManager(aggregator role, no fleet files, no RunDir): %v, want nil", err)
	}
	if m != nil {
		t.Errorf("NewManager(aggregator role, no fleet files) = %+v, want nil", m)
	}
}

// The error text is asserted exactly, not only err != nil, so removing
// this guard (item 2's coverage LOW) fails the test rather than passing
// against some other, unrelated error.
func TestNewManager_RunDirRequiredWithFleetFiles(t *testing.T) {
	dir := t.TempDir()
	path := writeFleetFile(t, dir, validFleetFile())
	_, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{path},
		// RunDir deliberately omitted.
	})
	if err == nil {
		t.Fatal("NewManager(fleet file, no RunDir): want error, got nil")
	}
	const want = "gridlabd: RunDir is required when FleetFiles is non-empty"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// The error text is asserted exactly, not only err != nil (item 2's
// coverage LOW): removing this guard would fail with some other error
// (a nil pointer, or success), and this test would not notice without it.
func TestNewManager_NoInverterObjectMappedRefuses(t *testing.T) {
	dir := t.TempDir()
	ff := validFleetFile()
	devices := ff["devices"].([]map[string]any)
	devices[0]["objects"] = map[string]string{"battery": "probe_bat0"} // no "inverter" key
	path := writeFleetFile(t, dir, ff)

	_, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{path},
		RunDir:     shortSockDir(t), // t.TempDir() here can exceed the AF_UNIX sun_path limit once joined with the fleet name and socket suffix
	})
	if err == nil {
		t.Fatal("NewManager(device with no inverter object): want error, got nil")
	}
	const want = "fleet probe device probe-000: no inverter object mapped"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestNewManager_RelativeRunDirResolvesToAbsolute is item 2: a relative
// RunDir must not let the Go side stat a socket path relative to this
// process's own cwd while the sidecar (whose Dir is RunDir) binds it
// relative to its own : two different files on any cwd other than
// RunDir's parent. In-package so the built Supervisor's unexported cfg is
// directly inspectable, per the design.
func TestNewManager_RelativeRunDirResolvesToAbsolute(t *testing.T) {
	dir := shortSockDir(t) // short and absolute: dir/rundir/probe.sock must fit the AF_UNIX sun_path limit
	t.Chdir(dir)
	path := writeFleetFile(t, dir, validFleetFile())

	m, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{path},
		RunDir:     "rundir", // relative
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if len(m.Supervisors) != 1 {
		t.Fatalf("len(Supervisors) = %d, want 1", len(m.Supervisors))
	}
	sup := m.Supervisors[0]
	wantDir := filepath.Join(dir, "rundir")
	if sup.cfg.Dir != wantDir {
		t.Errorf("Supervisor Dir = %q, want %q (absolute)", sup.cfg.Dir, wantDir)
	}
	wantSock := filepath.Join(wantDir, "probe.sock")
	if sup.cfg.SocketPath != wantSock {
		t.Errorf("Supervisor SocketPath = %q, want %q (absolute)", sup.cfg.SocketPath, wantSock)
	}
}

// TestNewManager_OverlongSocketPathRefusesNamingFleet is item 2: a socket
// path over the AF_UNIX sun_path limit is refused at construction, naming
// the fleet, rather than surfacing as a dial failure after the first
// start's full StartTimeout.
func TestNewManager_OverlongSocketPathRefusesNamingFleet(t *testing.T) {
	base := t.TempDir()
	long := strings.Repeat("x", maxSocketPathLen)
	runDir := filepath.Join(base, long)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := writeFleetFile(t, base, validFleetFile()) // fleet name "probe"

	_, err := NewManager(ManagerConfig{
		Role:       AggregatorRole,
		FleetFiles: []string{path},
		RunDir:     runDir,
	})
	if err == nil {
		t.Fatal("NewManager with an over-long socket path: want error, got nil")
	}
	if !strings.Contains(err.Error(), "probe") {
		t.Errorf("error %q does not name the fleet %q", err.Error(), "probe")
	}
}

// TestPrepareRunDir_RootIsRefusedByOwner proves the owner check actually
// fires on a real, unwritable-by-us directory (not just the synthetic
// 0700 t.TempDir() every other prepareRunDir test constructs): "/" exists,
// is a directory, and (on any sane host) is not group- or world-writable,
// so it reaches and is refused by the ownership check specifically.
func TestPrepareRunDir_RootIsRefusedByOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: / is root-owned, so the owner check would not fire")
	}
	err := prepareRunDir("/")
	if err == nil {
		t.Fatal(`prepareRunDir("/"): want error, got nil`)
	}
	if !strings.Contains(err.Error(), "not owned by the current user") {
		t.Errorf(`prepareRunDir("/") error = %q, want it to name the owner check`, err.Error())
	}
}
