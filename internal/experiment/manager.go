package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/dataset"
	"piquant-live/internal/models/common"
	"piquant-live/internal/telemetry"
)

// FeasibilityResult provides guidance on whether an experiment is runnable.
type FeasibilityResult struct {
	Feasible     bool     `json:"feasible"`
	Warnings     []string `json:"warnings,omitempty"`
	Errors       []string `json:"errors,omitempty"`
	EstimatedRAM string   `json:"estimated_ram"`
	EstimatedSec int      `json:"estimated_sec"`
}

// Manager orchestrates experiments, persistence, and scheduling.
type Manager struct {
	dataDir string
	cfg     *config.Config

	mu           sync.RWMutex
	experiments  map[string]*Experiment
	searches     map[string]*ParameterSearch
	activeCancel context.CancelFunc
	currentJob   string

	// ActivityLog records user-facing milestone events in human-friendly language.
	activityLog []string

	datasetCache map[string]datasetCacheEntry
	telCollector *telemetry.Collector
}

type datasetCacheEntry struct {
	modTime time.Time
	size    int64
	meta    DatasetMeta
}

// NewManager creates a manager rooted at dataDir.
func NewManager(dataDir string, cfg *config.Config) (*Manager, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	expDir := filepath.Join(dataDir, "experiments")
	searchDir := filepath.Join(dataDir, "searches")
	if err := os.MkdirAll(expDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(searchDir, 0o755); err != nil {
		return nil, err
	}

	m := &Manager{
		dataDir:      dataDir,
		cfg:          cfg,
		experiments:  make(map[string]*Experiment),
		searches:     make(map[string]*ParameterSearch),
		datasetCache: make(map[string]datasetCacheEntry),
		telCollector: telemetry.New(dataDir),
		activityLog: []string{
			"Research engine started.",
			"Hardware telemetry initialized.",
		},
	}

	m.loadPersisted()
	return m, nil
}

// LogActivity adds a human-readable event to the dashboard activity feed.
func (m *Manager) LogActivity(event string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activityLog = append(m.activityLog, event)
	if len(m.activityLog) > 30 {
		m.activityLog = m.activityLog[len(m.activityLog)-30:]
	}
}

// ActivityEvents returns the recent human-friendly events.
func (m *Manager) ActivityEvents() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]string, len(m.activityLog))
	copy(res, m.activityLog)
	return res
}

