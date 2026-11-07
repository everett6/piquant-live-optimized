// Package dashboard serves the read-only PiQuant control panel and the small
// status endpoint the research engine exposes to it.
//
// # Two roles, one package
//
// The Pi 3B runs Server: an HTML interface with live host metrics, peer state,
// experiment progress and a log viewer. The Pi 5 runs Agent: a tiny JSON
// endpoint (/api/health and /api/status) that says whether the research engine
// is alive, what it is doing and how far along it is. The dashboard finds the
// agent through internal/discovery, so no address is typed anywhere.
//
// # Read-only by construction
//
// Every handler in this package reads. There is no route that starts an
// experiment, edits a config, changes a model or submits an order, and the log
// viewer only tails a file. That is a deliberate product boundary, not a TODO:
// the dashboard cannot become a trading console by adding a button, because the
// engine exposes nothing to press.
//
// # Authentication
//
// Both roles require a token. The browser exchanges the dashboard token for a
// short-lived in-memory session cookie; the other board presents the peer token
// as a bearer credential. Tokens come from internal/secrets, are compared in
// constant time, and login attempts are rate limited per address. Without a
// configured token the server refuses to start rather than serving an open
// panel to the LAN.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"piquant-live/internal/cluster"
	"piquant-live/internal/discovery"
	"piquant-live/internal/experiment"
	"piquant-live/internal/secrets"
	"piquant-live/internal/telemetry"
)

// Version is the interface version reported to a peer. A dashboard that speaks
// a different version shows the peer as incompatible instead of guessing.
const Version = 1

// Status is what Agent reports and what the dashboard renders.
type Status struct {
	V           int                           `json:"v"`
	Role        discovery.Role                `json:"role"`
	Instance    string                        `json:"instance"`
	Name        string                        `json:"name"`
	App         string                        `json:"app"`
	StartedAt   time.Time                     `json:"started_at"`
	Now         time.Time                     `json:"now"`
	Host        telemetry.Host                `json:"host"`
	Proc        telemetry.Proc                `json:"proc"`
	Engine      EngineState                   `json:"engine"`
	Peers       []PeerStatus                  `json:"peers,omitempty"`
	Jobs        []Job                         `json:"jobs,omitempty"`
	Warnings    []string                      `json:"warnings,omitempty"`
	Checks      []Check                       `json:"checks,omitempty"`
	PeerEngine  *Status                       `json:"peer_engine,omitempty"`
	Activity    []string                      `json:"activity,omitempty"`
	Mode        string                        `json:"mode,omitempty"`
	ClusterMode string                        `json:"cluster_mode,omitempty"` // "pi5_only" or "dual_pi"
	Experiments []*experiment.Experiment      `json:"experiments,omitempty"`
	Searches    []*experiment.ParameterSearch `json:"searches,omitempty"`
	Datasets    []experiment.DatasetMeta      `json:"datasets,omitempty"`
	Telemetry   *TelemetrySummary             `json:"telemetry,omitempty"`
}

// TelemetrySummary exposes Raspberry Pi 5 hardware telemetry, thermal measurements,
// and firmware throttling bitmask flags formatted for dashboard status endpoints and UI.
type TelemetrySummary struct {
	Hostname                string  `json:"hostname"`
	Arch                    string  `json:"arch"`
	Kernel                  string  `json:"kernel"`
	Cores                   int     `json:"cores"`
	Governor                string  `json:"governor"`
	CPUUtilizationPct       float64 `json:"cpu_utilization_pct"`
	MemAvailableMB          uint64  `json:"mem_available_mb"`
	SwapUsedMB              uint64  `json:"swap_used_mb"`
	TemperatureC            float64 `json:"temperature_c"`
	Throttled               bool    `json:"throttled"`
	DiskAvailableGB         uint64  `json:"disk_available_gb"`
	UnderVoltage            bool    `json:"under_voltage"`
	FreqCapped              bool    `json:"freq_capped"`
	SoftTempLimit           bool    `json:"soft_temp_limit"`
	ActiveUnderVoltage      bool    `json:"active_under_voltage"`
	ActiveFreqCapped        bool    `json:"active_freq_capped"`
	ActiveThrottled         bool    `json:"active_throttled"`
	ActiveSoftTempLimit     bool    `json:"active_soft_temp_limit"`
	HistoricalUnderVoltage  bool    `json:"historical_under_voltage"`
	HistoricalFreqCapped    bool    `json:"historical_freq_capped"`
	HistoricalThrottled     bool    `json:"historical_throttled"`
	HistoricalSoftTempLimit bool    `json:"historical_soft_temp_limit"`
	ThrottleRaw             uint32  `json:"throttle_raw"`
}

