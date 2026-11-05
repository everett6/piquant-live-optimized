// Package telemetry samples the health of the host the process runs on.
//
// It is read-only: every value comes from /proc, /sys or the standard library,
// nothing is written and no privileged call is made. The dashboard uses it to
// show CPU, memory, temperature and disk headroom on both Raspberry Pis; the
// research engine uses it to record the machine state a result was produced on,
// so a benchmark from the Pi 5 is never confused with one from the dev PC.
//
// # Portability
//
// The Linux collector reads text files. Anything that is missing on a given
// board (a Pi has no thermal zone on a dev PC, a PC has no under-voltage flag)
// reports as unavailable rather than as zero, so a chart cannot silently show
// "0 °C" for "not measured". Every field carries an explicit availability flag
// or a NaN sentinel; the JSON encoding drops unavailable values.
package telemetry

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Role classifies the machine, so the dashboard can label it and the setup
// flow can decide which side of the two-Pi topology it is looking at.
type Role string

// Machine roles.
const (
	RolePi5     Role = "pi5"     // Raspberry Pi 5: research engine
	RolePi4     Role = "pi4"     // Raspberry Pi 4: not a supported target, but detected
	RolePi3     Role = "pi3"     // Raspberry Pi 3B: dashboard server
	RolePiOther Role = "pi"      // any other Raspberry Pi board
	RoleDev     Role = "dev"     // a workstation, used for development
	RoleUnknown Role = "unknown" // model could not be read
)

// TempNaN marks a temperature that could not be read.
var TempNaN = math.NaN()

// Throttle decodes the Raspberry Pi firmware's under-voltage and thermal flags.
// On a non-Pi board it is the zero value and Available is false.
type Throttle struct {
	Available     bool   `json:"available"`
	UnderVoltage  bool   `json:"under_voltage"`
	FreqCapped    bool   `json:"freq_capped"`
	Throttled     bool   `json:"throttled"`
	SoftTempLimit bool   `json:"soft_temp_limit"`
	Raw           uint32 `json:"raw"`
}

