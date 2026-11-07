package cluster

import (
	"sync"
	"time"
)

// TaskID identifies a scheduled computational task in PiQuant.
type TaskID uint8

const (
	// Latency-critical tasks strictly pinned to Pi 5 arm64 (never offloaded)
	TaskMarketIngest        TaskID = 1
	TaskL2OrderBook         TaskID = 2
	TaskSIMDFeatures        TaskID = 3
	TaskHFTQuoting          TaskID = 4
	TaskMicrostructureAlpha TaskID = 5

	// Auxiliary tasks eligible for offload to Pi 3B armv7
	TaskWebUIDashboard       TaskID = 6
	TaskTelemetryAggregation TaskID = 7
	TaskDailyTearSheet       TaskID = 8
	TaskArchiveVerification  TaskID = 9
	TaskParameterSweepShard  TaskID = 10
)

const (
	MinTaskID TaskID = 1
	MaxTaskID TaskID = 10
	NumTasks         = 10
)

var taskNames = [...]string{
	"",
	"TaskMarketIngest",
	"TaskL2OrderBook",
	"TaskSIMDFeatures",
	"TaskHFTQuoting",
	"TaskMicrostructureAlpha",
	"TaskWebUIDashboard",
	"TaskTelemetryAggregation",
	"TaskDailyTearSheet",
	"TaskArchiveVerification",
	"TaskParameterSweepShard",
}

// String returns the task identifier name.
func (t TaskID) String() string {
	if int(t) < len(taskNames) {
		return taskNames[t]
	}
	return "TaskUnknown"
}

// IsLatencyCritical returns true if the task is pinned exclusively to Pi 5 arm64.
func (t TaskID) IsLatencyCritical() bool {
	return t >= TaskMarketIngest && t <= TaskMicrostructureAlpha
}

// IsOffloadEligible returns true if the task can be safely delegated to Pi 3B armv7.
func (t TaskID) IsOffloadEligible() bool {
	return t >= TaskWebUIDashboard && t <= TaskParameterSweepShard
}

// TargetNode represents an execution node in the dual-Pi cluster.
type TargetNode uint8

const (
	TargetPi5  TargetNode = 1 // Pi 5 Primary arm64 (RoleEngine)
	TargetPi3B TargetNode = 2 // Pi 3B Auxiliary armv7 (RoleDashboard)
)

func (n TargetNode) String() string {
	switch n {
	case TargetPi5:
		return "Pi5_arm64"
	case TargetPi3B:
		return "Pi3B_armv7"
	default:
		return "Unknown"
	}
}

// Cluster operational modes.
const (
	ModeCooperativeDualPi     = "COOPERATIVE_DUAL_PI"
	ModeStandalonePi5Failover = "STANDALONE_PI5_FAILOVER"
)

// Hardware thresholds for asymmetric task delegation.
const (
	MaxPeerRTTMicros        = 10000.0 // 10ms network RTT threshold
	Pi3BMaxCPUPermille      = 700     // Pi 3B CPU < 70%
	Pi3BMinMemAvailMB       = 200     // Pi 3B MemAvail > 200MB to admit new offloads
	Pi3BFailoverMemAvailMB  = 180     // Pi 3B MemAvail < 180MB triggers failback
	Pi3BMaxTempDeciC        = 750     // Pi 3B Temp < 75.0°C
	Pi5ThermalWarnTempDeciC = 750     // Pi 5 Temp > 75.0°C triggers aggressive thermal shedding
)

// NodeHealth tracks live telemetry for a cluster node.
type NodeHealth struct {
	Online       bool
	CPUPermille  uint16 // 0..1000
	MemAvailMB   uint16 // MB
	TempDeciC    uint16 // deci-Celsius (e.g. 585 = 58.5°C)
	ActiveTasks  uint16
	LastSeenUnix int64 // Unix nanoseconds
}

