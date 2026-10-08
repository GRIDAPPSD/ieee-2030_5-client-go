// Package simconfig loads the one JSON file that holds every sim-mode
// setting of an inverterclient process. Each key has a default; an unknown
// key is a load error so a misspelled setting cannot silently fall back to
// its default.
package simconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Defaults for keys the file may omit. The server, cert, key and ca
// defaults are the flag defaults and are supplied by the caller, so the two
// cannot drift.
const (
	DefaultRole           = "der"
	DefaultDeviceType     = "pv"
	DefaultRatedW         = 10000.0
	DefaultCapacityWh     = 10000.0
	DefaultInitialSOC     = 0.5
	DefaultReplayClock    = "wall"
	DefaultReplayScale    = 1.0
	DefaultReportInterval = 60
	DefaultStatusInterval = 60
	DefaultResponse       = "follow"
	DefaultFrqEnergyWh    = 6000.0
	DefaultFrqPowerW      = 3000.0
	DefaultFrqStartInS    = 2400
	DefaultFrqDurationS   = 3600
	DefaultFrqMinLeadS    = 60
	DefaultFrqPollS       = 30
	DefaultDispatchTickS  = 5
	DefaultFrqRequestFile = "frq-request.json"
)

var lfdiRE = regexp.MustCompile(`^[0-9A-Fa-f]{40}$`)

// Device is the nameplate and identity of one simulated device.
type Device struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	RatedW     float64 `json:"rated_w"`
	CapacityWh float64 `json:"capacity_wh"`
	// InitialSOC is the battery's state of charge (0 to 1) at process
	// start. A pointer so an explicit 0 (empty) differs from absent; it is
	// non-nil after Parse.
	InitialSOC *float64 `json:"initial_soc"`
}

// Replay selects and shapes the recorded-output backend.
type Replay struct {
	File  string  `json:"file"`
	Clock string  `json:"clock"`
	Scale float64 `json:"scale"`
}

// Interval is a setting held in whole seconds.
type Interval struct {
	IntervalS int `json:"interval_s"`
}

// Controls shapes how a device reacts to DER controls.
type Controls struct {
	Response string `json:"response"`
	PollS    int    `json:"poll_s"`
}

// Managed is one device an aggregator acts for. Parsed and validated here;
// the aggregator that consumes it is a later issue.
type Managed struct {
	Name     string    `json:"name"`
	LFDI     string    `json:"lfdi"`
	Device   *Device   `json:"device"`
	Replay   *Replay   `json:"replay"`
	Controls *Controls `json:"controls"`
}

// Frq holds the flow-reservation defaults an aggregator reads. Parsed and
// unused until the reservation issue.
type Frq struct {
	EnergyWh    float64 `json:"energy_wh"`
	PowerW      float64 `json:"power_w"`
	StartInS    int     `json:"start_in_s"`
	DurationS   int     `json:"duration_s"`
	MinLeadS    int     `json:"min_lead_s"`
	PollS       int     `json:"poll_s"`
	RequestFile string  `json:"request_file"`
}

// Dispatch holds aggregator dispatch settings. Parsed and unused until the
// reservation issue.
type Dispatch struct {
	TickS int `json:"tick_s"`
}

// Notify holds the notification listener address.
type Notify struct {
	// Listen is a pointer so an explicit empty string (listener disabled)
	// differs from an absent key (the flag default applies).
	Listen *string `json:"listen"`
}

// HMI holds the dashboard port.
type HMI struct {
	Port int `json:"port"`
}

// File is the decoded, defaulted and validated sim-mode config.
type File struct {
	Role string `json:"role"`
	// Server, Cert, Key and CA are empty when the key is absent: the caller
	// keeps the flag default, which is this setting's default.
	Server         string    `json:"server"`
	Cert           string    `json:"cert"`
	Key            string    `json:"key"`
	CA             string    `json:"ca"`
	LookupOwnEdev  *bool     `json:"lookup_own_edev"`
	Pin            uint      `json:"pin"`
	Device         Device    `json:"device"`
	Backend        string    `json:"backend"`
	Replay         Replay    `json:"replay"`
	Report         Interval  `json:"report"`
	Status         Interval  `json:"status"`
	Controls       Controls  `json:"controls"`
	Managed        []Managed `json:"managed"`
	Frq            Frq       `json:"frq"`
	Dispatch       Dispatch  `json:"dispatch"`
	Notify         Notify    `json:"notify"`
	HMI            HMI       `json:"hmi"`
	LookupOwnEdevV bool      `json:"-"`
}