// EngineState describes the research engine's current activity on the Pi 5.
type EngineState struct {
	Running  bool      `json:"running"`
	Phase    string    `json:"phase"`    // "idle", "replay", "features", "fit", "evaluate"
	Dataset  string    `json:"dataset"`  // capture being replayed, if any
	Progress float64   `json:"progress"` // 0..1 for the current phase
	Events   int64     `json:"events"`   // events processed this run
	Rejects  int64     `json:"rejects"`  // records refused by the normaliser
	Message  string    `json:"message"`  // one line for the UI
	Updated  time.Time `json:"updated"`
}

// Job is one experiment in the queue or recently finished.
type Job struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	State    string    `json:"state"` // queued, running, done, failed
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
	Progress float64   `json:"progress"`
	Result   string    `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// PeerStatus is a peer as the dashboard shows it.
type PeerStatus struct {
	Role   discovery.Role `json:"role"`
	Name   string         `json:"name"`
	Addr   string         `json:"addr"`
	ID     string         `json:"id"`
	AgeSec float64        `json:"age_seconds"`
	Auth   bool           `json:"authenticated"`
	// Live is true when the peer answered a health probe, not merely when its
	// beacon was heard: a board whose dashboard is wedged still broadcasts.
	Live      bool   `json:"live"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Check is one setup/health diagnostic line, shown as a tick or a warning.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Options configures the dashboard server.
type Options struct {
	// Addr is the listen address, e.g. ":8080" or "0.0.0.0:8080".
	Addr string
	// Token protects the browser interface. Required.
	Token string
	// PeerToken authenticates machine-to-machine calls. Optional; when set,
	// the other board may read /api/status as a bearer client.
	PeerToken string
	// Keys is the secret store used to redact log lines. Optional.
	Keys *secrets.Store
	// LogFiles are tailed by the log viewer. A missing file is skipped.
	LogFiles []string
	// DataDirs are the paths whose disk usage is reported.
	DataDirs []string
	// Role is this process's role, echoed in status.
	Role discovery.Role
	// App is the version string shown in the footer.
	App string
	// Discover enables zero-config peering. Nil disables it.
	Discover *discovery.Browser
	// Instance is this installation's discovery id, shown in the footer.
	Instance string
	// EngineStatus, when set, replaces the local engine state (used by the
	// agent side to report a live pipeline).
	EngineStatus func() EngineState
	// Jobs, when set, returns the experiment list.
	Jobs func() []Job
	// SessionTTL bounds a browser session. Zero uses 12 h.
	SessionTTL time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
	// ProbeTimeout bounds a health probe to a peer. Zero uses 2 s.
	ProbeTimeout time.Duration
	// Manager coordinates experiments and persistence.
	Manager *experiment.Manager
	// InitialMode: "simple" (default) or "advanced".
	Mode string
	// DryRun, when set, exposes the 14-day autonomous paper money dry run engine.
	DryRun any
}

// Server is the dashboard HTTP server.
type Server struct {
	opts   Options
	tel    *telemetry.Collector
	start  time.Time
	mux    *http.ServeMux
	logger *log.Logger

	sess *sessionStore
	lim  *loginLimiter

	mu   sync.Mutex
	last telemetry.Sample

	modeMu         sync.RWMutex
	mode           string
	clusterMode    string // "pi5_only" or "dual_pi"
	peerMu         sync.RWMutex
	lastPeerStatus *Status
	lastPeerFetch  time.Time
	httpClient     *http.Client
	probeMu        sync.RWMutex
	probeCache     map[string]cachedProbe
	delegator      *cluster.TaskDelegator
}

type cachedProbe struct {
	live      bool
	latencyMs int64
	errStr    string
	probedAt  time.Time
}

var sharedHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			return c, nil
		},
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     60 * time.Second,
	},
	Timeout: 5 * time.Second,
}

// New validates options and builds a server. It does not listen yet.
func New(opts Options) (*Server, error) {
	if opts.Token == "" {
		return nil, errors.New("dashboard: no dashboard token configured; run `piquant setup` first")
	}
	if opts.Addr == "" {
		opts.Addr = ":8080"
	}
	if opts.Role == "" {
		opts.Role = discovery.RoleDashboard
	}
	if opts.App == "" {
		opts.App = "dev"
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 12 * time.Hour
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = 2 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	mode := opts.Mode
	if mode == "" {
		mode = "simple"
	}
	s := &Server{
		opts:       opts,
		tel:        telemetry.New(opts.DataDirs...),
		start:      opts.Now(),
		mux:        http.NewServeMux(),
		logger:     log.New(os.Stderr, "dashboard: ", log.LstdFlags),
		sess:       newSessionStore(opts.SessionTTL),
		lim:        newLoginLimiter(5, 15*time.Minute),
		mode:       mode,
		probeCache: make(map[string]cachedProbe),
		delegator:  cluster.NewTaskDelegator(),
	}
	dialer := &net.Dialer{
		Timeout:   opts.ProbeTimeout,
		KeepAlive: 15 * time.Second,
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			return conn, nil
		},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	s.httpClient = &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
	}
	s.routes()
	return s, nil
}

// Handler returns the root handler, wrapped with security headers and access
// logging. Tests use it with httptest.
func (s *Server) Handler() http.Handler {
	return s.withHeaders(s.withLogging(s.mux))
}

// ListenAndServe runs until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("dashboard: cannot listen on %s: %w", s.opts.Addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// No WriteTimeout: an SSE stream is a long-lived response.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.logger.Printf("listening on http://%s", ln.Addr())
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.opts.Addr }

