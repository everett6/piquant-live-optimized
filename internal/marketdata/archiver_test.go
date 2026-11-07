package marketdata

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"piquant-live/internal/engine"
)

func TestMarketEvent_BinaryRecordLayout184(t *testing.T) {
	ev := engine.MarketEvent{
		Timestamp: 1_700_000_000_123_456_789,
		Sequence:  9876543210,
		Price:     50123.45,
		Size:      1.234,
		Side:      1,
		Kind:      0,
		SymbolID:  42,
		Flags:     7,
	}

	buf := make([]byte, PQBinRecordSize)
	EncodeMarketEvent(&ev, buf)
	if len(buf) != 184 {
		t.Fatalf("expected 184 bytes, got %d", len(buf))
	}

	decoded := DecodeMarketEvent(buf)
	if decoded.Timestamp != ev.Timestamp {
		t.Errorf("timestamp mismatch: got %d, want %d", decoded.Timestamp, ev.Timestamp)
	}
	if decoded.Sequence != ev.Sequence {
		t.Errorf("sequence mismatch: got %d, want %d", decoded.Sequence, ev.Sequence)
	}
	if decoded.Price != ev.Price {
		t.Errorf("price mismatch: got %f, want %f", decoded.Price, ev.Price)
	}
	if decoded.Size != ev.Size {
		t.Errorf("size mismatch: got %f, want %f", decoded.Size, ev.Size)
	}
	if decoded.Side != ev.Side {
		t.Errorf("side mismatch: got %d, want %d", decoded.Side, ev.Side)
	}
	if decoded.Kind != ev.Kind {
		t.Errorf("kind mismatch: got %d, want %d", decoded.Kind, ev.Kind)
	}
	if decoded.SymbolID != ev.SymbolID {
		t.Errorf("symbolID mismatch: got %d, want %d", decoded.SymbolID, ev.SymbolID)
	}
	if decoded.Flags != ev.Flags {
		t.Errorf("flags mismatch: got %d, want %d", decoded.Flags, ev.Flags)
	}
}

func TestRollingArchiver_HourlyRotationAndReadback(t *testing.T) {
	dir := t.TempDir()

	cfg := ArchiverConfig{
		ArchiveDir:        dir,
		MaxRetentionBytes: 10 * 1024 * 1024,
		MaxRetentionHours: 168,
		BufferCapacity:    4, // small buffer for testing flushes
	}

	archiver, err := NewRollingArchiver(cfg)
	if err != nil {
		t.Fatalf("NewRollingArchiver failed: %v", err)
	}
	defer archiver.Close()

	// Hour 1: 2026-11-05 10:00:00 UTC
	t1, _ := time.Parse("2006-01-02 15:04:05", "2026-11-05 10:15:00")
	t1Ns := t1.UnixNano()
	for i := 0; i < 5; i++ {
		ev := engine.MarketEvent{
			Timestamp: t1Ns + int64(i)*1_000_000,
			Sequence:  uint64(i + 1),
			Price:     50000.0 + float64(i),
			Size:      1.0,
			Side:      1,
			Kind:      0,
		}
		if err := archiver.Ingest(ev); err != nil {
			t.Fatalf("Ingest failed at hour 1, event %d: %v", i, err)
		}
	}

	// Hour 2: 2026-11-05 11:00:00 UTC (triggers hourly partition rotation)
	t2, _ := time.Parse("2006-01-02 15:04:05", "2026-11-05 11:10:00")
	t2Ns := t2.UnixNano()
	for i := 0; i < 3; i++ {
		ev := engine.MarketEvent{
			Timestamp: t2Ns + int64(i)*1_000_000,
			Sequence:  uint64(10 + i),
			Price:     51000.0 + float64(i),
			Size:      2.0,
			Side:      2,
			Kind:      1,
		}
		if err := archiver.Ingest(ev); err != nil {
			t.Fatalf("Ingest failed at hour 2, event %d: %v", i, err)
		}
	}

	if err := archiver.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Check partition 1 file
	p1Path := filepath.Join(dir, "pqbin_20261105_10.pqbin")
	events1, err := ReadArchiveFile(p1Path)
	if err != nil {
		t.Fatalf("ReadArchiveFile p1 failed: %v", err)
	}
	if len(events1) != 5 {
		t.Fatalf("expected 5 events in partition 1, got %d", len(events1))
	}
	if events1[0].Price != 50000.0 {
		t.Errorf("p1 event 0 price = %f, want 50000.0", events1[0].Price)
	}

	// Check partition 2 file
	p2Path := filepath.Join(dir, "pqbin_20261105_11.pqbin")
	events2, err := ReadArchiveFile(p2Path)
	if err != nil {
		t.Fatalf("ReadArchiveFile p2 failed: %v", err)
	}
	if len(events2) != 3 {
		t.Fatalf("expected 3 events in partition 2, got %d", len(events2))
	}
	if events2[0].Price != 51000.0 {
		t.Errorf("p2 event 0 price = %f, want 51000.0", events2[0].Price)
	}
}

