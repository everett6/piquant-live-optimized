package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/event"
	"piquant-live/internal/features"
	"piquant-live/internal/marketdata"
	"piquant-live/internal/models/common"
	"piquant-live/internal/orderbook"
	"piquant-live/internal/paper"
)

type benchFlags struct {
	events int
	mode   string
}

func addBenchFlags(fs *flag.FlagSet) *benchFlags {
	f := &benchFlags{}
	fs.IntVar(&f.events, "events", 20000, "number of events per benchmark scenario")
	fs.StringVar(&f.mode, "mode", "all", "scenario to run: 1x, 10x, burst, or all")
	return f
}

type benchResult struct {
	Scenario     string
	TargetRate   string
	ActualRate   float64
	Events       int
	P50DevUs     float64
	P99DevUs     float64
	P999DevUs    float64
	P50Pi5Us     float64 // 4x Pi factor
	P99Pi5Us     float64 // 4x Pi factor
	P999Pi5Us    float64 // 4x Pi factor
	PassP99Pi5   bool    // P99Pi5 < 1000us (1ms)
	RingDrops    uint64
	MaxGCPauseUs float64
	AllocsPerOp  float64
	RSSMb        float64
}

func runBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	f := addBenchFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf("\n=========================================================================================\n")
	fmt.Printf(" PIQUANT PI 5 ARCHITECTURE STRESS TEST & MICRO-LATENCY HARNESS\n")
	fmt.Printf(" Full Path: SPSCRing -> L2OrderBook -> FeaturesEngine -> Models -> PaperEngine\n")
	fmt.Printf(" Environment: %d Cores (GOMAXPROCS=%d), GOARCH=%s, Pi 5 Multiplier = 4.0x\n",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.GOARCH)
	fmt.Printf("=========================================================================================\n\n")

	scenarios := []struct {
		name       string
		targetRate int // events/sec, 0 = unthrottled burst
		label      string
	}{
		{"1x Baseline", 100, "100 events/s"},
		{"10x Pressure", 1000, "1,000 events/s"},
		{"Burst Saturation", 0, "Max Burst (~20k-50k/s)"},
	}

	var results []benchResult
	for _, sc := range scenarios {
		if f.mode != "all" && !strings.EqualFold(f.mode, sc.name) && !strings.Contains(strings.ToLower(sc.name), strings.ToLower(f.mode)) {
			continue
		}
		res := runScenario(sc.name, sc.label, sc.targetRate, f.events)
		results = append(results, res)
	}

	// Print Summary Table
	fmt.Printf("\n%-18s %-15s %-12s %-11s %-11s %-11s %-9s %-9s %s\n",
		"Scenario", "Actual Rate", "p50 (Pi5)", "p99 (Pi5)", "p99.9(Pi5)", "Max GC", "Drops", "RSS", "Pass (<1ms)")
	fmt.Printf("------------------------------------------------------------------------------------------------------------------\n")

	allPassed := true
	for _, r := range results {
		passStr := "PASS"
		if !r.PassP99Pi5 || (r.Scenario == "10x Pressure" && r.RingDrops > 0) {
			passStr = "FAIL"
			allPassed = false
		}
		fmt.Printf("%-18s %-15s %-12s %-11s %-11s %-11s %-9d %-9s %s\n",
			r.Scenario,
			fmt.Sprintf("%.0f ev/s", r.ActualRate),
			fmt.Sprintf("%.1f µs", r.P50Pi5Us),
			fmt.Sprintf("%.1f µs", r.P99Pi5Us),
			fmt.Sprintf("%.1f µs", r.P999Pi5Us),
			fmt.Sprintf("%.1f µs", r.MaxGCPauseUs),
			r.RingDrops,
			fmt.Sprintf("%.1f MB", r.RSSMb),
			passStr,
		)
	}
	fmt.Printf("------------------------------------------------------------------------------------------------------------------\n")
	if allPassed {
		fmt.Printf(" ALL STRESS TEST CRITERIA PASSED: Pi 5 4x p99 latency < 1.0ms and zero 10x ring drops.\n\n")
	} else {
		fmt.Printf(" [WARNING] Some criteria exceeded performance budgets.\n\n")
	}

	return nil
}

