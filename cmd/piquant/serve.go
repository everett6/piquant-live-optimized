package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/dashboard"
	"piquant-live/internal/discovery"
	"piquant-live/internal/experiment"
	"piquant-live/internal/secrets"
)

// dashboardFlags is shared by the two serving commands.
type serveFlags struct {
	addr       string
	secretsDir string
	dataDir    string
	logDir     string
	noDiscover bool
	app        string
}

func addServeFlags(fs *flag.FlagSet) *serveFlags {
	f := &serveFlags{}
	fs.StringVar(&f.addr, "addr", ":8080", "listen address, e.g. :8080")
	fs.StringVar(&f.secretsDir, "secrets-dir", "", "secrets directory (default ~/.config/piquant)")
	fs.StringVar(&f.dataDir, "data", "", "data directory whose disk usage is reported")
	fs.StringVar(&f.logDir, "log-dir", "", "directory holding the service logs")
	fs.BoolVar(&f.noDiscover, "no-discovery", false, "do not announce or search for the peer board")
	fs.StringVar(&f.app, "app", version, "version string shown in the interface")
	return f
}

// openStore opens the secrets file, failing with an actionable message when the
// board has not been set up. It deliberately does not create tokens: a server
// that silently invented one would leave the operator with no way to log in.
func openStore(dir string) (*secrets.Store, error) {
	store, warning, err := secrets.Open(pathFor(dir))
	if err != nil {
		return nil, err
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	if !store.Has(secrets.DashboardToken) {
		return nil, fmt.Errorf("no dashboard token in %s; run `piquant setup` first", store.Path())
	}
	return store, nil
}

func storeToken(store *secrets.Store, name string) string {
	v, _ := store.Get(name)
	return v
}

// startAnnouncer advertises this board so the other one finds it without an IP
// address. A failure is reported and ignored: discovery is a convenience, not a
// dependency.
func startAnnouncer(role discovery.Role, port int, token, instance, app string) *discovery.Announcer {
	a := &discovery.Announcer{
		Token: token,
		Beacon: discovery.Beacon{
			V: discovery.Version, Role: role, ID: instance, App: app,
			Port: port, StartedAt: time.Now().Unix(),
		},
	}
	if err := a.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: discovery announcement disabled: %v\n", err)
		return nil
	}
	return a
}

// parsePort extracts the TCP port a listen address uses.
func parsePort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 8080
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return 8080
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 8080
	}
	return n
}

// runDashboard serves the control panel (the Pi 3B role).
func runDashboard(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	f := addServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openStore(f.secretsDir)
	if err != nil {
		return err
	}
	instance, _ := discovery.InstanceID(secretsDirOf(f.secretsDir))
	logDir := f.logDir
	if logDir == "" {
		logDir = defaultLogDir()
	}
	logFile, err := openLog(logDir, "dashboard.log")
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	} else {
		defer logFile.Close()
	}

	var browser *discovery.Browser
	if !f.noDiscover {
		b := &discovery.Browser{Token: storeToken(store, secrets.PeerToken), SelfID: instance}
		if err := b.Listen(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: peer discovery disabled: %v\n", err)
		} else {
			browser = b
			defer b.Close()
		}
	}
	port := parsePort(f.addr)
	if a := startAnnouncer(discovery.RoleDashboard, port, storeToken(store, secrets.PeerToken), instance, f.app); a != nil {
		defer a.Stop()
	}

	srv, err := dashboard.New(dashboard.Options{
		Addr:       f.addr,
		Token:      storeToken(store, secrets.DashboardToken),
		PeerToken:  storeToken(store, secrets.PeerToken),
		Keys:       store,
		LogFiles:   logFiles(logDir),
		DataDirs:   dataDirs(f.dataDir),
		Role:       discovery.RoleDashboard,
		App:        f.app,
		Discover:   browser,
		Instance:   instance,
		SessionTTL: 12 * time.Hour,
	})
	if err != nil {
		return err
	}
	fmt.Printf("piquant dashboard %s: http://%s\n", f.app, listenHint(f.addr))
	return srv.ListenAndServe(ctx)
}