// loadPersisted loads existing experiment records and recovers interrupted jobs.
func (m *Manager) loadPersisted() {
	expDir := filepath.Join(m.dataDir, "experiments")
	entries, _ := os.ReadDir(expDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(expDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var exp Experiment
		if err := json.Unmarshal(data, &exp); err == nil && exp.ID != "" {
			// Crash recovery: mark interrupted if process previously died while running
			if exp.State == StateRunning || exp.State == StateQueued {
				exp.State = StateInterrupted
				exp.Error = "experiment interrupted by system restart or power interruption; safe to rerun"
				_ = m.saveExperiment(&exp)
			}
			m.experiments[exp.ID] = &exp
		}
	}

	searchDir := filepath.Join(m.dataDir, "searches")
	sEntries, _ := os.ReadDir(searchDir)
	for _, e := range sEntries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(searchDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var ps ParameterSearch
		if err := json.Unmarshal(data, &ps); err == nil && ps.ID != "" {
			if ps.State == StateRunning || ps.State == StateQueued {
				ps.State = StateInterrupted
				ps.Error = "parameter search interrupted by system restart"
				_ = m.saveSearch(&ps)
			}
			m.searches[ps.ID] = &ps
		}
	}
}

func (m *Manager) saveExperiment(exp *Experiment) error {
	path := filepath.Join(m.dataDir, "experiments", fmt.Sprintf("%s.json", exp.ID))
	tmpPath := path + ".tmp"
	data, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (m *Manager) saveSearch(ps *ParameterSearch) error {
	path := filepath.Join(m.dataDir, "searches", fmt.Sprintf("%s.json", ps.ID))
	tmpPath := path + ".tmp"
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// ValidateFeasibility checks if an experiment can execute safely.
func (m *Manager) ValidateFeasibility(datasetPath string, horizon common.Horizon) FeasibilityResult {
	res := FeasibilityResult{Feasible: true, EstimatedRAM: "350 MiB", EstimatedSec: 15}

	info, err := os.Stat(datasetPath)
	if err != nil {
		res.Feasible = false
		res.Errors = append(res.Errors, fmt.Sprintf("dataset file not found: %s", datasetPath))
		return res
	}

	if info.Size() < 5000 {
		res.Warnings = append(res.Warnings, "dataset is very small (< 5 KB); results may have high statistical variance")
	}

	if horizon.Duration() > 10*time.Minute {
		res.Warnings = append(res.Warnings, "long forecast horizon requires extended continuous historical recording")
	}

	return res
}

// LaunchExperiment starts an experiment in a background goroutine.
func (m *Manager) LaunchExperiment(exp *Experiment) error {
	m.mu.Lock()
	if m.currentJob != "" {
		m.mu.Unlock()
		return errors.New("another experiment or search is currently active; please wait or cancel it")
	}

	if exp.ID == "" {
		exp.ID = fmt.Sprintf("exp-%d", time.Now().Unix())
	}
	if exp.CreatedAt.IsZero() {
		exp.CreatedAt = time.Now()
	}
	exp.State = StateQueued
	m.experiments[exp.ID] = exp
	_ = m.saveExperiment(exp)

	ctx, cancel := context.WithCancel(context.Background())
	m.activeCancel = cancel
	m.currentJob = exp.ID
	m.mu.Unlock()

	m.LogActivity(fmt.Sprintf("Experiment #%s started: %s (%s).", exp.ID, exp.Model, exp.Horizon))

	go func() {
		defer func() {
			m.mu.Lock()
			m.currentJob = ""
			m.activeCancel = nil
			_ = m.saveExperiment(exp)
			m.mu.Unlock()
			if exp.State == StateCompleted {
				m.LogActivity(fmt.Sprintf("Experiment #%s completed successfully (Val MAE: %.2f bps).", exp.ID, exp.ValMetrics.MAE))
			} else if exp.State == StateFailed {
				m.LogActivity(fmt.Sprintf("Experiment #%s failed: %s", exp.ID, exp.Error))
			}
		}()

		_ = Run(ctx, exp, m.cfg, func(stage string, pct float64) {
			m.mu.Lock()
			exp.Stage = stage
			exp.Progress = pct
			m.mu.Unlock()
		})
	}()

	return nil
}

// ThermalRecommendedWorkers samples the CPU core temperature and returns the thermally safe
// parallel worker count for multi-seed or hyperparameter evaluations.
func (m *Manager) ThermalRecommendedWorkers() (bool, int, float64) {
	var tempC float64
	if m != nil && m.telCollector != nil {
		sample := m.telCollector.Collect()
		if sample.Host.TempOK {
			tempC = float64(sample.Host.TempC)
		}
	}
	throttled, workers := ShouldThrottleForThermal(tempC)
	return throttled, workers, tempC
}

// LaunchSearch starts a parameter search in a background goroutine.
func (m *Manager) LaunchSearch(ps *ParameterSearch, space SearchSpace) error {
	m.mu.Lock()
	if m.currentJob != "" {
		m.mu.Unlock()
		return errors.New("another experiment or search is currently active")
	}

	if ps.ID == "" {
		ps.ID = fmt.Sprintf("search-%d", time.Now().Unix())
	}
	if ps.CreatedAt.IsZero() {
		ps.CreatedAt = time.Now()
	}
	ps.State = StateQueued
	m.searches[ps.ID] = ps
	_ = m.saveSearch(ps)

	ctx, cancel := context.WithCancel(context.Background())
	m.activeCancel = cancel
	m.currentJob = ps.ID
	m.mu.Unlock()

	throttled, workers, tempC := m.ThermalRecommendedWorkers()
	if throttled {
		m.LogActivity(fmt.Sprintf("Thermal guardrail active (%.1f°C): throttling search #%s to %d workers.", tempC, ps.ID, workers))
	} else {
		m.LogActivity(fmt.Sprintf("Parameter search #%s started for %s (%d workers).", ps.ID, ps.Model, workers))
	}

	go func() {
		defer func() {
			m.mu.Lock()
			m.currentJob = ""
			m.activeCancel = nil
			_ = m.saveSearch(ps)
			m.mu.Unlock()
			if ps.State == StateCompleted {
				m.LogActivity(fmt.Sprintf("Parameter search #%s completed. Best trial: lambda=%.4f (Val MAE: %.2f bps).", ps.ID, ps.BestTrial.Lambda, ps.BestTrial.ValMAE))
			}
		}()

		_ = RunSearch(ctx, ps, space, m.cfg, func(pct float64) {
			m.mu.Lock()
			ps.Progress = pct
			m.mu.Unlock()
		})
	}()

	return nil
}

// LaunchQuantumSearch starts a Quantum-Inspired Annealing hyperparameter search in a background goroutine.
func (m *Manager) LaunchQuantumSearch(ps *ParameterSearch, opts QuantumSearchOptions) error {
	m.mu.Lock()
	if m.currentJob != "" {
		m.mu.Unlock()
		return errors.New("another experiment or search is currently active")
	}

	if ps.ID == "" {
		ps.ID = fmt.Sprintf("qsearch-%d", time.Now().Unix())
	}
	if ps.CreatedAt.IsZero() {
		ps.CreatedAt = time.Now()
	}
	ps.State = StateQueued
	m.searches[ps.ID] = ps
	_ = m.saveSearch(ps)

	ctx, cancel := context.WithCancel(context.Background())
	m.activeCancel = cancel
	m.currentJob = ps.ID
	m.mu.Unlock()

	m.LogActivity(fmt.Sprintf("Quantum-Inspired Annealing search #%s started for %s.", ps.ID, ps.Model))

	go func() {
		defer func() {
			m.mu.Lock()
			m.currentJob = ""
			m.activeCancel = nil
			_ = m.saveSearch(ps)
			m.mu.Unlock()
			if ps.State == StateCompleted && ps.BestTrial != nil {
				m.LogActivity(fmt.Sprintf("Quantum search #%s completed. Best trial: lambda=%.4f (Val MAE: %.2f bps).", ps.ID, ps.BestTrial.Lambda, ps.BestTrial.ValMAE))
			}
		}()

		_ = RunQuantumSearch(ctx, ps, opts, m.cfg, func(pct float64) {
			m.mu.Lock()
			ps.Progress = pct
			m.mu.Unlock()
		})
	}()

	return nil
}

// CancelActive cancels any running experiment or search.
func (m *Manager) CancelActive() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeCancel != nil {
		m.activeCancel()
		m.LogActivity("Current experiment cancelled by user.")
		return nil
	}
	return errors.New("no active experiment to cancel")
}

// ListExperiments returns all experiments sorted newest first.
func (m *Manager) ListExperiments() []*Experiment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Experiment, 0, len(m.experiments))
	for _, e := range m.experiments {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list
}

// ListSearches returns all parameter searches sorted newest first.
func (m *Manager) ListSearches() []*ParameterSearch {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*ParameterSearch, 0, len(m.searches))
	for _, s := range m.searches {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list
}

// GetExperiment retrieves a single experiment by ID.
func (m *Manager) GetExperiment(id string) (*Experiment, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.experiments[id]
	return e, ok
}

// ActiveJob returns the ID and progress of the currently active job, if any.
func (m *Manager) ActiveJob() (string, float64, string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.currentJob == "" {
		return "", 0, "", false
	}
	if exp, ok := m.experiments[m.currentJob]; ok {
		return exp.ID, exp.Progress, exp.Stage, true
	}
	if ps, ok := m.searches[m.currentJob]; ok {
		return ps.ID, ps.Progress, "search", true
	}
	return m.currentJob, 0, "active", true
}

// DatasetMeta holds summary info about an available dataset file.
type DatasetMeta struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Checksum    string `json:"checksum"`
	Records     int64  `json:"records"`
	Rejected    int64  `json:"rejected"`
	QualityDesc string `json:"quality_desc"`
}

