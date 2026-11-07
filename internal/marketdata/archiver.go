package marketdata

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"piquant-live/internal/engine"
)

const (
	// PQBinMagic identifies PiQuant binary archive files.
	PQBinMagic = "PQBN"
	// PQBinVersion is the version identifier for .pqbin binary partitions.
	PQBinVersion = uint32(1)
	// PQBinHeaderSize is the 20-byte header (Magic:4B, Version:4B, SourceMtime:8B, Count:4B).
	PQBinHeaderSize = 20
	// PQBinRecordSize is the fixed 184-byte record size for each archived MarketEvent.
	PQBinRecordSize = 184
	// nsPerHour is the duration of one hour in nanoseconds.
	nsPerHour = int64(3600 * 1_000_000_000)
)

// ArchiverConfig holds storage and retention settings for RollingArchiver.
type ArchiverConfig struct {
	ArchiveDir        string // Base directory for partitioned .pqbin files
	MaxRetentionBytes int64  // Maximum cumulative disk budget in bytes (e.g. 5 GiB)
	MaxRetentionHours int    // Maximum archive age in hours before removal (e.g. 168 = 7 days)
	BufferCapacity    int    // Number of MarketEvents buffered in memory before flush
}

// DefaultArchiverConfig returns production defaults suited for continuous 14-day Raspberry Pi 5 runs.
func DefaultArchiverConfig() ArchiverConfig {
	return ArchiverConfig{
		ArchiveDir:        "data/archive",
		MaxRetentionBytes: 5 * 1024 * 1024 * 1024, // 5 GiB bounded budget
		MaxRetentionHours: 168,                    // 7 days retention
		BufferCapacity:    1024,                   // 1024 events * 184 bytes = ~188 KiB buffer
	}
}

// EncodeMarketEvent encodes an engine.MarketEvent into a fixed 184-byte little-endian record.
// Zero heap allocations.
func EncodeMarketEvent(ev *engine.MarketEvent, dst []byte) {
	binary.LittleEndian.PutUint64(dst[0:8], uint64(ev.Timestamp))
	binary.LittleEndian.PutUint64(dst[8:16], ev.Sequence)
	binary.LittleEndian.PutUint64(dst[16:24], math.Float64bits(ev.Price))
	binary.LittleEndian.PutUint64(dst[24:32], math.Float64bits(ev.Size))
	dst[32] = ev.Side
	dst[33] = ev.Kind
	binary.LittleEndian.PutUint16(dst[34:36], ev.SymbolID)
	binary.LittleEndian.PutUint32(dst[36:40], ev.Flags)
	// Clear remaining 144 bytes reserved slot
	for i := 40; i < PQBinRecordSize; i++ {
		dst[i] = 0
	}
}

// DecodeMarketEvent decodes an engine.MarketEvent from a 184-byte little-endian record.
func DecodeMarketEvent(src []byte) engine.MarketEvent {
	return engine.MarketEvent{
		Timestamp: int64(binary.LittleEndian.Uint64(src[0:8])),
		Sequence:  binary.LittleEndian.Uint64(src[8:16]),
		Price:     math.Float64frombits(binary.LittleEndian.Uint64(src[16:24])),
		Size:      math.Float64frombits(binary.LittleEndian.Uint64(src[24:32])),
		Side:      src[32],
		Kind:      src[33],
		SymbolID:  binary.LittleEndian.Uint16(src[34:36]),
		Flags:     binary.LittleEndian.Uint32(src[36:40]),
	}
}

// RollingArchiver manages continuous binary capture of high-frequency MarketEvents into
// hourly partitioned .pqbin files with bounded disk retention to prevent SD-card / NVMe exhaustion.
//
// Event ingestion on the hot path executes with zero heap allocations (0 B/op, 0 allocs/op).
type RollingArchiver struct {
	cfg ArchiverConfig
	mu  sync.Mutex

	// Ingestion buffer
	byteBuf  []byte // preallocated contiguous memory: BufferCapacity * PQBinRecordSize
	bufCount int
	bufCap   int

	// Partition state
	currentHourIndex int64  // Unix hour index: tsNs / (3600 * 1e9)
	currentPartKey   string // Format: YYYYMMDD_HH (e.g. 20261105_22)
	currentPath      string
	currentFile      *os.File
	recordCount      uint32

	headerBuf [PQBinHeaderSize]byte
	cntBuf    [4]byte
	closed    bool
}

