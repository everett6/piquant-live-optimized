package telemetry

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReadDisk reports the free space of the filesystem holding path. It returns
// false when the path does not exist, so a dashboard pointed at a data
// directory that has not been created yet shows "unknown" rather than 100%
// used.
//
// The conversions are explicit because syscall.Statfs_t field types differ
// between 64-bit and 32-bit Linux: on armv7 Blocks/Bavail/Bsize are int32,
// and an implicit conversion would either fail to compile or wrap.
func ReadDisk(path string) (Disk, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, false
	}
	bsize := int64(st.Bsize)
	blocks := int64(st.Blocks)
	bavail := int64(st.Bavail)
	if bsize <= 0 || blocks <= 0 {
		return Disk{}, false
	}
	total := blocks * bsize
	free := bavail * bsize
	if free < 0 {
		free = 0
	}
	d := Disk{Path: path, TotalBytes: uint64(total), FreeBytes: uint64(free)}
	d.UsedPercent = float64(total-free) / float64(total) * 100
	return d, true
}

// cpuBusy reads the aggregate "cpu" line of /proc/stat.
func (c *Collector) cpuBusy() (cpuTimes, bool) {
	b, err := os.ReadFile(c.path("proc", "stat"))
	if err != nil {
		return cpuTimes{}, false
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 4 {
			return cpuTimes{}, false
		}
		var t cpuTimes
		for i, s := range f {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return cpuTimes{}, false
			}
			t.total += v
			// Fields are user, nice, system, idle, iowait, irq, softirq,
			// steal, guest, guest_nice. Idle time is idle + iowait.
			if i == 3 || i == 4 {
				t.idle += v
			}
		}
		return t, true
	}
	return cpuTimes{}, false
}

// updateBusy converts two cumulative readings into the interval's busy
// fraction. The first reading has no predecessor, so it reports NaN and only
// seeds the baseline; that keeps "not measured yet" distinguishable from
// "measured as zero", which matters on a chart.
func (c *Collector) updateBusy(now time.Time) {
	cur, ok := c.cpuBusy()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !ok {
		c.lastBusy = math.NaN()
		c.prevAt = now
		return
	}
	prev, hasPrev := c.prevCPU, !c.prevAt.IsZero()
	c.prevCPU, c.prevAt = cur, now
	if !hasPrev || cur.total <= prev.total {
		c.lastBusy = math.NaN()
		return
	}
	dTotal := float64(cur.total - prev.total)
	dIdle := float64(cur.idle - prev.idle)
	if dIdle < 0 {
		dIdle = 0
	}
	c.lastBusy = (dTotal - dIdle) / dTotal
}
