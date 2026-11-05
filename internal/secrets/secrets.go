// Package secrets stores the small number of optional local credentials the
// deployment needs, and nothing else.
//
// # What is stored
//
// PiQuant's research engine needs no credential at all: market data is public,
// the replay reads local files, and no order is ever submitted. What a
// deployment does need is:
//
//	dashboard_token   the token that protects the Pi 3B dashboard
//	peer_token        the shared secret the Pi 5 and Pi 3B use to recognise
//	                  each other when auto-discovery is enabled
//
// The store is deliberately generic (any name/value pair may be held) but the
// setup flow only asks for those two. Exchange trading credentials are out of
// scope by design: this application must never connect to an exchange account,
// and a trade-only key stored here could not be used by anything in the tree.
//
// # Safety properties
//
//   - The file lives outside the repository (default ~/.config/piquant/secrets)
//     and is written with mode 0600. Open refuses a path inside a git work tree
//     so a key cannot be committed by accident.
//   - Writes are atomic: a temp file in the same directory, fsync, rename. A
//     power cut leaves the previous file intact, which matters on a microSD.
//   - Values never appear in an error message, in String, or in a log line.
//     Redact replaces every stored value in a string with a fixed marker, and
//     Redactor is what the CLI and the dashboard pass log text through.
//   - Values are held in memory only while the process runs them; nothing here
//     writes a copy anywhere else.
package secrets

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FileName is the store's file name inside the configuration directory.
const FileName = "secrets"

// Marker replaces a secret value in redacted text.
const Marker = "[redacted]"

// Common secret names used by the application.
const (
	DashboardToken = "dashboard_token"
	PeerToken      = "peer_token"
)

// maxValue bounds one stored value. Tokens are 44 bytes; the limit leaves room
// for a future certificate without letting the file become a dumping ground.
const maxValue = 4096

// ErrNotFound is returned by Get for a name that is not stored.
var ErrNotFound = errors.New("secrets: not found")

// ErrUnsafePath is returned by Open for a path inside a git work tree.
var ErrUnsafePath = errors.New("secrets: refusing to use a path inside a git working tree")

// Store is one secrets file. It is safe for concurrent use.
type Store struct {
	path string

	mu     sync.RWMutex
	values map[string]string
	mode   os.FileMode
}

// DefaultDir returns the directory the store uses when none is given:
// $PIQUANT_SECRETS_DIR, else $XDG_CONFIG_HOME/piquant, else ~/.config/piquant.
// The environment override exists so systemd units and tests can point at a
// private directory without changing the binary.
func DefaultDir() (string, error) {
	if d := os.Getenv("PIQUANT_SECRETS_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "piquant"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("secrets: cannot locate a home directory: %w", err)
	}
	return filepath.Join(home, ".config", "piquant"), nil
}

// DefaultPath returns the file path Open uses for dir. An empty dir selects
// DefaultDir.
func DefaultPath(dir string) (string, error) {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	return filepath.Join(dir, FileName), nil
}

// Open reads (or creates) the store at path. A missing file is not an error:
// an unconfigured deployment starts with an empty store and the setup flow
// fills it.
//
// A file with permissions wider than 0600 is tightened and reported through the
// returned warning, never silently accepted.
func Open(path string) (*Store, string, error) {
	if path == "" {
		var err error
		if path, err = DefaultPath(""); err != nil {
			return nil, "", err
		}
	}
	if err := checkNotInGitTree(path); err != nil {
		return nil, "", err
	}
	s := &Store{path: path, values: make(map[string]string), mode: 0o600}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, "", nil
	case err != nil:
		return nil, "", fmt.Errorf("secrets: cannot stat the store: %w", err)
	}
	var warning string
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, "", fmt.Errorf("secrets: %s is readable by other users and could not be tightened: %w", path, err)
		}
		warning = fmt.Sprintf("secrets: %s had mode %04o; it has been changed to 0600", path, perm)
	}
	if err := s.load(); err != nil {
		return nil, "", err
	}
	return s, warning, nil
}

// Path returns the file the store reads and writes.
func (s *Store) Path() string { return s.path }

// load parses the file. A malformed line is an error: silently skipping it
// would turn a corrupted token into a mystery authentication failure.
func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("secrets: cannot read the store: %w", err)
	}
	vals := make(map[string]string)
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("secrets: %s line %d is not name=value", s.path, i+1)
		}
		name = strings.TrimSpace(name)
		if err := ValidName(name); err != nil {
			return fmt.Errorf("secrets: %s line %d: %w", s.path, i+1, err)
		}
		if _, dup := vals[name]; dup {
			return fmt.Errorf("secrets: %s line %d repeats %q", s.path, i+1, name)
		}
		vals[name] = value
	}
	s.values = vals
	return nil
}