// NewRollingArchiver instantiates a RollingArchiver configured for bounded continuous archiving.
func NewRollingArchiver(cfg ArchiverConfig) (*RollingArchiver, error) {
	if cfg.ArchiveDir == "" {
		cfg.ArchiveDir = "data/archive"
	}
	if cfg.BufferCapacity <= 0 {
		cfg.BufferCapacity = 1024
	}
	if cfg.MaxRetentionBytes <= 0 {
		cfg.MaxRetentionBytes = 5 * 1024 * 1024 * 1024
	}
	if cfg.MaxRetentionHours <= 0 {
		cfg.MaxRetentionHours = 168
	}

	if err := os.MkdirAll(cfg.ArchiveDir, 0755); err != nil {
		return nil, fmt.Errorf("archiver: create dir %q: %w", cfg.ArchiveDir, err)
	}

	buf := make([]byte, cfg.BufferCapacity*PQBinRecordSize)

	return &RollingArchiver{
		cfg:      cfg,
		byteBuf:  buf,
		bufCap:   cfg.BufferCapacity,
		bufCount: 0,
	}, nil
}

// Ingest buffers an incoming MarketEvent. When the partition rotates or the in-memory buffer fills,
// chunks are flushed to disk.
//
// Zero heap allocations on the hot path (0 B/op, 0 allocs/op).
func (a *RollingArchiver) Ingest(ev engine.MarketEvent) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.New("archiver: closed")
	}

	hourIdx := ev.Timestamp / nsPerHour
	if hourIdx != a.currentHourIndex || a.currentFile == nil {
		partKey := a.partitionKey(ev.Timestamp)
		if err := a.rotatePartitionLocked(partKey, hourIdx, ev.Timestamp); err != nil {
			a.mu.Unlock()
			return err
		}
	}

	offset := a.bufCount * PQBinRecordSize
	dst := a.byteBuf[offset : offset+PQBinRecordSize]
	binary.LittleEndian.PutUint64(dst[0:8], uint64(ev.Timestamp))
	binary.LittleEndian.PutUint64(dst[8:16], ev.Sequence)
	binary.LittleEndian.PutUint64(dst[16:24], math.Float64bits(ev.Price))
	binary.LittleEndian.PutUint64(dst[24:32], math.Float64bits(ev.Size))
	dst[32] = ev.Side
	dst[33] = ev.Kind
	binary.LittleEndian.PutUint16(dst[34:36], ev.SymbolID)
	binary.LittleEndian.PutUint32(dst[36:40], ev.Flags)
	for i := 40; i < PQBinRecordSize; i++ {
		dst[i] = 0
	}
	a.bufCount++

	if a.bufCount >= a.bufCap {
		err := a.flushBufferLocked()
		a.mu.Unlock()
		return err
	}

	a.mu.Unlock()
	return nil
}

// partitionKey computes the UTC hourly partition key "YYYYMMDD_HH" from event timestamp.
func (a *RollingArchiver) partitionKey(tsNs int64) string {
	if tsNs <= 0 {
		return time.Now().UTC().Format("20060102_15")
	}
	return time.Unix(0, tsNs).UTC().Format("20060102_15")
}

// rotatePartitionLocked closes the active partition, executes retention pruning, and opens a new partition file.
func (a *RollingArchiver) rotatePartitionLocked(newKey string, hourIdx int64, tsNs int64) error {
	if a.bufCount > 0 {
		if err := a.flushBufferLocked(); err != nil {
			return err
		}
	}

	if a.currentFile != nil {
		a.updateHeaderCountLocked()
		_ = a.currentFile.Close()
		a.currentFile = nil
	}

	// Automatic retention pruning to guard SD card / NVMe quota
	_ = a.pruneRetentionLocked()

	// Hourly filename: data/archive/pqbin_YYYYMMDD_HH.pqbin
	fileName := fmt.Sprintf("pqbin_%s.pqbin", newKey)
	filePath := filepath.Join(a.cfg.ArchiveDir, fileName)

	fileInfo, statErr := os.Stat(filePath)
	var f *os.File
	var recCount uint32 = 0

	if statErr == nil && fileInfo.Size() >= int64(PQBinHeaderSize) {
		// Existing partition: open for append
		var err error
		f, err = os.OpenFile(filePath, os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("archiver: open partition %s: %w", filePath, err)
		}
		// Read existing count from header
		if _, err := f.ReadAt(a.cntBuf[:], 16); err == nil {
			recCount = binary.LittleEndian.Uint32(a.cntBuf[:])
		} else {
			recCount = uint32((fileInfo.Size() - int64(PQBinHeaderSize)) / int64(PQBinRecordSize))
		}
		_, _ = f.Seek(0, io.SeekEnd)
	} else {
		// Fresh partition: initialize with 20-byte PQBN header
		var err error
		f, err = os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("archiver: create partition %s: %w", filePath, err)
		}
		copy(a.headerBuf[0:4], PQBinMagic)
		binary.LittleEndian.PutUint32(a.headerBuf[4:8], PQBinVersion)
		binary.LittleEndian.PutUint64(a.headerBuf[8:16], uint64(tsNs))
		binary.LittleEndian.PutUint32(a.headerBuf[16:20], 0)
		if _, err := f.Write(a.headerBuf[:]); err != nil {
			_ = f.Close()
			return fmt.Errorf("archiver: write header %s: %w", filePath, err)
		}
	}

	a.currentHourIndex = hourIdx
	a.currentPartKey = newKey
	a.currentPath = filePath
	a.currentFile = f
	a.recordCount = recCount

	return nil
}

