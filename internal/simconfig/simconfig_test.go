package simconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAppliesEveryDefault(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Role != "der" || f.Backend != "synthetic" || f.LookupOwnEdev == nil || !*f.LookupOwnEdev {
		t.Errorf("role=%q backend=%q lookup=%v, want der synthetic true", f.Role, f.Backend, f.LookupOwnEdev)
	}
	d := f.Device
	if d.Type != "pv" || d.RatedW != 10000 || d.CapacityWh != 10000 || *d.InitialSOC != 0.5 {
		t.Errorf("device = %+v soc=%v", d, *d.InitialSOC)
	}
	if f.Replay.Clock != "wall" || f.Replay.Scale != 1 || f.Report.IntervalS != 60 || f.Status.IntervalS != 60 || f.Controls.Response != "follow" {
		t.Errorf("replay=%+v report=%d status=%d response=%q", f.Replay, f.Report.IntervalS, f.Status.IntervalS, f.Controls.Response)
	}
	q := f.Frq
	if q.EnergyWh != 6000 || q.PowerW != 3000 || q.StartInS != 2400 || q.DurationS != 3600 || q.MinLeadS != 60 || q.PollS != 30 || q.RequestFile != "frq-request.json" || f.Dispatch.TickS != 5 {
		t.Errorf("frq=%+v dispatch=%+v", q, f.Dispatch)
	}
}

func TestParseKeepsExplicitValuesIncludingZeroSOC(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte(`{"device":{"type":"battery","rated_w":5000,"capacity_wh":8000,"initial_soc":0},"replay":{"file":"x.csv","clock":"start","scale":0.5},"controls":{"response":"ack","poll_s":120},"notify":{"listen":""}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Backend != "replay" {
		t.Errorf("backend = %q, want replay once replay.file is set", f.Backend)
	}
	if f.Device.RatedW != 5000 || f.Device.CapacityWh != 8000 || *f.Device.InitialSOC != 0 || f.Device.Type != "battery" {
		t.Errorf("device = %+v soc=%v", f.Device, *f.Device.InitialSOC)
	}
	if f.Replay.Scale != 0.5 || f.Replay.Clock != "start" || f.Controls.Response != "ack" || f.Controls.PollS != 120 {
		t.Errorf("replay=%+v controls=%+v", f.Replay, f.Controls)
	}
	if f.Notify.Listen == nil || *f.Notify.Listen != "" {
		t.Errorf("notify.listen = %v, want an explicit empty string", f.Notify.Listen)
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	lfdi := strings.Repeat("a", 40)
	mgd := func(extra string) string {
		return `{"role":"aggregator","managed":[{"name":"b","lfdi":"` + lfdi + `"` + extra + `}]}`
	}
	for _, tc := range []struct{ name, body, want string }{
		{"unknown top-level key", `{"colour":1}`, "colour"},
		{"unknown nested key", `{"frq":{"energy":1}}`, "energy"},
		{"unknown key in a managed entry", mgd(`,"bogus":1`), "bogus"},
		{"trailing data", `{} {}`, "after the top-level"},
		{"bad role", `{"role":"boss"}`, "boss"},
		{"bad device type", `{"device":{"type":"wind"}}`, "wind"},
		{"negative rating", `{"device":{"rated_w":-1}}`, "rated_w"},
		{"soc above 1", `{"device":{"initial_soc":1.5}}`, "initial_soc"},
		{"bad clock", `{"replay":{"clock":"sundial"}}`, "sundial"},
		{"bad response", `{"controls":{"response":"obey"}}`, "obey"},
		{"replay without a file", `{"backend":"replay"}`, "replay.file"},
		{"aggregator without managed", `{"role":"aggregator"}`, "managed"},
		{"aggregator with lookup_own_edev", mgd(``)[:len(mgd(``))-1] + `,"lookup_own_edev":true}`, "lookup_own_edev"},
		{"short lfdi", `{"role":"aggregator","managed":[{"name":"b","lfdi":"abc"}]}`, "managed[0].lfdi"},
		{"duplicate lfdi", `{"role":"aggregator","managed":[{"name":"a","lfdi":"` + lfdi + `"},{"name":"b","lfdi":"` + strings.ToUpper(lfdi) + `"}]}`, "twice"},
		{"managed device type", mgd(`,"device":{"type":"wind"}`), "managed[0].device.type"},
		{"hmi port range", `{"hmi":{"port":70000}}`, "hmi.port"},
		{"negative interval", `{"report":{"interval_s":-5}}`, "report.interval_s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.body))
			if err == nil {
				t.Fatalf("Parse accepted %s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseAcceptsAggregatorWithManagedDefaults(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte(`{"role":"aggregator","managed":[{"name":"agg-bat-1","lfdi":"` + strings.Repeat("b", 40) + `","device":{"type":"battery","rated_w":5000}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := f.Managed[0]
	if f.LookupOwnEdev != nil {
		t.Errorf("aggregator lookup_own_edev = %v, want unset", *f.LookupOwnEdev)
	}
	if m.Device.Type != "battery" || m.Device.Name != "agg-bat-1" || m.Device.CapacityWh != 10000 || m.Replay.Clock != "wall" || m.Controls.Response != "follow" {
		t.Errorf("managed = %+v device=%+v", m, *m.Device)
	}
}

func TestLoadResolvesPathsAndNamesDeviceFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "solo-pv-1.json")
	if err := os.WriteFile(p, []byte(`{"replay":{"file":"rec/pv.csv"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Replay.File != filepath.Join(dir, "rec", "pv.csv") {
		t.Errorf("replay.file = %q", f.Replay.File)
	}
	if f.Device.Name != "solo-pv-1" {
		t.Errorf("device.name = %q, want the file base name", f.Device.Name)
	}
	if f.Frq.RequestFile != filepath.Join(dir, "frq-request.json") {
		t.Errorf("frq.request_file = %q, want beside the config", f.Frq.RequestFile)
	}
	if _, err := Load(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("Load accepted a missing file")
	}
}