// ListDatasets scans dataDir for .jsonl capture files with in-memory stat caching to eliminate disk read overhead.
func (m *Manager) ListDatasets() []DatasetMeta {
	m.mu.Lock()
	defer m.mu.Unlock()

	pattern := filepath.Join(m.dataDir, "*.jsonl")
	matches, _ := filepath.Glob(pattern)
	var list []DatasetMeta
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if entry, ok := m.datasetCache[p]; ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
			list = append(list, entry.meta)
			continue
		}

		csum, _ := dataset.Checksum(p)
		r, err := dataset.Open(p)
		var records, rejected int64
		desc := "valid capture"
		if err == nil {
			for {
				_, err := r.Next()
				if err != nil {
					break
				}
			}
			q := r.Quality()
			records = q.Records
			rejected = q.Rejected
			desc = q.Describe()
			_ = r.Close()
		}
		meta := DatasetMeta{
			Name:        filepath.Base(p),
			Path:        p,
			Size:        info.Size(),
			Checksum:    csum,
			Records:     records,
			Rejected:    rejected,
			QualityDesc: desc,
		}
		m.datasetCache[p] = datasetCacheEntry{
			modTime: info.ModTime(),
			size:    info.Size(),
			meta:    meta,
		}
		list = append(list, meta)
	}
	return list
}