// flushBufferLocked writes buffered records to the active partition file.
func (a *RollingArchiver) flushBufferLocked() error {
	if a.bufCount == 0 || a.currentFile == nil {
		return nil
	}

	bytesToWrite := a.bufCount * PQBinRecordSize
	n, err := a.currentFile.Write(a.byteBuf[:bytesToWrite])
	if err != nil {
		return fmt.Errorf("archiver: write to %s: %w", a.currentPath, err)
	}
	if n != bytesToWrite {
		return fmt.Errorf("archiver: partial write to %s: wrote %d of %d", a.currentPath, n, bytesToWrite)
	}

	a.recordCount += uint32(a.bufCount)
	a.bufCount = 0
	return nil
}

// updateHeaderCountLocked updates the sampleCount uint32 field at offset 16 in the active partition file.
func (a *RollingArchiver) updateHeaderCountLocked() {
	if a.currentFile == nil {
		return
	}
	binary.LittleEndian.PutUint32(a.cntBuf[:], a.recordCount)
	_, _ = a.currentFile.WriteAt(a.cntBuf[:], 16)
}

// Flush flushes all buffered records to disk.
func (a *RollingArchiver) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.New("archiver: closed")
	}
	if err := a.flushBufferLocked(); err != nil {
		return err
	}
	a.updateHeaderCountLocked()
	return nil
}

// Close flushes buffered events, updates file headers, and closes active file handles.
func (a *RollingArchiver) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true

	var flushErr error
	if a.bufCount > 0 {
		flushErr = a.flushBufferLocked()
	}

	if a.currentFile != nil {
		a.updateHeaderCountLocked()
		closeErr := a.currentFile.Close()
		a.currentFile = nil
		if flushErr != nil {
			return flushErr
		}
		return closeErr
	}
	return flushErr
}

// archiveFileInfo wraps file metadata for retention analysis.
type archiveFileInfo struct {
	path    string
	name    string
	size    int64
	modTime time.Time
	partKey string
}

// pruneRetentionLocked enforces MaxRetentionHours and MaxRetentionBytes policies.
func (a *RollingArchiver) pruneRetentionLocked() error {
	entries, err := os.ReadDir(a.cfg.ArchiveDir)
	if err != nil {
		return err
	}

	var files []archiveFileInfo
	var totalBytes int64

	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(a.cfg.MaxRetentionHours) * time.Hour)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "pqbin_") || !strings.HasSuffix(name, ".pqbin") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		partKey := strings.TrimSuffix(strings.TrimPrefix(name, "pqbin_"), ".pqbin")
		fullPath := filepath.Join(a.cfg.ArchiveDir, name)

		// 1. Enforce MaxRetentionHours: remove expired files (older than cutoff)
		if a.cfg.MaxRetentionHours > 0 {
			if partTime, err := time.Parse("20060102_15", partKey); err == nil {
				if partTime.Before(cutoff) && fullPath != a.currentPath {
					_ = os.Remove(fullPath)
					continue
				}
			}
		}

		files = append(files, archiveFileInfo{
			path:    fullPath,
			name:    name,
			size:    info.Size(),
			modTime: info.ModTime(),
			partKey: partKey,
		})
		totalBytes += info.Size()
	}

	// 2. Enforce MaxRetentionBytes: remove oldest partitions until within quota
	if a.cfg.MaxRetentionBytes > 0 && totalBytes > a.cfg.MaxRetentionBytes {
		// Sort chronologically ascending (oldest first)
		sort.Slice(files, func(i, j int) bool {
			return files[i].partKey < files[j].partKey
		})

		for _, f := range files {
			if totalBytes <= a.cfg.MaxRetentionBytes {
				break
			}
			// Never prune the active partition
			if f.path == a.currentPath {
				continue
			}
			if err := os.Remove(f.path); err == nil {
				totalBytes -= f.size
			}
		}
	}

	return nil
}

// ReadArchiveFile reads a .pqbin partition file and returns the decoded MarketEvents.
func ReadArchiveFile(path string) ([]engine.MarketEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < PQBinHeaderSize {
		return nil, errors.New("archiver: file smaller than header")
	}

	offset := 0
	if string(data[0:4]) == PQBinMagic {
		offset = PQBinHeaderSize
	}

	remaining := len(data) - offset
	if remaining%PQBinRecordSize != 0 {
		return nil, fmt.Errorf("archiver: corrupt file, remaining bytes %d not multiple of %d", remaining, PQBinRecordSize)
	}

	count := remaining / PQBinRecordSize
	events := make([]engine.MarketEvent, count)
	for i := 0; i < count; i++ {
		events[i] = DecodeMarketEvent(data[offset : offset+PQBinRecordSize])
		offset += PQBinRecordSize
	}

	return events, nil
}