// Load reads, strictly decodes, defaults and validates the file at path.
// Relative replay and request file paths are resolved against the config
// file's directory so the config can be moved with its recordings.
func Load(path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sim config %s: %w", path, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read sim config %s: %w", abs, err)
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("sim config %s: %w", abs, err)
	}
	dir := filepath.Dir(abs)
	if f.Device.Name == "" {
		f.Device.Name = strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	}
	f.Replay.File = resolve(dir, f.Replay.File)
	f.Frq.RequestFile = resolve(dir, f.Frq.RequestFile)
	for i := range f.Managed {
		if f.Managed[i].Replay != nil {
			f.Managed[i].Replay.File = resolve(dir, f.Managed[i].Replay.File)
		}
	}
	return f, nil
}

func resolve(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// Parse decodes b strictly (an unknown key at any depth is an error that
// names it), applies defaults and validates.
func Parse(b []byte) (*File, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse: unexpected data after the top-level object")
	}
	if f.Role == "aggregator" && f.LookupOwnEdev != nil {
		return nil, errors.New("lookup_own_edev is a der-only setting and has no effect in the aggregator role; remove it")
	}
	f.applyDefaults()
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (d *Device) applyDefaults(name string) {
	if d.Name == "" {
		d.Name = name
	}
	if d.Type == "" {
		d.Type = DefaultDeviceType
	}
	if d.RatedW == 0 {
		d.RatedW = DefaultRatedW
	}
	if d.CapacityWh == 0 {
		d.CapacityWh = DefaultCapacityWh
	}
	if d.InitialSOC == nil {
		soc := DefaultInitialSOC
		d.InitialSOC = &soc
	}
}

func (r *Replay) applyDefaults() {
	if r.Clock == "" {
		r.Clock = DefaultReplayClock
	}
	if r.Scale == 0 {
		r.Scale = DefaultReplayScale
	}
}

func (c *Controls) applyDefaults() {
	if c.Response == "" {
		c.Response = DefaultResponse
	}
}

func (f *File) applyDefaults() {
	if f.Role == "" {
		f.Role = DefaultRole
	}
	if f.LookupOwnEdev == nil && f.Role == "der" {
		t := true
		f.LookupOwnEdev = &t
	}
	f.Device.applyDefaults("")
	f.Replay.applyDefaults()
	f.Controls.applyDefaults()
	if f.Backend == "" {
		if f.Replay.File != "" {
			f.Backend = "replay"
		} else {
			f.Backend = "synthetic"
		}
	}
	if f.Report.IntervalS == 0 {
		f.Report.IntervalS = DefaultReportInterval
	}
	if f.Status.IntervalS == 0 {
		f.Status.IntervalS = DefaultStatusInterval
	}
	for i := range f.Managed {
		m := &f.Managed[i]
		if m.Device == nil {
			m.Device = &Device{}
		}
		m.Device.applyDefaults(m.Name)
		if m.Replay == nil {
			m.Replay = &Replay{}
		}
		m.Replay.applyDefaults()
		if m.Controls == nil {
			m.Controls = &Controls{}
		}
		m.Controls.applyDefaults()
	}
	if f.Frq.EnergyWh == 0 {
		f.Frq.EnergyWh = DefaultFrqEnergyWh
	}
	if f.Frq.PowerW == 0 {
		f.Frq.PowerW = DefaultFrqPowerW
	}
	if f.Frq.StartInS == 0 {
		f.Frq.StartInS = DefaultFrqStartInS
	}
	if f.Frq.DurationS == 0 {
		f.Frq.DurationS = DefaultFrqDurationS
	}
	if f.Frq.MinLeadS == 0 {
		f.Frq.MinLeadS = DefaultFrqMinLeadS
	}
	if f.Frq.PollS == 0 {
		f.Frq.PollS = DefaultFrqPollS
	}
	if f.Frq.RequestFile == "" {
		f.Frq.RequestFile = DefaultFrqRequestFile
	}
	if f.Dispatch.TickS == 0 {
		f.Dispatch.TickS = DefaultDispatchTickS
	}
}

func oneOf(key, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s %q must be one of %s", key, v, strings.Join(allowed, ", "))
}

func (d Device) validate(prefix string) error {
	if err := oneOf(prefix+"type", d.Type, "pv", "battery"); err != nil {
		return err
	}
	if d.RatedW <= 0 {
		return fmt.Errorf("%srated_w %v must be positive", prefix, d.RatedW)
	}
	if d.CapacityWh <= 0 {
		return fmt.Errorf("%scapacity_wh %v must be positive", prefix, d.CapacityWh)
	}
	if *d.InitialSOC < 0 || *d.InitialSOC > 1 {
		return fmt.Errorf("%sinitial_soc %v must be within 0 to 1", prefix, *d.InitialSOC)
	}
	return nil
}

func (r Replay) validate(prefix string) error {
	if err := oneOf(prefix+"clock", r.Clock, "wall", "start"); err != nil {
		return err
	}
	if r.Scale < 0 {
		return fmt.Errorf("%sscale %v must not be negative", prefix, r.Scale)
	}
	return nil
}

func (c Controls) validate(prefix string) error {
	if err := oneOf(prefix+"response", c.Response, "follow", "ack", "none"); err != nil {
		return err
	}
	if c.PollS < 0 {
		return fmt.Errorf("%spoll_s %d must not be negative", prefix, c.PollS)
	}
	return nil
}

func (f *File) validate() error {
	if err := oneOf("role", f.Role, "der", "aggregator"); err != nil {
		return err
	}
	if err := oneOf("backend", f.Backend, "replay", "synthetic"); err != nil {
		return err
	}
	if f.Backend == "replay" && f.Replay.File == "" {
		return errors.New("backend \"replay\" needs replay.file")
	}
	if err := f.Device.validate("device."); err != nil {
		return err
	}
	if err := f.Replay.validate("replay."); err != nil {
		return err
	}
	if err := f.Controls.validate("controls."); err != nil {
		return err
	}
	for name, v := range map[string]int{
		"report.interval_s": f.Report.IntervalS, "status.interval_s": f.Status.IntervalS,
		"frq.start_in_s": f.Frq.StartInS, "frq.duration_s": f.Frq.DurationS,
		"frq.min_lead_s": f.Frq.MinLeadS, "frq.poll_s": f.Frq.PollS,
		"dispatch.tick_s": f.Dispatch.TickS,
	} {
		if v < 0 {
			return fmt.Errorf("%s %d must not be negative", name, v)
		}
	}
	if f.HMI.Port < 0 || f.HMI.Port > 65535 {
		return fmt.Errorf("hmi.port %d must be within 0 to 65535", f.HMI.Port)
	}
	if f.Role == "aggregator" && len(f.Managed) == 0 {
		return errors.New("role \"aggregator\" needs at least one managed device")
	}
	seen := map[string]bool{}
	for i, m := range f.Managed {
		p := fmt.Sprintf("managed[%d].", i)
		if m.Name == "" {
			return fmt.Errorf("%sname is required", p)
		}
		if !lfdiRE.MatchString(m.LFDI) {
			return fmt.Errorf("%slfdi %q is not 40 hexadecimal characters", p, m.LFDI)
		}
		key := strings.ToUpper(m.LFDI)
		if seen[key] {
			return fmt.Errorf("%slfdi %s is listed twice", p, key)
		}
		seen[key] = true
		if err := m.Device.validate(p + "device."); err != nil {
			return err
		}
		if err := m.Replay.validate(p + "replay."); err != nil {
			return err
		}
		if err := m.Controls.validate(p + "controls."); err != nil {
			return err
		}
	}
	return nil
}
