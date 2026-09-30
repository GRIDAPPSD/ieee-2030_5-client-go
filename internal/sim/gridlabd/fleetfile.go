package gridlabd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// hex40 matches an LFDI: exactly 40 hexadecimal characters (#35's rule).
var hex40 = regexp.MustCompile(`^[0-9A-Fa-f]{40}$`)

// safeFleetName matches the same character set the one existing generator
// validates its fleet name against (models/battery_fleet.py's
// _SAFE_NAME), so a fleet name can never carry a path separator or "..":
// it becomes a socket file name under RunDir (Fleet+".sock") with no
// further escaping.
var safeFleetName = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// DeviceEntry is one managed device's identity and simulator mapping, as
// written by sim/gridlabd/models/battery_fleet.py's generator.
type DeviceEntry struct {
	Name    string            `json:"name"`
	LFDI    string            `json:"lfdi"`
	Objects map[string]string `json:"objects"` // class name -> object name, e.g. "inverter" -> "fleet_bat0_inv"
}

// FleetFile is one fleet's device list and the GLM it drives, as written by
// the generator and read by both this package (to launch the sidecar) and
// __main__.py (to build its Adapter's expected version and objects).
type FleetFile struct {
	Fleet           string        `json:"fleet"`
	GridlabdVersion string        `json:"gridlabd_version"`
	AggregatorPEN   string        `json:"aggregator_pen"`
	GLM             string        `json:"glm"`
	Devices         []DeviceEntry `json:"devices"`
	// Meter is the shared coupling-point object read for grid voltage and
	// frequency. Optional: today's generator does not write this field, so
	// an empty value falls back to the generator's known convention
	// (defaultFleetMeterObject); a later generator may set it explicitly.
	Meter string `json:"meter,omitempty"`
	// Interpreter is the absolute path to the Python interpreter this
	// fleet's sidecar is launched with (operator decision 2026-09-30, Q2:
	// the interpreter is named per fleet, not by a manager-wide flag).
	// Optional: empty means "python3 resolved from PATH", the existing
	// default. Validated at load time (Validate below): must be an
	// absolute path to an existing, executable file, so a wrong value is a
	// fleet-file load error naming the fleet, not a launch failure once
	// the sidecar process is already being started.
	Interpreter string `json:"interpreter,omitempty"`

	path string // absolute path this was loaded from; anchors GLMPath
}

// LoadFleetFile reads and validates path. Every device's LFDI is
// normalized to uppercase, per #35's rule, before this returns.
func LoadFleetFile(path string) (*FleetFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("fleet file %s: %w", path, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read fleet file %s: %w", abs, err)
	}
	var f FleetFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse fleet file %s: %w", abs, err)
	}
	f.path = abs
	if err := f.Validate(); err != nil {
		return nil, err
	}
	for i := range f.Devices {
		f.Devices[i].LFDI = strings.ToUpper(f.Devices[i].LFDI)
	}
	return &f, nil
}

// GLMPath returns the GLM file's absolute path, resolved next to the fleet
// file (the generator writes both into the same directory).
func (f *FleetFile) GLMPath() string {
	return filepath.Join(filepath.Dir(f.path), f.GLM)
}

// Validate checks the rules issue #35 states for a device mapping: every
// LFDI is 40 hex characters, no LFDI is mapped twice, no object name is
// mapped to two devices, and every device names at least one object.
func (f *FleetFile) Validate() error {
	if f.Fleet == "" {
		return fmt.Errorf("fleet file %s: fleet name is required", f.path)
	}
	if !safeFleetName.MatchString(f.Fleet) {
		return fmt.Errorf("fleet file %s: fleet name %q must be 1-32 characters of [A-Za-z0-9_]", f.path, f.Fleet)
	}
	if f.GLM == "" {
		return fmt.Errorf("fleet file %s: glm path is required", f.path)
	}
	if len(f.Devices) == 0 {
		return fmt.Errorf("fleet file %s: has no devices", f.Fleet)
	}
	seenLFDI := make(map[string]string, len(f.Devices))
	seenObject := make(map[string]string, len(f.Devices)*2)
	for _, dev := range f.Devices {
		if dev.Name == "" {
			return fmt.Errorf("fleet file %s: a device has no name", f.Fleet)
		}
		if !hex40.MatchString(dev.LFDI) {
			return fmt.Errorf("fleet file %s: device %s: LFDI %q is not 40 hexadecimal characters", f.Fleet, dev.Name, dev.LFDI)
		}
		norm := strings.ToUpper(dev.LFDI)
		if other, dup := seenLFDI[norm]; dup {
			return fmt.Errorf("fleet file %s: LFDI %s is mapped to two devices (%s and %s)", f.Fleet, norm, other, dev.Name)
		}
		seenLFDI[norm] = dev.Name
		if len(dev.Objects) == 0 {
			return fmt.Errorf("fleet file %s: device %s has no object mapping", f.Fleet, dev.Name)
		}
		for class, obj := range dev.Objects {
			if obj == "" {
				return fmt.Errorf("fleet file %s: device %s: empty object name for class %s", f.Fleet, dev.Name, class)
			}
			if other, dup := seenObject[obj]; dup {
				return fmt.Errorf("fleet file %s: object %s is mapped to two devices (%s and %s)", f.Fleet, obj, other, dev.Name)
			}
			seenObject[obj] = dev.Name
		}
	}
	if f.Interpreter != "" {
		if !filepath.IsAbs(f.Interpreter) {
			return fmt.Errorf("fleet file %s: interpreter %q must be an absolute path", f.Fleet, f.Interpreter)
		}
		info, err := os.Stat(f.Interpreter)
		if err != nil {
			return fmt.Errorf("fleet file %s: interpreter %s: %w", f.Fleet, f.Interpreter, err)
		}
		if info.IsDir() {
			return fmt.Errorf("fleet file %s: interpreter %s is a directory, not an executable", f.Fleet, f.Interpreter)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("fleet file %s: interpreter %s is not executable", f.Fleet, f.Interpreter)
		}
	}
	return nil
}
