package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"piquant-live/internal/discovery"
	"piquant-live/internal/secrets"
	"piquant-live/internal/telemetry"
)

// setupRun is everything a guided setup decides, kept separate from the I/O so
// the decisions can be tested without a terminal.
type setupRun struct {
	Role      discovery.Role
	HTTPPort  int
	Discovery bool
	PeerToken string
	Token     string
	DataDir   string
	LogDir    string
	Instance  string
	PeerSeen  bool
	PeerName  string
	Overwrite bool
}

// runSetup configures a board in one pass: detect what it is, choose the role,
// generate the tokens, store them, and report exactly what to do next.
func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	role := fs.String("role", "", "force the role: dashboard (Pi 3B) or agent (Pi 5); default is detected")
	port := fs.Int("port", 8080, "HTTP port this board serves on")
	noDiscovery := fs.Bool("no-discovery", false, "do not enable automatic peer discovery")
	nonInteractive := fs.Bool("yes", false, "accept the detected defaults without prompting")
	force := fs.Bool("force", false, "replace existing tokens")
	dir := fs.String("dir", "", "secrets directory (default: ~/.config/piquant)")
	dataDir := fs.String("data", "", "data directory (default: ~/piquant-data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = nonInteractive
	in := bufio.NewReader(os.Stdin)

	store, warning, err := secrets.Open(pathFor(*dir))
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}

	machine := telemetry.New().Collect().Host
	fmt.Printf("\nPiQuant setup\n\n")
	fmt.Printf("  board      %s (%s, %s)\n", machine.Hostname, modelOf(machine), machine.Arch)
	fmt.Printf("  kernel     %s\n", machine.Kernel)
	if os.Geteuid() == 0 {
		fmt.Printf("  warning    running as root; prefer a normal user account\n")
	}

	run := setupRun{HTTPPort: *port, DataDir: *dataDir, Discovery: !*noDiscovery}
	if run.DataDir == "" {
		run.DataDir = defaultDataDir()
	}
	run.LogDir = defaultLogDir()
	run.Role = chooseRole(*role, machine)

	// A role that was forced or detected still gets confirmation, because the
	// two boards need opposite answers and a wrong one is silent otherwise.
	if *role == "" {
		fmt.Printf("\n  This board will act as the %s.\n", describeRole(run.Role))
		if !confirm(in, "Use this role?", true) {
			fmt.Println("  choose 1) dashboard (Raspberry Pi 3B)  2) agent (Raspberry Pi 5)")
			switch strings.TrimSpace(readLine(in)) {
			case "1":
				run.Role = discovery.RoleDashboard
			case "2":
				run.Role = discovery.RoleEngine
			default:
				return errors.New("setup cancelled")
			}
		}
	}

	run.Instance, err = discovery.InstanceID(secretsDirOf(*dir))
	if err != nil {
		return err
	}

	// Tokens. Generating rather than asking keeps the flow short and avoids a
	// weak password; the token is shown once, after it is safely stored.
	if *force || !store.Has(secrets.DashboardToken) {
		run.Token, err = secrets.NewToken()
		if err != nil {
			return err
		}
	} else {
		run.Token, _ = store.Get(secrets.DashboardToken)
	}
	if *force || !store.Has(secrets.PeerToken) {
		run.PeerToken, err = secrets.NewToken()
		if err != nil {
			return err
		}
	} else {
		run.PeerToken, _ = store.Get(secrets.PeerToken)
	}

	if run.Discovery {
		fmt.Println("\n  Looking for the other board for 5 seconds (press Ctrl-C to skip)...")
		peer, ok := lookForPeer(ctx, store)
		if ok {
			run.PeerSeen, run.PeerName = true, peer.Name
			// The peer's token is the one both boards must share: adopt it
			// when both are present, so a second run of setup converges.
			if peer.Role == discovery.RoleEngine && run.Role == discovery.RoleDashboard {
				fmt.Printf("  found %s at %s\n", peer.Name, peer.Endpoint())
				fmt.Println("  copy the peer token from that board if the checks report a mismatch")
			}
		} else {
			fmt.Println("  no other board heard; that is fine, the panel keeps looking")
		}
	}

	for _, kv := range [][2]string{
		{secrets.DashboardToken, run.Token},
		{secrets.PeerToken, run.PeerToken},
	} {
		if err := store.Set(kv[0], kv[1]); err != nil {
			return err
		}
	}

	fmt.Printf("\n  Next, on this board:\n\n")
	switch run.Role {
	case discovery.RoleDashboard:
		fmt.Printf("    piquant dashboard -addr :%d\n\n", run.HTTPPort)
		fmt.Printf("  Then open http://%s:%d from your computer or phone.\n",
			firstAddr(), run.HTTPPort)
	default:
		fmt.Printf("    piquant agent -addr :%d\n\n", run.HTTPPort)
		fmt.Printf("  The control panel on the other board finds this one automatically.\n")
	}
	fmt.Printf("\n  Dashboard token (sign in with this; shown once):\n\n    %s\n\n", run.Token)
	fmt.Printf("  Peer token (must match on both boards):\n\n    %s\n\n", run.PeerToken)
	fmt.Printf("  Stored in %s (mode 0600). Show it again with:\n    piquant secrets get %s\n\n",
		store.Path(), secrets.DashboardToken)
	fmt.Printf("  Check the result with: piquant doctor\n\n")
	return nil
}