func TestRollingArchiver_RetentionPruning(t *testing.T) {
	dir := t.TempDir()

	// Set quota strictly: only enough for ~2 files of size (20B + 2*184B = 388 bytes)
	fileSizeBytes := int64(PQBinHeaderSize + 2*PQBinRecordSize) // 388 bytes
	maxBytes := fileSizeBytes*2 + 10                            // allow max 2 files

	cfg := ArchiverConfig{
		ArchiveDir:        dir,
		MaxRetentionBytes: maxBytes,
		MaxRetentionHours: 24,
		BufferCapacity:    2,
	}

	archiver, err := NewRollingArchiver(cfg)
	if err != nil {
		t.Fatalf("NewRollingArchiver failed: %v", err)
	}
	defer archiver.Close()

	// Ingest across 4 distinct hours: 01, 02, 03, 04
	hours := []string{
		"2026-11-05 01:00:00",
		"2026-11-05 02:00:00",
		"2026-11-05 03:00:00",
		"2026-11-05 04:00:00",
	}

	for _, hStr := range hours {
		tm, _ := time.Parse("2006-01-02 15:04:05", hStr)
		ts := tm.UnixNano()
		for i := 0; i < 2; i++ {
			_ = archiver.Ingest(engine.MarketEvent{
				Timestamp: ts,
				Sequence:  uint64(i),
				Price:     50000.0,
			})
		}
	}
	_ = archiver.Flush()

	// Check files remaining in archive directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var remaining []string
	for _, e := range entries {
		if !e.IsDir() {
			remaining = append(remaining, e.Name())
		}
	}

	// Because maxBytes allows ~2 files, the oldest partitions (01 and 02) should have been pruned!
	if len(remaining) > 2 {
		t.Fatalf("retention pruning failed: expected at most 2 partitions, found %d: %v", len(remaining), remaining)
	}

	// Verify the oldest file pqbin_20261105_01.pqbin was removed
	p01 := filepath.Join(dir, "pqbin_20261105_01.pqbin")
	if _, err := os.Stat(p01); !os.IsNotExist(err) {
		t.Errorf("expected pqbin_20261105_01.pqbin to be pruned")
	}
}

func BenchmarkRollingArchiver_Ingest(b *testing.B) {
	dir := b.TempDir()
	cfg := ArchiverConfig{
		ArchiveDir:        dir,
		MaxRetentionBytes: 100 * 1024 * 1024,
		MaxRetentionHours: 168,
		BufferCapacity:    4096,
	}

	archiver, err := NewRollingArchiver(cfg)
	if err != nil {
		b.Fatalf("NewRollingArchiver failed: %v", err)
	}
	defer archiver.Close()

	ev := engine.MarketEvent{
		Timestamp: 1_700_000_000_000_000_000,
		Sequence:  12345,
		Price:     50000.0,
		Size:      1.5,
		Side:      1,
		Kind:      0,
		SymbolID:  1,
		Flags:     0,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = archiver.Ingest(ev)
	}
}