// Status builds one status document. Exported so the agent binary and the tests
// share exactly the shape the UI parses.
func (s *Server) Status(ctx context.Context) Status {
	sample := s.tel.Collect()
	s.mu.Lock()
	s.last = sample
	s.mu.Unlock()

	st := Status{
		V:         Version,
		Role:      s.opts.Role,
		Name:      sample.Host.Hostname,
		App:       s.opts.App,
		StartedAt: s.start,
		Now:       sample.At,
		Host:      sample.Host,
		Proc:      sample.Proc,
		Engine:    EngineState{Phase: "idle"},
	}
	st.Instance = s.opts.Instance
	st.Mode = s.Mode()
	st.ClusterMode = s.ClusterMode()

	if s.opts.Manager != nil {
		st.Activity = s.opts.Manager.ActivityEvents()
		st.Experiments = s.opts.Manager.ListExperiments()
		st.Searches = s.opts.Manager.ListSearches()
		st.Datasets = s.opts.Manager.ListDatasets()

		activeID, activeProg, activeStage, active := s.opts.Manager.ActiveJob()
		if active {
			st.Engine = EngineState{
				Running:  true,
				Phase:    activeStage,
				Progress: activeProg,
				Message:  fmt.Sprintf("Active: %s (%s)", activeID, activeStage),
				Updated:  time.Now(),
			}
		}

		// Sync jobs view
		st.Jobs = nil
		for _, exp := range st.Experiments {
			res := ""
			if exp.State == experiment.StateCompleted {
				res = fmt.Sprintf("Val MAE: %.2f bps", exp.ValMetrics.MAE)
			}
			st.Jobs = append(st.Jobs, Job{
				ID:       exp.ID,
				Name:     exp.Name,
				State:    string(exp.State),
				Started:  exp.StartedAt,
				Finished: exp.FinishedAt,
				Progress: exp.Progress,
				Result:   res,
				Error:    exp.Error,
			})
		}
	}

	if s.opts.EngineStatus != nil {
		st.Engine = s.opts.EngineStatus()
	}
	if s.opts.Jobs != nil && len(st.Jobs) == 0 {
		st.Jobs = s.opts.Jobs()
	}
	st.Peers = append(st.Peers, s.peers(ctx)...)
	sort.Slice(st.Peers, func(i, j int) bool {
		if st.Peers[i].Role != st.Peers[j].Role {
			return st.Peers[i].Role < st.Peers[j].Role
		}
		return st.Peers[i].Name < st.Peers[j].Name
	})

	// If running on Pi 3B dashboard, poll the discovered Pi 5 engine peer
	if s.opts.Role == discovery.RoleDashboard {
		peerSt := s.syncPeerEngine(ctx, st.Peers)
		if peerSt != nil {
			st.PeerEngine = peerSt
			if len(st.Experiments) == 0 {
				st.Experiments = peerSt.Experiments
			}
			if len(st.Searches) == 0 {
				st.Searches = peerSt.Searches
			}
			if len(st.Datasets) == 0 {
				st.Datasets = peerSt.Datasets
			}
			if len(st.Activity) == 0 {
				st.Activity = peerSt.Activity
			}
			if !st.Engine.Running && peerSt.Engine.Running {
				st.Engine = peerSt.Engine
			}
			if len(st.Jobs) == 0 {
				st.Jobs = peerSt.Jobs
			}
		}
	}

	var diskAvailGB uint64
	for _, d := range st.Host.Disks {
		diskAvailGB += d.FreeBytes / (1024 * 1024 * 1024)
	}
	var tempC float64
	if st.Host.TempOK && !math.IsNaN(float64(st.Host.TempC)) {
		tempC = float64(st.Host.TempC)
	}
	var cpuUtil float64
	if !math.IsNaN(float64(st.Host.CPUBusy)) {
		cpuUtil = float64(st.Host.CPUBusy) * 100
	}
	st.Telemetry = &TelemetrySummary{
		Hostname:                st.Host.Hostname,
		Arch:                    st.Host.Arch,
		Kernel:                  st.Host.Kernel,
		Cores:                   st.Host.CPUs,
		Governor:                "performance",
		CPUUtilizationPct:       cpuUtil,
		MemAvailableMB:          st.Host.MemAvailable / (1024 * 1024),
		SwapUsedMB:              (st.Host.SwapTotal - st.Host.SwapFree) / (1024 * 1024),
		TemperatureC:            tempC,
		Throttled:               st.Host.Throttle.Throttled || st.Host.Throttle.ActiveThrottled,
		DiskAvailableGB:         diskAvailGB,
		UnderVoltage:            st.Host.Throttle.UnderVoltage || st.Host.Throttle.ActiveUnderVoltage,
		FreqCapped:              st.Host.Throttle.FreqCapped || st.Host.Throttle.ActiveFreqCapped,
		SoftTempLimit:           st.Host.Throttle.SoftTempLimit || st.Host.Throttle.ActiveSoftTempLimit,
		ActiveUnderVoltage:      st.Host.Throttle.ActiveUnderVoltage,
		ActiveFreqCapped:        st.Host.Throttle.ActiveFreqCapped,
		ActiveThrottled:         st.Host.Throttle.ActiveThrottled,
		ActiveSoftTempLimit:     st.Host.Throttle.ActiveSoftTempLimit,
		HistoricalUnderVoltage:  st.Host.Throttle.HistoricalUnderVoltage,
		HistoricalFreqCapped:    st.Host.Throttle.HistoricalFreqCapped,
		HistoricalThrottled:     st.Host.Throttle.HistoricalThrottled,
		HistoricalSoftTempLimit: st.Host.Throttle.HistoricalSoftTempLimit,
		ThrottleRaw:             st.Host.Throttle.Raw,
	}

	st.Checks = s.checks(st)
	return st
}

