package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"piquant-live/internal/discovery"
	"piquant-live/internal/secrets"
	"piquant-live/internal/telemetry"
)

// runSecrets is the non-interactive credential CLI: `piquant secrets set/get/
// list/delete`. `get` prints a value on purpose (the user asked for it and is at
// the console); every other command prints names only.
func runSecrets(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: piquant secrets list | set NAME | get NAME | delete NAME | path")
	}
	fs := flag.NewFlagSet("secrets", flag.ContinueOnError)
	dir := fs.String("dir", "", "secrets directory (default ~/.config/piquant)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: piquant secrets list | set NAME | get NAME | delete NAME | path")
	}
	store, warning, err := secrets.Open(pathFor(*dir))
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	switch rest[0] {
	case "list", "ls":
		names := store.Names()
		if len(names) == 0 {
			fmt.Println("no secrets stored")
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
	case "path":
		fmt.Println(store.Path())
	case "get":
		if len(rest) < 2 {
			return fmt.Errorf("usage: piquant secrets get NAME")
		}
		v, err := store.Get(rest[1])
		if err != nil {
			return err
		}
		fmt.Println(v)
	case "set":
		if len(rest) < 2 {
			return fmt.Errorf("usage: piquant secrets set NAME [VALUE]")
		}
		value := ""
		if len(rest) >= 3 {
			value = rest[2]
		} else {
			// No value on the command line keeps it out of the shell history.
			fmt.Fprintf(os.Stderr, "value for %s: ", rest[1])
			line, err := readLineSecrets()
			if err != nil {
				return err
			}
			value = line
		}
		if err := store.Set(rest[1], value); err != nil {
			return err
		}
		fmt.Printf("stored %s\n", rest[1])
	case "delete", "rm":
		if len(rest) < 2 {
			return fmt.Errorf("usage: piquant secrets delete NAME")
		}
		if err := store.Delete(rest[1]); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", rest[1])
	case "rotate":
		token, err := secrets.NewToken()
		if err != nil {
			return err
		}
		name := secrets.DashboardToken
		if len(rest) > 1 {
			name = rest[1]
		}
		if err := store.Set(name, token); err != nil {
			return err
		}
		fmt.Printf("%s\n", token)
	default:
		return fmt.Errorf("unknown secrets command %q", rest[0])
	}
	return nil
}

