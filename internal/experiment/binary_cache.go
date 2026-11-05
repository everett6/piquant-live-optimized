package experiment

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piquant-live/internal/models/common"
)

const (
	pqbinMagic      = "PQBN"
	pqbinVersion    = uint32(1)
	pqbinHeaderSize = 20  // 4B magic + 4B version + 8B sourceMtime + 4B sampleCount
	pqbinRecordSize = 184 // TsNs(8) + Mid(8) + Spread(8) + Vol(8) + Target(8) + TargetCls(8) + Features[16](128) + Reserved(8)
)

// resolveSourceMtime infers the modification timestamp of the underlying .jsonl file
// when path follows "<datasetPath>.<horizonNs>.pqbin" or "<datasetPath>.pqbin",
// falling back to current wall time if no source file exists on disk.
func resolveSourceMtime(cachePath string) int64 {
	if strings.HasSuffix(cachePath, ".pqbin") {
		base := strings.TrimSuffix(cachePath, ".pqbin")
		if info, err := os.Stat(base); err == nil {
			return info.ModTime().UnixNano()
		}
		if dot := strings.LastIndexByte(base, '.'); dot > 0 {
			candidate := base[:dot]
			if info, err := os.Stat(candidate); err == nil {
				return info.ModTime().UnixNano()
			}
		}
	}
	return time.Now().UnixNano()
}

// SaveBinarySampleCache serializes samples into a compact little-endian .pqbin binary file
// with a 20-byte header and fixed 184-byte records per common.Sample.
func SaveBinarySampleCache(path string, samples []common.Sample) error {
	return SaveBinarySampleCacheWithMtime(path, resolveSourceMtime(path), samples)
}

// SaveBinarySampleCacheWithMtime writes the binary sample cache with an explicit sourceMtime.
func SaveBinarySampleCacheWithMtime(path string, sourceMtime int64, samples []common.Sample) error {
	if path == "" {
		return errors.New("binary cache: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	count := len(samples)
	totalSize := pqbinHeaderSize + count*pqbinRecordSize
	buf := make([]byte, totalSize)

	// Header: "PQBN" (4B), Version 1 (4B), SourceMtime (8B), Count (4B)
	copy(buf[0:4], pqbinMagic)
	binary.LittleEndian.PutUint32(buf[4:8], pqbinVersion)
	binary.LittleEndian.PutUint64(buf[8:16], uint64(sourceMtime))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(count))

	offset := pqbinHeaderSize
	for i := 0; i < count; i++ {
		s := &samples[i]
		rec := buf[offset : offset+pqbinRecordSize]

		spread := s.Spread
		if spread == 0 {
			spread = s.X[4]
		}
		vol := s.Vol
		if vol == 0 {
			vol = s.X[10]
		}
		targetCls := s.TargetCls
		if targetCls == 0 {
			if s.Y > 0 {
				targetCls = 1
			} else if s.Y < 0 {
				targetCls = -1
			}
		}

		binary.LittleEndian.PutUint64(rec[0:8], uint64(s.TsNs))
		binary.LittleEndian.PutUint64(rec[8:16], math.Float64bits(s.Mid))
		binary.LittleEndian.PutUint64(rec[16:24], math.Float64bits(spread))
		binary.LittleEndian.PutUint64(rec[24:32], math.Float64bits(vol))
		binary.LittleEndian.PutUint64(rec[32:40], math.Float64bits(s.Y))
		binary.LittleEndian.PutUint64(rec[40:48], uint64(targetCls))

		featOff := 48
		for f := 0; f < common.FeatureCount; f++ {
			binary.LittleEndian.PutUint64(rec[featOff:featOff+8], math.Float64bits(s.X[f]))
			featOff += 8
		}
		// Bytes 176..184 reserved/checksum slot (zeroed by make)
		offset += pqbinRecordSize
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// LoadBinarySampleCache loads a .pqbin sample slice in a single zero-parse pass.
// Returns (samples, true) if the file is valid and matches or post-dates sourceMtime.
func LoadBinarySampleCache(path string, sourceMtime int64) ([]common.Sample, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < pqbinHeaderSize {
		return nil, false
	}
	if string(data[0:4]) != pqbinMagic {
		return nil, false
	}
	ver := binary.LittleEndian.Uint32(data[4:8])
	if ver != pqbinVersion {
		return nil, false
	}
	headerMtime := int64(binary.LittleEndian.Uint64(data[8:16]))
	count := int(binary.LittleEndian.Uint32(data[16:20]))

	// If caller specifies a positive sourceMtime and it is strictly newer than the cached source mtime,
	// invalidate the cache.
	if sourceMtime > 0 && headerMtime > 0 && sourceMtime > headerMtime {
		return nil, false
	}

	expectedLen := pqbinHeaderSize + count*pqbinRecordSize
	if len(data) != expectedLen {
		return nil, false
	}

	samples := make([]common.Sample, count)
	offset := pqbinHeaderSize
	for i := 0; i < count; i++ {
		rec := data[offset : offset+pqbinRecordSize]
		s := &samples[i]
		s.TsNs = int64(binary.LittleEndian.Uint64(rec[0:8]))
		s.Mid = math.Float64frombits(binary.LittleEndian.Uint64(rec[8:16]))
		s.Spread = math.Float64frombits(binary.LittleEndian.Uint64(rec[16:24]))
		s.Vol = math.Float64frombits(binary.LittleEndian.Uint64(rec[24:32]))
		s.Y = math.Float64frombits(binary.LittleEndian.Uint64(rec[32:40]))
		s.TargetCls = int64(binary.LittleEndian.Uint64(rec[40:48]))

		featOff := 48
		for f := 0; f < common.FeatureCount; f++ {
			s.X[f] = math.Float64frombits(binary.LittleEndian.Uint64(rec[featOff : featOff+8]))
			featOff += 8
		}
		offset += pqbinRecordSize
	}

	return samples, true
}
