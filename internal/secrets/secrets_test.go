package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// openTemp opens a store in a directory that is guaranteed not to be inside a
// git work tree, so the safety check does not reject the test itself.
func openTemp(t *testing.T) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "piquant-secrets-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, _, err := Open(filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetPersistsAndReloads(t *testing.T) {
	s := openTemp(t)
	if s.Has(DashboardToken) {
		t.Fatal("empty store already has a token")
	}
	if _, err := s.Get(DashboardToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on an empty store = %v", err)
	}
	if err := s.Set(DashboardToken, "tok-abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(PeerToken, "peer-123456"); err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); len(got) != 2 || got[0] != DashboardToken || got[1] != PeerToken {
		t.Fatalf("Names = %v", got)
	}

	reopened, warning, err := Open(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Fatalf("unexpected warning: %s", warning)
	}
	if got, err := reopened.Get(DashboardToken); err != nil || got != "tok-abcdef" {
		t.Fatalf("reloaded token = %q, %v", got, err)
	}
	if got, err := reopened.Get(PeerToken); err != nil || got != "peer-123456" {
		t.Fatalf("reloaded peer = %q, %v", got, err)
	}
}

func TestFileIsPrivateAndSingleLine(t *testing.T) {
	s := openTemp(t)
	if err := s.Set("a", "value-one"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("mode = %04o, want 0600", perm)
		}
	}
	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Count(line, "=") != 1 || !strings.HasPrefix(line, "a=") {
			t.Fatalf("unexpected line %q", line)
		}
	}
}

// A store with looser permissions must be tightened and reported, not ignored.
func TestLoosePermissionsAreTightened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets")
	if err := os.WriteFile(path, []byte("dashboard_token=abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, warning, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if warning == "" {
		t.Fatal("a world-readable store opened without a warning")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o after Open", info.Mode().Perm())
	}
}

func TestMalformedFileIsRejected(t *testing.T) {
	for _, body := range []string{
		"no equals sign\n",
		"a=1\na=2\n",
		"bad name=1\n",
		"=1\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "secrets")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(path); err == nil {
			t.Fatalf("Open accepted %q", body)
		}
	}
}

func TestRedactHidesEveryValue(t *testing.T) {
	s := openTemp(t)
	mustSet(t, s, DashboardToken, "dashboard-secret-1")
	mustSet(t, s, PeerToken, "peer-secret-2")
	text := "token=dashboard-secret-1 peer=peer-secret-2 other=visible"
	got := s.Redact(text)
	if strings.Contains(got, "dashboard-secret-1") || strings.Contains(got, "peer-secret-2") {
		t.Fatalf("Redact leaked a value: %q", got)
	}
	if !strings.Contains(got, "other=visible") {
		t.Fatalf("Redact removed unrelated text: %q", got)
	}
	if strings.Count(got, Marker) != 2 {
		t.Fatalf("Redact = %q, want two markers", got)
	}
	// A nil store is a no-op, so callers need no nil check.
	var nilStore *Store
	if nilStore.Redact(text) != text {
		t.Fatal("nil store changed the text")
	}
}

// The longest value is replaced first, so a short value inside a long one does
// not leave the long one half redacted.
func TestRedactLongestFirst(t *testing.T) {
	s := openTemp(t)
	mustSet(t, s, "short", "abcdef")
	mustSet(t, s, "long", "abcdefXYZ")
	got := s.Redact("v=abcdefXYZ")
	if strings.Contains(got, "XYZ") || got != "v="+Marker {
		t.Fatalf("Redact = %q", got)
	}
}

func TestStringDoesNotContainValues(t *testing.T) {
	s := openTemp(t)
	mustSet(t, s, DashboardToken, "super-secret-token")
	if out := s.String(); strings.Contains(out, "super-secret-token") {
		t.Fatalf("String leaked the value: %q", out)
	}
}

func TestDelete(t *testing.T) {
	s := openTemp(t)
	mustSet(t, s, DashboardToken, "x1")
	if err := s.Delete(DashboardToken); err != nil {
		t.Fatal(err)
	}
	if s.Has(DashboardToken) {
		t.Fatal("Delete left the name")
	}
	if err := s.Delete("never-existed"); err != nil {
		t.Fatalf("Delete of a missing name = %v", err)
	}
	reopened, _, err := Open(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Has(DashboardToken) {
		t.Fatal("Delete did not reach the file")
	}
}

func TestValidation(t *testing.T) {
	s := openTemp(t)
	for _, name := range []string{"", "has space", "semi;colon", "eq=uals", strings.Repeat("a", 65)} {
		if err := s.Set(name, "v"); err == nil {
			t.Fatalf("Set accepted name %q", name)
		}
	}
	for _, value := range []string{"", " leading", "trailing ", "two\nlines", strings.Repeat("a", maxValue+1)} {
		if err := s.Set("ok", value); err == nil {
			t.Fatalf("Set accepted value %q", value)
		}
	}
	if err := ValidName("ok_name-1.x"); err != nil {
		t.Fatalf("ValidName rejected a good name: %v", err)
	}
}

func TestTokenIsRandomAndUrlSafe(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two tokens are equal")
	}
	if len(a) < 40 {
		t.Fatalf("token is only %d characters", len(a))
	}
	for _, r := range a {
		ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			t.Fatalf("token contains %q", r)
		}
	}
}

func TestEqual(t *testing.T) {
	if !Equal("abc123", "abc123") {
		t.Fatal("Equal rejected identical tokens")
	}
	if Equal("abc123", "abc124") || Equal("", "") || Equal("a", "") {
		t.Fatal("Equal accepted different or empty tokens")
	}
}

// A secrets file inside a git work tree must be refused; that is the one place
// a key could end up in a commit.
func TestRefusesPathInGitTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "sub", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := Open(filepath.Join(nested, "secrets"))
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Open in a git tree = %v, want ErrUnsafePath", err)
	}
}

func mustSet(t *testing.T, s *Store, name, value string) {
	t.Helper()
	if err := s.Set(name, value); err != nil {
		t.Fatal(err)
	}
}