// runDoctor checks the board read-only and prints a report an operator can act
// on. It never changes anything and exits non-zero only when a problem would
// stop the application from working.
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := fs.String("secrets-dir", "", "secrets directory (default ~/.config/piquant)")
	wait := fs.Duration("wait", 6*time.Second, "how long to search for the peer board")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var problems, warnings int
	line := func(ok bool, name, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Printf("  [%s] %-26s %s\n", mark, name, detail)
	}
	h := telemetry.New(defaultDataDir()).Collect().Host
	fmt.Printf("\nPiQuant doctor\n\n")
	fmt.Printf("  board       %s (%s)\n  arch        %s/%s\n  kernel      %s\n\n",
		h.Hostname, modelOf(h), "linux", h.Arch, h.Kernel)

	fmt.Printf("  This board\n")
	switch h.Role {
	case telemetry.RolePi5:
		fmt.Printf("  [note] detected Raspberry Pi 5: research engine role\n")
	case telemetry.RolePi3:
		fmt.Printf("  [note] detected Raspberry Pi 3B: dashboard role\n")
	case telemetry.RolePi4:
		fmt.Printf("  [note] detected Raspberry Pi 4: supported as an engine, not benchmarked\n")
	default:
		fmt.Printf("  [note] not a Raspberry Pi: development host\n")
	}
	line(h.CPUs >= 4, "cores", fmt.Sprintf("%d", h.CPUs))
	if h.MemTotal > 0 {
		free := float64(h.MemAvailable) / float64(h.MemTotal)
		line(free > 0.10, "memory", fmt.Sprintf("%s of %s available",
			gb(h.MemAvailable), gb(h.MemTotal)))
		if h.Role == telemetry.RolePi3 && !freeSmallOK(free) {
			fmt.Printf("  [note] the Pi 3B has 1 GB; close other services before a run\n")
		}
	}
	if h.TempOK {
		line(h.TempC.Value() < 80, "temperature", fmt.Sprintf("%.1f degC", h.TempC.Value()))
	} else {
		fmt.Printf("  [skip] no temperature sensor on this board\n")
	}
	if th := h.Throttle; th.Available {
		line(!th.UnderVoltage, "power", pick(th.UnderVoltage, "under-voltage detected: use the official supply", "healthy"))
		line(!th.Throttled, "throttling", pick(th.Throttled, "the board is throttling now", "none"))
	}
	for _, d := range h.Disks {
		line(d.UsedPercent < 90, "disk "+d.Path,
			fmt.Sprintf("%.0f%% used, %s free of %s", d.UsedPercent, gb(d.FreeBytes), gb(d.TotalBytes)))
	}

	fmt.Printf("\n  Configuration\n")
	store, warning, err := secrets.Open(pathFor(*dir))
	if err != nil {
		problems++
		fmt.Printf("  [FAIL] secrets store          %v\n", err)
		fmt.Printf("\n  %d problem(s), %d warning(s)\n\n", problems, warnings)
		return fmt.Errorf("doctor found %d problem(s)", problems)
	}
	if warning != "" {
		warnings++
		fmt.Printf("  [warn] %s\n", warning)
	}
	fmt.Printf("  [ok  ] %-26s %s\n", "secrets store", store.Path())
	line(store.Has(secrets.DashboardToken), "dashboard token",
		pick(!store.Has(secrets.DashboardToken), "missing: run `piquant setup`", "present"))
	line(store.Has(secrets.PeerToken), "peer token",
		pick(!store.Has(secrets.PeerToken), "missing: peers cannot authenticate", "present"))
	if !store.Has(secrets.PeerToken) {
		warnings++
	}

	fmt.Printf("\n  Network\n")
	addrs := discovery.LocalAddrs()
	line(len(addrs) > 0, "local addresses", strings.Join(addrs, ", "))
	if len(addrs) == 0 {
		fmt.Printf("  [note] no Ethernet address found; connect the board to the switch\n")
	}

	// Peer discovery is the part most likely to be misconfigured, so it is
	// checked concretely: listen, wait, and report what was heard.
	if *wait > 0 {
		b := &discovery.Browser{Token: storeToken(store, secrets.PeerToken), TTL: 3 * time.Second}
		if err := b.Listen(); err != nil {
			warnings++
			fmt.Printf("  [warn] discovery listener     %v\n", err)
		} else {
			defer b.Close()
			fmt.Printf("  [..] searching for a peer for %s\n", wait.String())
			deadline := time.Now().Add(*wait)
			var seen []discovery.Peer
			for time.Now().Before(deadline) {
				if peers := b.Peers(); len(peers) > 0 {
					seen = peers
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if len(seen) == 0 {
				warnings++
				fmt.Printf("  [warn] no peer heard: start the other board with the same peer token\n")
			}
			for _, p := range seen {
				fmt.Printf("  [ok  ] peer %-17s %s  %s  (%s)\n",
					p.Role, p.Name, p.Endpoint(), pick(p.Auth, "verified", "unauthenticated"))
			}
		}
	}

	fmt.Printf("\n  %d problem(s), %d warning(s)\n", problems, warnings)
	if problems > 0 {
		return fmt.Errorf("doctor found %d problem(s)", problems)
	}
	fmt.Printf("  This board can run the PiQuant services.\n\n")
	return nil
}

func readLineSecrets() (string, error) {
	var buf [4096]byte
	n, err := os.Stdin.Read(buf[:])
	if err != nil && n == 0 {
		return "", err
	}
	return strings.TrimRight(string(buf[:n]), "\r\n"), nil
}

func gb(n uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

func freeSmallOK(free float64) bool { return free > 0.10 }

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// sortStrings is used by tests to compare listings.
func sortStrings(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}