// runAgent exposes engine status (the Pi 5 role). It reports the same document
// the panel renders for itself, minus the browser interface.
func runAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	f := addServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openStore(f.secretsDir)
	if err != nil {
		return err
	}
	instance, _ := discovery.InstanceID(secretsDirOf(f.secretsDir))
	logDir := f.logDir
	if logDir == "" {
		logDir = defaultLogDir()
	}
	if lf, err := openLog(logDir, "engine.log"); err == nil {
		defer lf.Close()
	}

	var browser *discovery.Browser
	if !f.noDiscover {
		b := &discovery.Browser{Token: storeToken(store, secrets.PeerToken), SelfID: instance}
		if err := b.Listen(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: peer discovery disabled: %v\n", err)
		} else {
			browser = b
			defer b.Close()
		}
	}
	port := parsePort(f.addr)
	if a := startAnnouncer(discovery.RoleEngine, port, storeToken(store, secrets.PeerToken), instance, f.app); a != nil {
		defer a.Stop()
	}

	dDir := f.dataDir
	if dDir == "" {
		dDir = defaultDataDir()
	}
	_ = os.MkdirAll(dDir, 0o755)
	_ = experiment.GenerateStandardDatasets(dDir)

	cfg, err := config.Load("configs/production.yaml")
	if err != nil {
		cfg, _ = config.Load("configs/dev.yaml")
	}
	if cfg == nil {
		cfg = &config.Config{
			Symbols: []config.Symbol{
				{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"},
				{Name: "ETH", VenueSym: "ETHUSDT", Venue: "binance"},
			},
			Targets: map[string][]string{
				"BTC": {"binance:BTCUSDT"},
				"ETH": {"binance:ETHUSDT"},
			},
		}
	}

	mgr, err := experiment.NewManager(dDir, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot initialize experiment manager: %v\n", err)
	}

	srv, err := dashboard.New(dashboard.Options{
		Addr:      f.addr,
		Token:     storeToken(store, secrets.DashboardToken),
		PeerToken: storeToken(store, secrets.PeerToken),
		Keys:      store,
		LogFiles:  logFiles(logDir),
		DataDirs:  dataDirs(f.dataDir),
		Role:      discovery.RoleEngine,
		App:       f.app,
		Discover:  browser,
		Instance:  instance,
		Manager:   mgr,
	})
	if err != nil {
		return err
	}
	fmt.Printf("piquant agent %s: http://%s\n", f.app, listenHint(f.addr))
	return srv.ListenAndServe(ctx)
}

// engineState is the process-wide view the agent reports. A pipeline calls its
// setters; until then it reports idle.
type engineState struct {
	mu   sync.Mutex
	st   dashboard.EngineState
	jobs []dashboard.Job
}

func newEngineState() *engineState {
	return &engineState{st: dashboard.EngineState{Phase: "idle"}}
}

// Snapshot returns a copy for the status document.
func (e *engineState) Snapshot() dashboard.EngineState {
	e.mu.Lock()
	defer e.mu.Unlock()
	// The status endpoint must never claim a phase is live when nothing has
	// updated it for a minute: a wedged pipeline should look wedged.
	if e.st.Running && !e.st.Updated.IsZero() && time.Since(e.st.Updated) > time.Minute {
		cp := e.st
		cp.Running = false
		cp.Message = "no update for over a minute; the pipeline may be wedged"
		return cp
	}
	return e.st
}

// Set updates the reported state.
func (e *engineState) Set(phase string, progress float64, events, rejects int64, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st = dashboard.EngineState{
		Running: true, Phase: phase, Progress: progress,
		Events: events, Rejects: rejects, Message: message, Updated: time.Now(),
	}
}

// Idle clears the running flag.
func (e *engineState) Idle(message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.Running = false
	e.st.Message = message
	e.st.Updated = time.Now()
}

func logFiles(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{
		dir + "/dashboard.log",
		dir + "/engine.log",
		dir + "/piquant.log",
	}
}

func dataDirs(flagValue string) []string {
	if flagValue != "" {
		return []string{flagValue}
	}
	return []string{defaultDataDir()}
}

func listenHint(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return firstAddr() + addr
	}
	return addr
}