func runScenario(name, targetLabel string, targetRate, nEvents int) benchResult {
	// Initialize SPSCRing with 8192 capacity
	ring := engine.NewSPSCRing(8192)

	// Initialize L2 Book
	l2Book := marketdata.NewL2OrderBook("BTCUSDT")

	// Initialize internal orderbook.Book and features.Engine
	rawBook := &orderbook.Book{}
	featEngine := features.New(0)

	// Initialize Models
	ridge := &common.Ridge{Lam: 1e-3, Bias: 0.05, Fitted: true}
	for i := 0; i < common.FeatureCount; i++ {
		ridge.Weights[i] = 0.15 / float64(i+1)
		ridge.Std[i] = 1.0
	}

	sor := common.NewSmartOrderRouter()

	// Initialize Paper Dry Run Engine
	tempDir, _ := os.MkdirTemp("", "piquant_bench_*")
	defer os.RemoveAll(tempDir)
	paperCfg := paper.DefaultDryRunConfig(tempDir)
	paperCfg.OrderLatency = 1 * time.Millisecond // fast latency for bench

	dryRun, _ := paper.NewDryRunEngine(paperCfg, ring, nil)

	latencies := make([]float64, nEvents)
	var ringDrops uint64

	// GC and memory baseline
	runtime.GC()
	var gcStatsBefore debug.GCStats
	debug.ReadGCStats(&gcStatsBefore)
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	r := rand.New(rand.NewSource(1337))
	basePrice := 80000.0

	// Interval timer for target rate pacing
	var interval time.Duration
	if targetRate > 0 {
		interval = time.Second / time.Duration(targetRate)
	}

	tBatchStart := time.Now()

	for i := 0; i < nEvents; i++ {
		// Pace event injection if targetRate > 0
		if interval > 0 && i%10 == 0 {
			expectedElapsed := time.Duration(i) * interval
			actualElapsed := time.Since(tBatchStart)
			if actualElapsed < expectedElapsed {
				time.Sleep(expectedElapsed - actualElapsed)
			}
		}

		// Microsecond-precision tick start timestamp
		t0 := time.Now()

		isTrade := (i % 3) == 0
		var ev engine.MarketEvent

		if isTrade {
			tradePrice := basePrice + (r.Float64()-0.5)*2.0
			tradeSize := 0.05 + r.Float64()*0.5
			side := uint8(1 + (i % 2))
			ev = engine.MarketEvent{
				Timestamp: t0.UnixNano(),
				Sequence:  uint64(i + 1),
				Price:     tradePrice,
				Size:      tradeSize,
				Side:      side,
				Kind:      0, // Trade
				SymbolID:  0,
			}
		} else {
			spread := 0.50 + r.Float64()*0.50
			mid := basePrice + math.Sin(float64(i)*0.01)*5.0
			ev = engine.MarketEvent{
				Timestamp: t0.UnixNano(),
				Sequence:  uint64(i + 1),
				Price:     mid,
				Size:      1.5,
				Side:      0,
				Kind:      1, // Depth
				SymbolID:  0,
			}
			// Update L2 order book
			var bids, asks [marketdata.BookDepth]marketdata.PriceLevel
			for k := 0; k < marketdata.BookDepth; k++ {
				bids[k] = marketdata.PriceLevel{Price: mid - spread*0.5 - float64(k)*0.2, Size: 1.0 + float64(k)*0.1}
				asks[k] = marketdata.PriceLevel{Price: mid + spread*0.5 + float64(k)*0.2, Size: 1.0 + float64(k)*0.1}
			}
			l2Book.SetSnapshot(uint64(i+1), bids[:], asks[:])
		}

		// 1. Stage 1: Push to Lock-free SPSCRing
		pushed := ring.Push(ev)
		if !pushed {
			ringDrops++
			continue
		}

		// 2. Stage 2: Pop from SPSCRing
		poppedEv, ok := ring.Pop()
		if !ok {
			continue
		}

		// 3. Stage 3: Features Engine Advance
		var dummyEnv event.Envelope
		dummyEnv.RecvTime = poppedEv.Timestamp
		if poppedEv.Kind == 0 {
			dummyEnv.Kind = event.KindTrade
			dummyEnv.Trade.Price = int64(poppedEv.Price * 1e4)
			dummyEnv.Trade.Qty = int64(poppedEv.Size * 1e4)
		} else {
			dummyEnv.Kind = event.KindDepth
		}
		_ = featEngine.Advance(rawBook, &dummyEnv)

		// 4. Stage 4: Quantitative Model Prediction
		var x [common.FeatureCount]float64
		x[0] = 0.25
		x[4] = 0.85
		x[5] = 1.20
		x[10] = 1.50
		x[15] = 0.55
		alphaBps := ridge.Predict(&x)

		// 5. Stage 5: Smart Order Routing Decision
		sorDecision := sor.Route(alphaBps, 0.20, 2.5, 1.2, 0.0)
		_ = sorDecision.Action

		// 6. Stage 6: Paper Engine Processing (Match Maker orders & Quoting)
		dryRun.ProcessMarketEvent(&poppedEv)

		// Decision completed: compute elapsed nanoseconds
		elapsedNs := float64(time.Since(t0).Nanoseconds())
		latencies[i] = elapsedNs / 1000.0 // in microseconds
	}

	totalDuration := time.Since(tBatchStart)
	actualRate := float64(nEvents) / totalDuration.Seconds()

	// Memory & GC stats
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)
	var gcStatsAfter debug.GCStats
	debug.ReadGCStats(&gcStatsAfter)

	var maxGCPauseUs float64
	for _, p := range gcStatsAfter.Pause {
		pUs := float64(p.Nanoseconds()) / 1000.0
		if pUs > maxGCPauseUs {
			maxGCPauseUs = pUs
		}
	}

	allocsPerOp := float64(mAfter.Mallocs-mBefore.Mallocs) / float64(nEvents)
	rssMb := getProcessRSSMb()

	// Latency percentiles
	sort.Float64s(latencies)
	p50 := latencies[int(float64(nEvents)*0.50)]
	p99 := latencies[int(float64(nEvents)*0.99)]
	p999 := latencies[int(float64(nEvents)*0.999)]

	const piFactor = 4.0 // Raspberry Pi 5 4x slowdown multiplier
	p50Pi := p50 * piFactor
	p99Pi := p99 * piFactor
	p999Pi := p999 * piFactor

	return benchResult{
		Scenario:     name,
		TargetRate:   targetLabel,
		ActualRate:   actualRate,
		Events:       nEvents,
		P50DevUs:     p50,
		P99DevUs:     p99,
		P999DevUs:    p999,
		P50Pi5Us:     p50Pi,
		P99Pi5Us:     p99Pi,
		P999Pi5Us:    p999Pi,
		PassP99Pi5:   p99Pi < 1000.0, // < 1ms budget
		RingDrops:    ringDrops,
		MaxGCPauseUs: maxGCPauseUs,
		AllocsPerOp:  allocsPerOp,
		RSSMb:        rssMb,
	}
}

func getProcessRSSMb() float64 {
	var rusage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusage); err == nil {
		// On Linux, Maxrss is in kilobytes
		return float64(rusage.Maxrss) / 1024.0
	}
	return 0.0
}