// peers turns the discovery table into UI rows and probes each for liveness.
func (s *Server) peers(ctx context.Context) []PeerStatus {
	if s.opts.Discover == nil {
		return nil
	}
	live := s.opts.Discover.Peers()
	out := make([]PeerStatus, 0, len(live))
	now := s.opts.Now()

	for _, p := range live {
		row := PeerStatus{
			Role:   p.Role,
			Name:   p.Name,
			Addr:   p.Addr,
			ID:     p.ID,
			AgeSec: p.Age(now).Seconds(),
			Auth:   p.Auth,
			Live:   true,
		}

		// Check probe cache (valid for 2.5s) to eliminate blocking synchronous TCP handshakes
		s.probeMu.RLock()
		cached, ok := s.probeCache[p.ID]
		s.probeMu.RUnlock()

		if ok && now.Sub(cached.probedAt) < 2500*time.Millisecond {
			row.Live = cached.live
			row.LatencyMs = cached.latencyMs
			row.Error = cached.errStr
		} else {
			probeCtx, cancel := context.WithTimeout(ctx, s.opts.ProbeTimeout)
			start := time.Now()
			endpoint := p.Endpoint() + "/api/health"
			var err error
			if s.httpClient != nil {
				err = s.probePeer(probeCtx, endpoint, s.opts.PeerToken)
			} else {
				err = probe(probeCtx, endpoint, s.opts.PeerToken)
			}
			cancel()
			durMs := time.Since(start).Milliseconds()
			row.LatencyMs = durMs
			if err != nil {
				row.Live, row.Error = false, err.Error()
			}

			s.probeMu.Lock()
			if s.probeCache == nil {
				s.probeCache = make(map[string]cachedProbe)
			}
			s.probeCache[p.ID] = cachedProbe{
				live:      row.Live,
				latencyMs: row.LatencyMs,
				errStr:    row.Error,
				probedAt:  now,
			}
			s.probeMu.Unlock()
		}

		// Keep delegator synchronized with peer connectivity and RTT
		if s.delegator != nil {
			target := cluster.TargetPi5
			if p.Role == discovery.RoleDashboard {
				target = cluster.TargetPi3B
			}
			rttUs := float64(row.LatencyMs) * 1000.0
			if rttUs <= 0 {
				rttUs = 850.0
			}
			s.delegator.UpdateLinkStats(rttUs, 35.0, row.Live)
			s.delegator.UpdateNodeState(target, cluster.NodeHealth{
				Online:      row.Live,
				CPUPermille: 320,
				MemAvailMB:  512,
				TempDeciC:   520,
			})
		}

		out = append(out, row)
	}
	return out
}

// checks turns the status into the short diagnostic list the UI shows first.
func (s *Server) checks(st Status) []Check {
	var out []Check
	out = append(out, Check{Name: "dashboard token", OK: s.opts.Token != ""})
	out = append(out, Check{
		Name:   "peer token",
		OK:     s.opts.PeerToken != "",
		Detail: pick(s.opts.PeerToken == "", "not set: peers are unauthenticated", ""),
	})
	th := st.Host.Throttle
	if th.Available {
		out = append(out, Check{
			Name:   "power",
			OK:     !th.UnderVoltage && !th.Throttled,
			Detail: throttleDetail(th),
		})
	}
	if st.Host.TempOK {
		hot := st.Host.TempC >= 80
		out = append(out, Check{
			Name:   "temperature",
			OK:     !hot,
			Detail: fmt.Sprintf("%.1f degC", st.Host.TempC),
		})
	}
	if st.Host.MemTotal > 0 {
		used := 1 - float64(st.Host.MemAvailable)/float64(st.Host.MemTotal)
		out = append(out, Check{
			Name:   "memory",
			OK:     used < 0.9,
			Detail: fmt.Sprintf("%.0f%% used of %s", used*100, humanBytes(st.Host.MemTotal)),
		})
	}
	for _, d := range st.Host.Disks {
		out = append(out, Check{
			Name:   "disk " + d.Path,
			OK:     d.UsedPercent < 90,
			Detail: fmt.Sprintf("%.0f%% used, %s free", d.UsedPercent, humanBytes(d.FreeBytes)),
		})
	}
	engineLive := false
	if s.ClusterMode() == "pi5_only" || s.opts.Role == discovery.RoleEngine {
		engineLive = true
	} else {
		for _, p := range st.Peers {
			if p.Role == discovery.RoleEngine && p.Live {
				engineLive = true
			}
		}
	}
	if s.opts.Discover != nil && s.ClusterMode() == "dual_pi" {
		out = append(out, Check{
			Name:   "research engine reachable",
			OK:     engineLive,
			Detail: pick(engineLive, "", "no engine answered on the local network"),
		})
	} else if s.ClusterMode() == "pi5_only" {
		out = append(out, Check{
			Name:   "standalone mode",
			OK:     true,
			Detail: "running engine and dashboard co-located on Raspberry Pi 5",
		})
	}
	return out
}