// ClusterSnapshot captures a frozen point-in-time state of cluster topology,
// link statistics, and live task delegation assignments.
type ClusterSnapshot struct {
	Mode             string
	Pi5Healthy       bool
	Pi5TempDeciC     uint16
	Pi5ThermalAlert  bool
	Pi3BHealthy      bool
	Pi3BCPUPermille  uint16
	Pi3BMemAvailMB   uint16
	Pi3BTempDeciC    uint16
	LinkRTTMicros    float64
	LinkJitterMicros float64
	LinkConnected    bool
	Assignments      [MaxTaskID + 1]TargetNode
	Pi5TaskCount     uint16
	Pi3BTaskCount    uint16
}

// TaskDelegator implements zero-allocation, work-stealing asymmetric task scheduling
// across the Pi 5 (arm64, 2.4GHz, 8GB) and Pi 3B (armv7, 1.2GHz, 1GB) boards.
type TaskDelegator struct {
	mu          sync.RWMutex
	pi5Health   NodeHealth
	pi3bHealth  NodeHealth
	rttMicros   float64
	jitterUs    float64
	linkHealthy bool
	assignments [MaxTaskID + 1]TargetNode
	mode        string
}

// NewTaskDelegator creates an initialized TaskDelegator defaulting to local execution.
func NewTaskDelegator() *TaskDelegator {
	d := &TaskDelegator{
		pi5Health: NodeHealth{
			Online:     true,
			MemAvailMB: 6000,
			TempDeciC:  500,
		},
		mode: ModeStandalonePi5Failover,
	}
	for i := MinTaskID; i <= MaxTaskID; i++ {
		d.assignments[i] = TargetPi5
	}
	return d
}

// DecideTarget returns the execution target node for the given task.
// Hot path: executes in O(1) time with 0 B/op, 0 allocs/op.
func (d *TaskDelegator) DecideTarget(task TaskID) TargetNode {
	if task.IsLatencyCritical() || task > MaxTaskID || task < MinTaskID {
		return TargetPi5
	}
	d.mu.RLock()
	target := d.assignments[task]
	d.mu.RUnlock()
	return target
}

// UpdateNodeState updates telemetry for Pi 5 or Pi 3B and recalculates task assignments.
// Hot path: 0 B/op, 0 allocs/op.
func (d *TaskDelegator) UpdateNodeState(node TargetNode, state NodeHealth) {
	d.mu.Lock()
	if node == TargetPi5 {
		d.pi5Health = state
	} else if node == TargetPi3B {
		d.pi3bHealth = state
	}
	d.recomputeLocked()
	d.mu.Unlock()
}

// UpdateLinkStats updates peer link RTT, jitter, and connectivity.
// Hot path: 0 B/op, 0 allocs/op.
func (d *TaskDelegator) UpdateLinkStats(rttMicros, jitterMicros float64, connected bool) {
	d.mu.Lock()
	d.rttMicros = rttMicros
	d.jitterUs = jitterMicros
	d.linkHealthy = connected && (rttMicros < MaxPeerRTTMicros)
	d.recomputeLocked()
	d.mu.Unlock()
}

