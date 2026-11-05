package telemetry

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a file tree under t.TempDir and returns the root, so the
// collector can be exercised against a known /proc layout instead of whatever
// machine the test happens to run on.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const memInfoBody = `MemTotal:        8000000 kB
MemFree:         1000000 kB
MemAvailable:    5000000 kB
Buffers:          200000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
`

func TestCollectReadsARealPiLayout(t *testing.T) {
	root := fixture(t, map[string]string{
		"proc/sys/kernel/osrelease":            "6.6.31+rpt-rpi-2712",
		"proc/device-tree/model":               "Raspberry Pi 5 Model B Rev 1.0",
		"proc/meminfo":                         memInfoBody,
		"proc/loadavg":                         "0.42 0.51 0.60 2/310 8891\n",
		"proc/uptime":                          "12345.67 99999.00\n",
		"proc/self/statm":                      "1000 250 100 1 0 200 0\n",
		"sys/class/thermal/thermal_zone0/temp": "48500\n",
		"sys/devices/platform/soc/soc:firmware/get_throttled": "0x50005\n",
	})
	c := &Collector{Root: root}
	s := c.Collect()

	if s.Host.Role != RolePi5 {
		t.Fatalf("role = %q, want pi5", s.Host.Role)
	}
	if !strings.Contains(s.Host.Kernel, "6.6.31") {
		t.Fatalf("kernel = %q", s.Host.Kernel)
	}
	if s.Host.MemTotal != 8000000*1024 || s.Host.MemAvailable != 5000000*1024 {
		t.Fatalf("memory = %d/%d", s.Host.MemAvailable, s.Host.MemTotal)
	}
	if s.Host.SwapTotal != 2000000*1024 || s.Host.SwapFree != 1500000*1024 {
		t.Fatalf("swap = %d/%d", s.Host.SwapFree, s.Host.SwapTotal)
	}
	if s.Host.Load1 != 0.42 || s.Host.Load15 != 0.60 {
		t.Fatalf("load = %v/%v/%v", s.Host.Load1, s.Host.Load5, s.Host.Load15)
	}
	if s.Host.UptimeSec != 12345.67 {
		t.Fatalf("uptime = %v", s.Host.UptimeSec)
	}
	if !s.Host.TempOK || s.Host.TempC.Value() != 48.5 {
		t.Fatalf("temperature = %v (ok=%v), want 48.5", s.Host.TempC, s.Host.TempOK)
	}
	// 0x50005 = under-voltage now (bit 0) + under-voltage and throttled
	// since boot (bits 16, 18). Frequency capping never fired.
	th := s.Host.Throttle
	if !th.Available || !th.UnderVoltage || !th.Throttled || th.FreqCapped {
		t.Fatalf("throttle = %+v", th)
	}
	// statm field 2 is resident pages.
	wantRSS := uint64(250) * uint64(os.Getpagesize())
	if s.Proc.RSSBytes != wantRSS {
		t.Fatalf("rss = %d, want %d", s.Proc.RSSBytes, wantRSS)
	}
	if s.Proc.Goroutines <= 0 || s.Proc.GOMAXPROCS <= 0 || s.Proc.PID == 0 {
		t.Fatalf("proc = %+v", s.Proc)
	}
}

// A workstation must not be labelled as a Pi, and a missing sensor must read
// as unavailable rather than as 0 °C.
func TestMissingSensorsAreUnavailableNotZero(t *testing.T) {
	root := fixture(t, map[string]string{
		"proc/meminfo": "MemTotal: 1000 kB\n",
	})
	c := &Collector{Root: root}
	s := c.Collect()
	if s.Host.Role != RoleDev {
		t.Fatalf("role = %q, want dev", s.Host.Role)
	}
	if s.Host.TempOK || !math.IsNaN(s.Host.TempC.Value()) {
		t.Fatalf("temperature = %v (ok=%v), want NaN/unavailable", s.Host.TempC, s.Host.TempOK)
	}
	if s.Host.Throttle.Available {
		t.Fatalf("throttle reported available without a firmware file: %+v", s.Host.Throttle)
	}
	if s.Host.MemTotal != 1000*1024 {
		t.Fatalf("memtotal = %d", s.Host.MemTotal)
	}
	if s.Host.MemAvailable != 0 {
		t.Fatalf("memavailable = %d, want 0 when absent", s.Host.MemAvailable)
	}
}

func TestNamedThermalZoneIsFound(t *testing.T) {
	root := fixture(t, map[string]string{
		"sys/class/thermal/thermal_zone1/type": "soc-thermal\n",
		"sys/class/thermal/thermal_zone1/temp": "61250\n",
	})
	c := &Collector{Root: root}
	got, ok := c.temperature()
	if !ok || got != 61.25 {
		t.Fatalf("temperature = %v (ok=%v)", got, ok)
	}
}

func TestCPUBusyNeedsTwoSamples(t *testing.T) {
	root := fixture(t, map[string]string{
		"proc/stat": "cpu  100 0 100 800 0 0 0 0 0 0\n",
	})
	c := &Collector{Root: root}
	c.Collect()
	if v := c.Busy(); !math.IsNaN(v) {
		t.Fatalf("first sample busy = %v, want NaN", v)
	}
	// 200 further jiffies, 100 of them idle: half busy.
	if err := os.WriteFile(filepath.Join(root, "proc/stat"),
		[]byte("cpu  150 0 150 900 0 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Collect()
	if v := c.Busy(); math.Abs(v-0.5) > 1e-9 {
		t.Fatalf("busy = %v, want 0.5", v)
	}
}

func TestDecodeThrottleBits(t *testing.T) {
	for _, tc := range []struct {
		raw  uint32
		want Throttle
	}{
		{0x0, Throttle{Available: true}},
		{0x1, Throttle{Available: true, UnderVoltage: true, ActiveUnderVoltage: true}},
		{0x2, Throttle{Available: true, FreqCapped: true, ActiveFreqCapped: true}},
		{0x4, Throttle{Available: true, Throttled: true, ActiveThrottled: true}},
		{0x8, Throttle{Available: true, SoftTempLimit: true, ActiveSoftTempLimit: true}},
		{0x10000, Throttle{Available: true, UnderVoltage: true, HistoricalUnderVoltage: true}},
		{0x20000, Throttle{Available: true, FreqCapped: true, HistoricalFreqCapped: true}},
		{0x40000, Throttle{Available: true, Throttled: true, HistoricalThrottled: true}},
		{0x80000, Throttle{Available: true, SoftTempLimit: true, HistoricalSoftTempLimit: true}},
		{0x50005, Throttle{
			Available:              true,
			UnderVoltage:           true,
			Throttled:              true,
			ActiveUnderVoltage:     true,
			ActiveThrottled:        true,
			HistoricalUnderVoltage: true,
			HistoricalThrottled:    true,
		}},
	} {
		got := DecodeThrottle(tc.raw)
		got.Raw = tc.want.Raw
		if got != tc.want {
			t.Fatalf("DecodeThrottle(%#x) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestClassifyModels(t *testing.T) {
	for _, tc := range []struct {
		model, arch string
		want        Role
	}{
		{"Raspberry Pi 5 Model B Rev 1.0", "arm64", RolePi5},
		{"Raspberry Pi 4 Model B Rev 1.4", "arm64", RolePi4},
		{"Raspberry Pi 3 Model B Rev 1.2", "arm", RolePi3},
		{"Raspberry Pi Zero 2 W Rev 1.0", "arm", RolePi3},
		{"Raspberry Pi Compute Module 4", "arm64", RolePiOther},
		{"ASUS TUF Gaming", "amd64", RoleDev},
		{"", "amd64", RoleDev},
	} {
		if got := Classify(tc.model, tc.arch); got != tc.want {
			t.Fatalf("Classify(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestReadDisk(t *testing.T) {
	d, ok := ReadDisk(t.TempDir())
	if !ok {
		t.Fatal("ReadDisk failed on a temp dir")
	}
	if d.TotalBytes == 0 || d.FreeBytes > d.TotalBytes {
		t.Fatalf("disk = %+v", d)
	}
	if d.UsedPercent < 0 || d.UsedPercent > 100 {
		t.Fatalf("used = %v%%", d.UsedPercent)
	}
	if _, ok := ReadDisk(filepath.Join(t.TempDir(), "does-not-exist")); ok {
		t.Fatal("ReadDisk accepted a missing path")
	}
}

// The real collector must produce a usable sample on whatever machine runs the
// tests, with no privilege and no panic.
func TestCollectRealHost(t *testing.T) {
	s := New(".").Collect()
	if s.At.IsZero() || s.Host.OS != "linux" || s.Host.CPUs <= 0 {
		t.Fatalf("sample = %+v", s)
	}
	if len(s.Host.Disks) != 1 || s.Host.Disks[0].TotalBytes == 0 {
		t.Fatalf("disks = %+v", s.Host.Disks)
	}
}

func BenchmarkCollect(b *testing.B) {
	c := New(".")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Collect()
	}
}

// encoding/json fails a whole document on NaN, so a missing sensor must encode
// as null rather than blanking the dashboard status.
func TestStatusJSONEncodesUnavailableAsNull(t *testing.T) {
	root := fixture(t, map[string]string{"proc/meminfo": "MemTotal: 1000 kB\n"})
	s := New().Collect()
	s.Host = (&Collector{Root: root}).Collect().Host
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("a sample with an unavailable sensor did not encode: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	host := back["host"].(map[string]any)
	if host["temp_c"] != nil {
		t.Fatalf("temp_c = %v, want null", host["temp_c"])
	}
	if host["cpu_busy"] != nil {
		t.Fatalf("cpu_busy = %v, want null on the first sample", host["cpu_busy"])
	}
	// Round trip: a null reading comes back as NaN, not as zero.
	var again Sample
	if err := json.Unmarshal(body, &again); err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(again.Host.TempC.Value()) {
		t.Fatalf("round trip turned null into %v", again.Host.TempC.Value())
	}
}

// With no fixture root the collector must read the real filesystem, not a
// relative "proc/..." in the working directory.
func TestRealRootReadsSystemFiles(t *testing.T) {
	c := &Collector{}
	h := c.Collect().Host
	if h.MemTotal == 0 {
		t.Fatal("mem_total is 0: the collector did not read /proc/meminfo")
	}
	if h.Kernel == "" {
		t.Fatal("kernel is empty: the collector did not read /proc/sys/kernel/osrelease")
	}
	if h.CPUs == 0 {
		t.Fatal("cpu count is 0")
	}
}