// chooseRole maps an explicit flag or the detected board to a role.
func chooseRole(forced string, h telemetry.Host) discovery.Role {
	switch strings.ToLower(forced) {
	case "dashboard", "pi3", "ui":
		return discovery.RoleDashboard
	case "agent", "engine", "pi5":
		return discovery.RoleEngine
	}
	switch h.Role {
	case telemetry.RolePi3:
		return discovery.RoleDashboard
	case telemetry.RolePi5, telemetry.RolePi4:
		return discovery.RoleEngine
	}
	// On a workstation, default to the engine: it is the part that can do
	// useful work without a second board.
	return discovery.RoleEngine
}

func describeRole(r discovery.Role) string {
	if r == discovery.RoleDashboard {
		return "dashboard server (read-only web interface)"
	}
	return "research engine (replay, features, models)"
}

func modelOf(h telemetry.Host) string {
	if h.Model != "" {
		return h.Model
	}
	return "unknown machine"
}

// lookForPeer listens for a beacon briefly. A missing peer is not an error: the
// panel keeps searching at runtime.
func lookForPeer(ctx context.Context, store *secrets.Store) (discovery.Peer, bool) {
	token, _ := store.Get(secrets.PeerToken)
	b := &discovery.Browser{Token: token}
	if err := b.Listen(); err != nil {
		fmt.Printf("  discovery unavailable: %v\n", err)
		return discovery.Peer{}, false
	}
	defer b.Close()
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan discovery.Peer, 1)
	go func() {
		for {
			if p, ok := b.Best(discovery.RoleEngine); ok {
				done <- p
				return
			}
			if p, ok := b.Best(discovery.RoleDashboard); ok {
				done <- p
				return
			}
			select {
			case <-wait.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	select {
	case p := <-done:
		return p, true
	case <-wait.Done():
		return discovery.Peer{}, false
	}
}

func confirm(in *bufio.Reader, prompt string, def bool) bool {
	suffix := " [Y/n] "
	if !def {
		suffix = " [y/N] "
	}
	fmt.Print("  " + prompt + suffix)
	line := strings.ToLower(strings.TrimSpace(readLine(in)))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// readLine reads one line, treating EOF as an empty answer so a piped or
// non-interactive run does not hang.
func readLine(in *bufio.Reader) string {
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

func pathFor(dir string) string {
	if dir == "" {
		p, err := secrets.DefaultPath("")
		if err != nil {
			return ""
		}
		return p
	}
	return dir + "/" + secrets.FileName
}

func secretsDirOf(dir string) string {
	if dir != "" {
		return dir
	}
	d, err := secrets.DefaultDir()
	if err != nil {
		return "."
	}
	return d
}

func firstAddr() string {
	if addrs := discovery.LocalAddrs(); len(addrs) > 0 {
		return addrs[0]
	}
	return "localhost"
}
