package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piquant-live/internal/discovery"
	"piquant-live/internal/secrets"
	"piquant-live/internal/telemetry"
)

// A Pi 3B must default to the dashboard, a Pi 5 to the engine, and a
// workstation to something useful. A wrong default is silent at runtime, so it
// is pinned here.
func TestChooseRole(t *testing.T) {
	for _, tc := range []struct {
		forced string
		host   telemetry.Host
		want   discovery.Role
	}{
		{"", telemetry.Host{Role: telemetry.RolePi3, Arch: "arm"}, discovery.RoleDashboard},
		{"", telemetry.Host{Role: telemetry.RolePi5, Arch: "arm64"}, discovery.RoleEngine},
		{"", telemetry.Host{Role: telemetry.RolePi4, Arch: "arm64"}, discovery.RoleEngine},
		{"", telemetry.Host{Role: telemetry.RoleDev, Arch: "amd64"}, discovery.RoleEngine},
		{"dashboard", telemetry.Host{Role: telemetry.RolePi5}, discovery.RoleDashboard},
		{"agent", telemetry.Host{Role: telemetry.RolePi3}, discovery.RoleEngine},
		{"Pi3", telemetry.Host{Role: telemetry.RolePi5}, discovery.RoleDashboard},
		{"engine", telemetry.Host{Role: telemetry.RolePi3}, discovery.RoleEngine},
		{"nonsense", telemetry.Host{Role: telemetry.RolePi3}, discovery.RoleDashboard},
	} {
		if got := chooseRole(tc.forced, tc.host); got != tc.want {
			t.Fatalf("chooseRole(%q, %s) = %q, want %q", tc.forced, tc.host.Role, got, tc.want)
		}
	}
}

func TestParsePort(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{":8080", 8080},
		{"127.0.0.1:9001", 9001},
		{"[::1]:3000", 3000},
		{"", 8080},
		{":http", 8080},
		{":0", 8080},
	} {
		if got := parsePort(tc.in); got != tc.want {
			t.Fatalf("parsePort(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Setup must never overwrite a token it was not asked to replace: re-running it
// would otherwise invalidate the browser session the user just obtained.
func TestSetupKeepsExistingTokensUnlessForced(t *testing.T) {
	runSetupOnce := func(t *testing.T, extra ...string) (dir string) {
		t.Helper()
		dir = t.TempDir()
		args := append([]string{
			"--role", "agent", "--port", "8099", "--no-discovery",
			"--dir", dir, "--data", t.TempDir(),
		}, extra...)
		old := os.Stdin
		os.Stdin, _ = os.Open(os.DevNull)
		defer func() { os.Stdin = old }()
		if err := runSetup(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	dir := runSetupOnce(t)
	store, _, err := secrets.Open(filepath.Join(dir, secrets.FileName))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(secrets.DashboardToken)
	if err != nil || first == "" {
		t.Fatalf("first setup did not store a token: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, secrets.FileName)); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secrets mode = %04o", perm)
	}

	runSetupOnce(t, "--dir", dir)
	store2, _, err := secrets.Open(filepath.Join(dir, secrets.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := store2.Get(secrets.DashboardToken); second != first {
		t.Fatal("a second setup replaced the dashboard token")
	}

	runSetupOnce(t, "--dir", dir, "--force")
	store3, _, err := secrets.Open(filepath.Join(dir, secrets.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if forced, _ := store3.Get(secrets.DashboardToken); forced == first {
		t.Fatal("--force did not replace the token")
	}
}

func TestConfirmAndReadLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		def  bool
		want bool
	}{{"\n", true, true}, {"y\n", false, true}, {"\n", false, false}, {"yes\n", false, true}, {"n\n", true, false}} {
		if got := confirm(bufio.NewReader(strings.NewReader(tc.in)), "ok?", tc.def); got != tc.want {
			t.Fatalf("confirm(%q, def=%v) = %v", tc.in, tc.def, got)
		}
	}
}

func TestSecretsCLI(t *testing.T) {
	dir := t.TempDir()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	capture := func() string {
		w.Close()
		os.Stdout = old
		var buf bytes.Buffer
		buf.ReadFrom(r)
		return buf.String()
	}

	if err := runSecrets([]string{"--dir", dir, "set", "dashboard_token", "abc123"}); err != nil {
		t.Fatal(err)
	}
	if out := capture(); !strings.Contains(out, "stored dashboard_token") {
		t.Fatalf("set output = %q", out)
	}

	r2, w2, _ := os.Pipe()
	os.Stdout = w2
	if err := runSecrets([]string{"--dir", dir, "get", "dashboard_token"}); err != nil {
		t.Fatal(err)
	}
	w2.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r2)
	if got := strings.TrimSpace(buf.String()); got != "abc123" {
		t.Fatalf("get = %q", got)
	}

	if err := runSecrets([]string{"--dir", dir, "get", "missing"}); err == nil {
		t.Fatal("get of a missing name succeeded")
	}
	if err := runSecrets([]string{"--dir", dir, "bogus"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}

// The agent's reported state must stop claiming to be running once it has not
// been updated, so a wedged pipeline is visible rather than frozen-green.
func TestEngineStateGoesStale(t *testing.T) {
	e := newEngineState()
	if e.Snapshot().Running {
		t.Fatal("a fresh agent reports a running pipeline")
	}
	e.Set("replay", 0.4, 1000, 3, "replaying fixture")
	st := e.Snapshot()
	if !st.Running || st.Phase != "replay" || st.Events != 1000 || st.Rejects != 3 {
		t.Fatalf("state = %+v", st)
	}
	e.mu.Lock()
	e.st.Updated = st.Updated.Add(-2 * time.Minute)
	e.mu.Unlock()
	if got := e.Snapshot(); got.Running {
		t.Fatal("a state that has not been updated for two minutes still reports running")
	}
	e.Idle("done")
	if e.Snapshot().Running {
		t.Fatal("Idle left the state running")
	}
}