// Disk is the free space of one filesystem the process writes to.
type Disk struct {
	Path        string  `json:"path"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// Host is the machine-level sample.
type Host struct {
	Hostname     string   `json:"hostname"`
	Model        string   `json:"model"`
	Role         Role     `json:"role"`
	OS           string   `json:"os"`
	Kernel       string   `json:"kernel"`
	Arch         string   `json:"arch"`
	CPUs         int      `json:"cpus"`
	MemTotal     uint64   `json:"mem_total_bytes"`
	MemAvailable uint64   `json:"mem_available_bytes"`
	SwapTotal    uint64   `json:"swap_total_bytes"`
	SwapFree     uint64   `json:"swap_free_bytes"`
	Load1        float64  `json:"load1"`
	Load5        float64  `json:"load5"`
	Load15       float64  `json:"load15"`
	CPUBusy      Float    `json:"cpu_busy"` // 0..1, null before the second sample
	UptimeSec    float64  `json:"uptime_seconds"`
	TempC        Float    `json:"temp_c"`
	TempOK       bool     `json:"temp_ok"`
	Throttle     Throttle `json:"throttle"`
	Disks        []Disk   `json:"disks,omitempty"`
}

// Proc is the process-level sample of the running PiQuant binary.
type Proc struct {
	PID        int     `json:"pid"`
	RSSBytes   uint64  `json:"rss_bytes"`
	HeapBytes  uint64  `json:"heap_bytes"`
	Goroutines int     `json:"goroutines"`
	GOMAXPROCS int     `json:"gomaxprocs"`
	NumGC      uint32  `json:"num_gc"`
	UptimeSec  float64 `json:"uptime_seconds"`
}

// Sample is one timestamped reading.
type Sample struct {
	At   time.Time `json:"at"`
	Host Host      `json:"host"`
	Proc Proc      `json:"proc"`
}

// Collector reads system state. A zero Collector is usable; files are read on
// every Collect call, so the collector holds no cache and no stale state.
//
// Root prefixes every filesystem path and exists so tests can point the
// collector at a fixture tree instead of the real /proc. In production it is
// empty.
type Collector struct {
	Root     string   // "" = the real filesystem
	DataDirs []string // paths whose free space is reported
	started  time.Time
	once     sync.Once

	mu       sync.Mutex
	prevCPU  cpuTimes
	prevAt   time.Time
	lastBusy float64 // 0..1, NaN before the first interval
}

type cpuTimes struct {
	idle, total uint64
}

// New returns a collector for the given data directories. An empty list falls
// back to the current working directory.
func New(dataDirs ...string) *Collector {
	return &Collector{DataDirs: dataDirs}
}

func (c *Collector) now() time.Time {
	c.once.Do(func() { c.started = time.Now() })
	return time.Now()
}

// path builds an absolute filesystem path. An empty Root means the real root:
// a relative join would read "proc/meminfo" from the working directory, which
// silently reports an empty host on the machine that has the data.
func (c *Collector) path(parts ...string) string {
	root := c.Root
	if root == "" {
		root = string(filepath.Separator)
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// Busy returns the fraction of CPU time spent non-idle since the previous
// Collect call, or NaN if this is the first call. It is sampled from
// /proc/stat, which is the only portable way to get a machine-wide figure; the
// process's own CPU use is not diluted by whatever else the board is doing.
// Collect refreshes it, so call Collect (or Sample) before reading Busy.
func (c *Collector) Busy() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastBusy
}

// Collect reads one sample.
func (c *Collector) Collect() Sample {
	now := c.now()
	c.updateBusy(now)
	s := Sample{At: now}
	s.Host = c.host(now)
	s.Proc = c.proc(now)
	return s
}

func (c *Collector) host(now time.Time) Host {
	h := Host{
		OS:     runtime.GOOS,
		Arch:   runtime.GOARCH,
		CPUs:   runtime.NumCPU(),
		TempC:  Float(TempNaN),
		Kernel: readTrim(c.path("proc", "sys", "kernel", "osrelease")),
	}
	h.Hostname, _ = os.Hostname()
	h.Model = readTrim(c.path("proc", "device-tree", "model"))
	if h.Model == "" {
		h.Model = readTrim(c.path("sys", "firmware", "devicetree", "model"))
	}
	h.Role = Classify(h.Model, h.Arch)

	if mem := readMemInfo(c.path("proc", "meminfo")); mem.total > 0 {
		h.MemTotal = mem.total
		h.MemAvailable = mem.available
		h.SwapTotal = mem.swapTotal
		h.SwapFree = mem.swapFree
	}
	if load, ok := readLoadAvg(c.path("proc", "loadavg")); ok {
		h.Load1, h.Load5, h.Load15 = load[0], load[1], load[2]
	}
	if up, ok := readUptime(c.path("proc", "uptime")); ok {
		h.UptimeSec = up
	}
	if t, ok := c.temperature(); ok {
		h.TempC, h.TempOK = Float(t), true
	}
	h.Throttle = c.throttle()
	h.CPUBusy = Float(c.Busy())
	dirs := c.DataDirs
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	for _, d := range dirs {
		if disk, ok := ReadDisk(d); ok {
			h.Disks = append(h.Disks, disk)
		}
	}
	return h
}

// temperature returns the SoC temperature in Celsius. Raspberry Pi boards
// expose it under thermal_zone0; a PC usually does not, and the value stays
// unavailable there rather than being reported as zero.
func (c *Collector) temperature() (float64, bool) {
	base := c.path("sys", "class", "thermal", "thermal_zone0")
	s := readTrim(filepath.Join(base, "temp"))
	if s == "" {
		// Some kernels expose the Pi sensor under a named zone.
		entries, err := os.ReadDir(c.path("sys", "class", "thermal"))
		if err != nil {
			return 0, false
		}
		for _, e := range entries {
			name := readTrim(c.path("sys", "class", "thermal", e.Name(), "type"))
			if name == "cpu-thermal" || name == "soc-thermal" {
				s = readTrim(c.path("sys", "class", "thermal", e.Name(), "temp"))
				break
			}
		}
	}
	milli, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(milli) / 1000, true
}

// throttle decodes the firmware flags, when the board has them.
func (c *Collector) throttle() Throttle {
	for _, p := range []string{
		filepath.Join("sys", "devices", "platform", "soc", "soc:firmware", "get_throttled"),
		filepath.Join("sys", "class", "hwmon", "hwmon0", "throttled"),
	} {
		s := readTrim(c.path(p))
		if s == "" {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
		if err != nil {
			continue
		}
		return DecodeThrottle(uint32(v))
	}
	return Throttle{}
}

// DecodeThrottle maps the firmware register to readable flags. Bits 0-3 are
// the "happened now" flags, bits 16-19 the "has happened since boot" ones.
func DecodeThrottle(raw uint32) Throttle {
	return Throttle{
		Available:     true,
		Raw:           raw,
		UnderVoltage:  raw&(1<<0|1<<16) != 0,
		FreqCapped:    raw&(1<<1|1<<17) != 0,
		Throttled:     raw&(1<<2|1<<18) != 0,
		SoftTempLimit: raw&(1<<3|1<<19) != 0,
	}
}

// Classify maps a device-tree model string to a role. An empty model is a
// non-Pi board; the architecture separates a 64-bit workstation from an unknown
// embedded target.
func Classify(model, arch string) Role {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "raspberry pi 5"):
		return RolePi5
	case strings.Contains(m, "raspberry pi 4"):
		return RolePi4
	case strings.Contains(m, "raspberry pi 3"), strings.Contains(m, "raspberry pi zero 2"):
		return RolePi3
	case strings.Contains(m, "raspberry pi"):
		return RolePiOther
	case model != "":
		return RoleDev // an SBC or VM that names itself in device-tree
	default:
		return RoleDev
	}
}

func (c *Collector) proc(now time.Time) Proc {
	p := Proc{
		PID:        os.Getpid(),
		Goroutines: runtime.NumGoroutine(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		UptimeSec:  now.Sub(c.started).Seconds(),
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p.RSSBytes = readRSS(c.path("proc", "self", "statm"))
	p.HeapBytes = ms.HeapAlloc
	p.NumGC = ms.NumGC
	return p
}

// readRSS returns the resident set size from statm, whose second field is the
// resident page count.
func readRSS(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

type memInfo struct{ total, available, swapTotal, swapFree uint64 }

func readMemInfo(path string) memInfo {
	var m memInfo
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), "kB")), 10, 64)
		if err != nil {
			continue
		}
		n *= 1024
		switch key {
		case "MemTotal":
			m.total = n
		case "MemAvailable":
			m.available = n
		case "SwapTotal":
			m.swapTotal = n
		case "SwapFree":
			m.swapFree = n
		}
	}
	return m
}

func readLoadAvg(path string) ([3]float64, bool) {
	var out [3]float64
	s := readTrim(path)
	if s == "" {
		return out, false
	}
	f := strings.Fields(s)
	if len(f) < 3 {
		return out, false
	}
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return out, false
		}
		out[i] = v
	}
	return out, true
}

func readUptime(path string) (float64, bool) {
	f := strings.Fields(readTrim(path))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