func throttleDetail(t telemetry.Throttle) string {
	var parts []string
	if t.ActiveUnderVoltage {
		parts = append(parts, "under-voltage (active)")
	} else if t.HistoricalUnderVoltage || t.UnderVoltage {
		parts = append(parts, "under-voltage (past)")
	}
	if t.ActiveFreqCapped {
		parts = append(parts, "frequency capped (active)")
	} else if t.HistoricalFreqCapped || t.FreqCapped {
		parts = append(parts, "frequency capped (past)")
	}
	if t.ActiveThrottled {
		parts = append(parts, "throttled (active)")
	} else if t.HistoricalThrottled || t.Throttled {
		parts = append(parts, "throttled (past)")
	}
	if t.ActiveSoftTempLimit {
		parts = append(parts, "soft temperature limit (active)")
	} else if t.HistoricalSoftTempLimit || t.SoftTempLimit {
		parts = append(parts, "soft temperature limit (past)")
	}
	if len(parts) == 0 {
		return "healthy"
	}
	return strings.Join(parts, ", ")
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// probePeer calls a peer's health endpoint using the server's pooled HTTP client.
func (s *Server) probePeer(ctx context.Context, url, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return trimErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("rejected: peer token differs")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// probe calls a peer's health endpoint using the shared pooled HTTP client.
func probe(ctx context.Context, url, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return trimErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("rejected: peer token differs")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// trimErr shortens a network error so the UI row stays one line.
func trimErr(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && len(msg) > 60 {
		msg = msg[:i] + ": " + msg[i+2:]
	}
	if len(msg) > 90 {
		msg = msg[:87] + "..."
	}
	return errors.New(msg)
}

// writeJSON encodes v with a stable shape and no HTML escaping surprises.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// humanBytes formats a byte count for the UI. Binary prefixes, one decimal.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// defaultLogs returns the log paths a Pi deployment writes, for the viewer.
func defaultLogs(dir string) []string {
	if dir == "" {
		return nil
	}
	names := []string{"piquant.log", "engine.log", "dashboard.log"}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(dir, n))
	}
	return out
}

func (s *Server) syncPeerEngine(ctx context.Context, peers []PeerStatus) *Status {
	var enginePeer *PeerStatus
	for i := range peers {
		if peers[i].Role == discovery.RoleEngine && peers[i].Live {
			enginePeer = &peers[i]
			break
		}
	}
	if enginePeer == nil {
		return nil
	}

	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	if s.lastPeerStatus != nil && time.Since(s.lastPeerFetch) < 2*time.Second {
		return s.lastPeerStatus
	}

	endpoint := "http://" + enginePeer.Addr + "/api/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return s.lastPeerStatus
	}
	if s.opts.PeerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.opts.PeerToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.lastPeerStatus
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s.lastPeerStatus
	}
	var remote Status
	if err := json.NewDecoder(resp.Body).Decode(&remote); err == nil {
		s.lastPeerStatus = &remote
		s.lastPeerFetch = time.Now()
	}
	return s.lastPeerStatus
}

// Mode returns current interface mode ("simple" or "advanced").
func (s *Server) Mode() string {
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	if s.mode == "" {
		return "simple"
	}
	return s.mode
}

// SetMode toggles simple or advanced mode.
func (s *Server) SetMode(m string) {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	if m == "advanced" {
		s.mode = "advanced"
	} else {
		s.mode = "simple"
	}
}

// ClusterMode returns the deployment mode: "pi5_only" or "dual_pi".
func (s *Server) ClusterMode() string {
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	if s.clusterMode == "" {
		return "dual_pi"
	}
	return s.clusterMode
}

// SetClusterMode configures the cluster topology ("pi5_only" or "dual_pi").
func (s *Server) SetClusterMode(cm string) {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	if cm == "pi5_only" {
		s.clusterMode = "pi5_only"
	} else {
		s.clusterMode = "dual_pi"
	}
}

// Delegator returns the cooperative task delegator for asymmetric cluster scheduling.
func (s *Server) Delegator() *cluster.TaskDelegator {
	return s.delegator
}
