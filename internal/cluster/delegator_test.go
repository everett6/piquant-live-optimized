package cluster

import (
	"testing"
)

func TestDelegatorInitialState(t *testing.T) {
	d := NewTaskDelegator()

	// Initial state before peer connection: all tasks on Pi 5
	for task := MinTaskID; task <= MaxTaskID; task++ {
		target := d.DecideTarget(task)
		if target != TargetPi5 {
			t.Fatalf("task %s expected TargetPi5, got %s", task, target)
		}
	}

	if d.DelegationMode() != ModeStandalonePi5Failover {
		t.Fatalf("expected mode %s, got %s", ModeStandalonePi5Failover, d.DelegationMode())
	}
}

func TestDelegatorCooperativeDelegation(t *testing.T) {
	d := NewTaskDelegator()

	// Configure healthy Pi 5
	d.UpdateNodeState(TargetPi5, NodeHealth{
		Online:      true,
		CPUPermille: 350,
		MemAvailMB:  6500,
		TempDeciC:   520, // 52.0°C
	})

	// Configure healthy Pi 3B
	d.UpdateNodeState(TargetPi3B, NodeHealth{
		Online:      true,
		CPUPermille: 300, // 30%
		MemAvailMB:  450, // 450 MB > 200 MB
		TempDeciC:   550, // 55.0°C < 75.0°C
	})

	// Healthy low-latency link: 1.2ms (1200 µs)
	d.UpdateLinkStats(1200.0, 45.0, true)

	if d.DelegationMode() != ModeCooperativeDualPi {
		t.Fatalf("expected %s, got %s", ModeCooperativeDualPi, d.DelegationMode())
	}

	// 1. Latency-critical tasks MUST remain on Pi 5
	for task := TaskMarketIngest; task <= TaskMicrostructureAlpha; task++ {
		if target := d.DecideTarget(task); target != TargetPi5 {
			t.Fatalf("latency-critical task %s must be TargetPi5, got %s", task, target)
		}
	}

	// 2. Auxiliary tasks MUST be delegated to Pi 3B
	for task := TaskWebUIDashboard; task <= TaskParameterSweepShard; task++ {
		if target := d.DecideTarget(task); target != TargetPi3B {
			t.Fatalf("auxiliary task %s should be delegated to TargetPi3B, got %s", task, target)
		}
	}

	var snap ClusterSnapshot
	d.Snapshot(&snap)
	if snap.Pi5TaskCount != 5 || snap.Pi3BTaskCount != 5 {
		t.Fatalf("expected 5/5 task split, got Pi5=%d, Pi3B=%d", snap.Pi5TaskCount, snap.Pi3BTaskCount)
	}
}

func TestDelegatorFailoverOnMemoryPressure(t *testing.T) {
	d := NewTaskDelegator()

	// Healthy initial cluster
	d.UpdateNodeState(TargetPi3B, NodeHealth{
		Online:      true,
		CPUPermille: 300,
		MemAvailMB:  300,
		TempDeciC:   550,
	})
	d.UpdateLinkStats(1500.0, 50.0, true)

	if d.DecideTarget(TaskWebUIDashboard) != TargetPi3B {
		t.Fatal("expected TaskWebUIDashboard on Pi 3B")
	}

	// Memory drops below 180MB threshold (e.g. 150MB) -> dynamic failover
	d.UpdateNodeState(TargetPi3B, NodeHealth{
		Online:      true,
		CPUPermille: 400,
		MemAvailMB:  150,
		TempDeciC:   560,
	})

	if d.DelegationMode() != ModeStandalonePi5Failover {
		t.Fatalf("expected failover mode, got %s", d.DelegationMode())
	}

	for task := MinTaskID; task <= MaxTaskID; task++ {
		if target := d.DecideTarget(task); target != TargetPi5 {
			t.Fatalf("task %s failed to failover to Pi 5, got %s", task, target)
		}
	}
}

func TestDelegatorFailoverOnLinkDegradationOrDisconnect(t *testing.T) {
	d := NewTaskDelegator()

	d.UpdateNodeState(TargetPi3B, NodeHealth{
		Online:      true,
		CPUPermille: 250,
		MemAvailMB:  500,
		TempDeciC:   500,
	})
	d.UpdateLinkStats(800.0, 30.0, true)

	if d.DecideTarget(TaskWebUIDashboard) != TargetPi3B {
		t.Fatal("expected Pi 3B target")
	}

	// High RTT > 10ms (12.5ms = 12500 µs)
	d.UpdateLinkStats(12500.0, 500.0, true)
	if d.DecideTarget(TaskWebUIDashboard) != TargetPi5 {
		t.Fatal("expected failover to Pi 5 when link RTT > 10ms")
	}

	// Link disconnected
	d.UpdateLinkStats(1000.0, 30.0, false)
	if d.DecideTarget(TaskWebUIDashboard) != TargetPi5 {
		t.Fatal("expected failover to Pi 5 when link disconnected")
	}
}

func TestDelegatorThermalShedding(t *testing.T) {
	d := NewTaskDelegator()

	// Pi 3B is healthy with medium memory
	d.UpdateNodeState(TargetPi3B, NodeHealth{
		Online:      true,
		CPUPermille: 200,
		MemAvailMB:  190, // Between 180MB and 200MB: normally parameter sweep is kept local
		TempDeciC:   500,
	})
	d.UpdateLinkStats(1000.0, 20.0, true)

	// Under normal temp, heavy sweep is on Pi 5
	if d.DecideTarget(TaskParameterSweepShard) != TargetPi5 {
		t.Fatal("expected TaskParameterSweepShard on Pi 5 when Pi 3B memory is 190MB")
	}

	// Pi 5 enters thermal warning: 78.5°C (785 deci-C)
	d.UpdateNodeState(TargetPi5, NodeHealth{
		Online:     true,
		MemAvailMB: 6000,
		TempDeciC:  785,
	})

	// Should aggressively offload ALL auxiliary tasks, including sweep shard!
	if d.DecideTarget(TaskParameterSweepShard) != TargetPi3B {
		t.Fatal("expected TaskParameterSweepShard offloaded to Pi 3B under thermal shedding")
	}

	var snap ClusterSnapshot
	d.Snapshot(&snap)
	if !snap.Pi5ThermalAlert {
		t.Fatal("expected Pi5ThermalAlert = true")
	}
}

func BenchmarkDecideTarget(b *testing.B) {
	d := NewTaskDelegator()
	d.UpdateNodeState(TargetPi3B, NodeHealth{
		Online:      true,
		CPUPermille: 300,
		MemAvailMB:  500,
		TempDeciC:   500,
	})
	d.UpdateLinkStats(1000.0, 20.0, true)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = d.DecideTarget(TaskSIMDFeatures)
		_ = d.DecideTarget(TaskWebUIDashboard)
	}
}

func BenchmarkUpdateNodeState(b *testing.B) {
	d := NewTaskDelegator()
	health := NodeHealth{
		Online:      true,
		CPUPermille: 350,
		MemAvailMB:  450,
		TempDeciC:   520,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.UpdateNodeState(TargetPi3B, health)
	}
}

func BenchmarkSnapshot(b *testing.B) {
	d := NewTaskDelegator()
	var snap ClusterSnapshot

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Snapshot(&snap)
	}
}