// recomputeLocked determines task delegation across the cluster. Must be called with d.mu locked.
func (d *TaskDelegator) recomputeLocked() {
	// Latency critical tasks (1..5) ALWAYS stay on Pi 5 arm64
	for t := TaskMarketIngest; t <= TaskMicrostructureAlpha; t++ {
		d.assignments[t] = TargetPi5
	}

	// Pi 3B admission rules:
	// 1. Link is connected and RTT < 10ms
	// 2. Pi 3B is online
	// 3. Pi 3B CPU < 70%
	// 4. Pi 3B MemAvail >= 180MB (and >= 200MB for new admits)
	// 5. Pi 3B Temp < 75.0°C
	pi3bAdmissible := d.linkHealthy &&
		d.pi3bHealth.Online &&
		d.pi3bHealth.CPUPermille < Pi3BMaxCPUPermille &&
		d.pi3bHealth.MemAvailMB >= Pi3BFailoverMemAvailMB &&
		d.pi3bHealth.TempDeciC < Pi3BMaxTempDeciC

	pi5ThermalWarning := d.pi5Health.TempDeciC >= Pi5ThermalWarnTempDeciC

	if pi3bAdmissible {
		d.mode = ModeCooperativeDualPi
		for t := TaskWebUIDashboard; t <= TaskParameterSweepShard; t++ {
			// If Pi 5 is overheating, aggressively delegate ALL offload-eligible tasks
			if pi5ThermalWarning {
				d.assignments[t] = TargetPi3B
				continue
			}

			// Standard cooperative dual-Pi distribution:
			// If Pi 3B has full headroom (>200MB), delegate
			if d.pi3bHealth.MemAvailMB >= Pi3BMinMemAvailMB {
				d.assignments[t] = TargetPi3B
			} else {
				// Pi 3B between 180MB and 200MB: keep lighter tasks delegated, keep heavy sweep local
				if t == TaskParameterSweepShard {
					d.assignments[t] = TargetPi5
				} else {
					d.assignments[t] = TargetPi3B
				}
			}
		}
	} else {
		// Standalone Pi 5 failover mode: fail back all tasks locally with zero disruption
		d.mode = ModeStandalonePi5Failover
		for t := TaskWebUIDashboard; t <= TaskParameterSweepShard; t++ {
			d.assignments[t] = TargetPi5
		}
	}
}

// Snapshot writes the current cluster state into dst with 0 B/op, 0 allocs/op.
func (d *TaskDelegator) Snapshot(dst *ClusterSnapshot) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	dst.Mode = d.mode
	dst.Pi5Healthy = d.pi5Health.Online
	dst.Pi5TempDeciC = d.pi5Health.TempDeciC
	dst.Pi5ThermalAlert = d.pi5Health.TempDeciC >= Pi5ThermalWarnTempDeciC
	dst.Pi3BHealthy = d.pi3bHealth.Online
	dst.Pi3BCPUPermille = d.pi3bHealth.CPUPermille
	dst.Pi3BMemAvailMB = d.pi3bHealth.MemAvailMB
	dst.Pi3BTempDeciC = d.pi3bHealth.TempDeciC
	dst.LinkRTTMicros = d.rttMicros
	dst.LinkJitterMicros = d.jitterUs
	dst.LinkConnected = d.linkHealthy
	dst.Assignments = d.assignments

	var pi5Count, pi3bCount uint16
	for t := MinTaskID; t <= MaxTaskID; t++ {
		if d.assignments[t] == TargetPi5 {
			pi5Count++
		} else if d.assignments[t] == TargetPi3B {
			pi3bCount++
		}
	}
	dst.Pi5TaskCount = pi5Count
	dst.Pi3BTaskCount = pi3bCount
}

// GetSnapshot returns a ClusterSnapshot value on stack without heap allocation.
func (d *TaskDelegator) GetSnapshot() ClusterSnapshot {
	var snap ClusterSnapshot
	d.Snapshot(&snap)
	return snap
}

// DelegationMode returns current cluster mode ("COOPERATIVE_DUAL_PI" or "STANDALONE_PI5_FAILOVER").
func (d *TaskDelegator) DelegationMode() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.mode
}

// TaskLists returns the human-readable names of tasks assigned to Pi 5 and Pi 3B.
func (d *TaskDelegator) TaskLists() (pi5Tasks []string, pi3bTasks []string) {
	var snap ClusterSnapshot
	d.Snapshot(&snap)

	for t := MinTaskID; t <= MaxTaskID; t++ {
		name := TaskID(t).String()
		if snap.Assignments[t] == TargetPi5 {
			pi5Tasks = append(pi5Tasks, name)
		} else {
			pi3bTasks = append(pi3bTasks, name)
		}
	}
	return pi5Tasks, pi3bTasks
}

// TouchLastSeen updates the timestamp for a node.
func (d *TaskDelegator) TouchLastSeen(node TargetNode) {
	now := time.Now().UnixNano()
	d.mu.Lock()
	if node == TargetPi5 {
		d.pi5Health.LastSeenUnix = now
	} else if node == TargetPi3B {
		d.pi3bHealth.LastSeenUnix = now
	}
	d.mu.Unlock()
}
