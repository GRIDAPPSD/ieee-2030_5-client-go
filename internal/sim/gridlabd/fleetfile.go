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
	return nil
}