// Get returns a stored value.
func (s *Store) Get(name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[name]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Has reports whether name is stored.
func (s *Store) Has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.values[name]
	return ok
}

// Names lists the stored names, sorted. Values are never returned by a listing.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.values))
	for k := range s.values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set stores name=value and rewrites the file atomically.
func (s *Store) Set(name, value string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.values[name]
	s.values[name] = value
	if err := s.saveLocked(); err != nil {
		// Restore the in-memory view so it agrees with the file on disk.
		if had {
			s.values[name] = prev
		} else {
			delete(s.values, name)
		}
		return err
	}
	return nil
}

// Delete removes a name. Removing a missing name is not an error.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[name]; !ok {
		return nil
	}
	delete(s.values, name)
	return s.saveLocked()
}

// saveLocked writes the store atomically. The caller holds s.mu.
//
// The sequence is: write a sibling temp file with 0600, fsync it, rename over
// the target (atomic on every filesystem the Pi uses), then fsync the
// directory so the rename itself survives a power cut. The directory fsync is
// best effort because some filesystems refuse it; the rename is what matters
// for consistency.
func (s *Store) saveLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secrets: cannot create %s: %w", dir, err)
	}
	var b strings.Builder
	b.WriteString("# PiQuant local secrets. Mode 0600, never commit this file.\n")
	b.WriteString("# Written by `piquant setup`; edit with `piquant secrets set NAME`.\n")
	names := make([]string, 0, len(s.values))
	for k := range s.values {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s.values[k])
		b.WriteByte('\n')
	}

	tmp, err := os.CreateTemp(dir, "."+FileName+".tmp*")
	if err != nil {
		return fmt.Errorf("secrets: cannot create a temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: cannot set permissions: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: cannot write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: cannot flush: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: cannot close: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("secrets: cannot replace %s: %w", s.path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// String describes the store without revealing a single value.
func (s *Store) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("secrets store %s (%d names, mode 0600)", s.path, len(s.values))
}

// Redact replaces every stored value in text with Marker. It is safe on nil and
// on an unopened store; both return the input unchanged.
//
// Longest-first replacement keeps a short value from mangling a longer one that
// contains it.
func (s *Store) Redact(text string) string {
	if s == nil || text == "" {
		return text
	}
	s.mu.RLock()
	vals := make([]string, 0, len(s.values))
	for _, v := range s.values {
		if len(v) >= 4 { // a 1-3 byte value would redact ordinary words
			vals = append(vals, v)
		}
	}
	s.mu.RUnlock()
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		text = strings.ReplaceAll(text, v, Marker)
	}
	return text
}

// Equal compares a presented token with a stored one in constant time.
func Equal(stored, presented string) bool {
	if stored == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(presented)) == 1
}

// NewToken returns a 256-bit URL-safe random token.
func NewToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("secrets: cannot read the system random source: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// ValidName reports whether name can be used as a key.
func ValidName(name string) error {
	if name == "" {
		return errors.New("secrets: name is empty")
	}
	if len(name) > 64 {
		return errors.New("secrets: name is longer than 64 bytes")
	}
	for _, r := range name {
		ok := r == '_' || r == '-' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("secrets: name %q may only contain letters, digits, '_', '-' and '.'", name)
		}
	}
	return nil
}

// validateValue rejects values that would break the one-line format or that
// are obviously not credentials.
func validateValue(v string) error {
	if v == "" {
		return errors.New("secrets: value is empty")
	}
	if len(v) > maxValue {
		return fmt.Errorf("secrets: value is longer than %d bytes", maxValue)
	}
	if strings.ContainsAny(v, "\r\n\x00") {
		return errors.New("secrets: value must be a single line without NUL bytes")
	}
	if strings.TrimSpace(v) != v {
		return errors.New("secrets: value must not start or end with whitespace")
	}
	return nil
}

// checkNotInGitTree refuses a path under a .git directory or a work tree root.
// It walks up at most 32 levels: a secrets file inside a repository is one
// `git add -A` away from being published.
func checkNotInGitTree(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	dir := filepath.Dir(abs)
	for i := 0; i < 32; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("%w: %s", ErrUnsafePath, abs)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}
