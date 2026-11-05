package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piquant-live/internal/discovery"
	"piquant-live/internal/secrets"
)

const testToken = "test-dashboard-token"
const peerToken = "test-peer-token"

func newTestServer(t *testing.T, mod func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Token:     testToken,
		PeerToken: peerToken,
		Addr:      "127.0.0.1:0",
		DataDirs:  []string{t.TempDir()},
		Role:      discovery.RoleDashboard,
		App:       "test",
	}
	if mod != nil {
		mod(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, h http.Handler, method, path string, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "192.168.1.55:4444"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New started without a dashboard token")
	}
}

// Health is the only endpoint a peer may call without a token, and it must not
// disclose anything but liveness.
func TestHealthIsMinimal(t *testing.T) {
	s := newTestServer(t, nil)
	rec := get(t, s.Handler(), http.MethodGet, "/api/health", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["role"] != "dashboard" {
		t.Fatalf("health body = %v", body)
	}
	for _, forbidden := range []string{"host", "proc", "peers", "jobs", "token", "instance"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("health disclosed %q", forbidden)
		}
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	if rec := get(t, h, http.MethodGet, "/api/status", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
	if rec := get(t, h, http.MethodGet, "/api/status", map[string]string{"Authorization": "Bearer wrong"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer = %d", rec.Code)
	}
	rec := get(t, h, http.MethodGet, "/api/status", map[string]string{"Authorization": "Bearer " + peerToken}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("peer bearer = %d", rec.Code)
	}
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Role != discovery.RoleDashboard || st.Host.CPUs == 0 || st.V != Version {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Checks) == 0 {
		t.Fatal("status has no setup checks")
	}
}

// A browser session must be issued only for the dashboard token, and must not
// be issued for the peer token.
func TestLoginFlow(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()

	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login?token="+url.QueryEscape(token), nil)
		req.RemoteAddr = "192.168.1.60:1000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	bad := post("wrong")
	if bad.Code != http.StatusSeeOther || !strings.Contains(bad.Header().Get("Location"), "error=1") {
		t.Fatalf("bad login = %d %q", bad.Code, bad.Header().Get("Location"))
	}
	if c := bad.Result().Cookies(); len(c) != 0 {
		t.Fatalf("bad login set a cookie: %v", c)
	}

	good := post(testToken)
	if good.Code != http.StatusSeeOther {
		t.Fatalf("good login = %d", good.Code)
	}
	cookies := good.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !cookies[0].HttpOnly {
		t.Fatalf("login cookies = %v", cookies)
	}
	if cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie SameSite = %v", cookies[0].SameSite)
	}

	rec := get(t, h, http.MethodGet, "/api/status", nil, cookies[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("status with session = %d", rec.Code)
	}

	// Logout invalidates the session immediately.
	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	req.AddCookie(cookies[0])
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if rec := get(t, h, http.MethodGet, "/api/status", nil, cookies[0]); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d", rec.Code)
	}
}

// The peer token must not open the browser interface, and the dashboard token
// must not authenticate a machine call.
func TestTokensAreNotInterchangeable(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	if rec := get(t, h, http.MethodGet, "/api/status", map[string]string{"Authorization": "Bearer " + testToken}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard token accepted as a peer: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/login?token="+url.QueryEscape(peerToken), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if c := rec.Result().Cookies(); len(c) != 0 {
		t.Fatal("peer token minted a browser session")
	}
}

func TestLoginRateLimited(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/login?token=guess", nil)
		req.RemoteAddr = "10.9.9.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 5; i++ {
		if code := post(); code != http.StatusSeeOther {
			t.Fatalf("attempt %d = %d", i, code)
		}
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt = %d, want 429", code)
	}
	// A different address is unaffected.
	req := httptest.NewRequest(http.MethodPost, "/login?token=guess", nil)
	req.RemoteAddr = "10.9.9.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("other address = %d", rec.Code)
	}
}

// The log viewer must only serve files it was configured with, must redact
// secrets, and must not be usable to read arbitrary paths.
func TestLogViewer(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "piquant.log")
	body := strings.Join([]string{
		"2026-11-02 INFO engine started",
		"2026-11-02 WARN dataset timestamp gap detected",
		"2026-11-02 ERROR connect failed with token tok-abcdef",
		"2026-11-02 INFO replay finished",
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := t.TempDir()
	store, _, err := secrets.Open(filepath.Join(secretsDir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("test_token", "tok-abcdef"); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) {
		o.LogFiles = []string{logPath}
		o.Keys = store
	})
	h := s.Handler()
	auth := map[string]string{"Authorization": "Bearer " + peerToken}

	rec := get(t, h, http.MethodGet, "/api/logs?tail=10", auth, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tok-abcdef") {
		t.Fatal("log response leaked a stored secret")
	}
	var resp LogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Matched != 4 || len(resp.Lines) != 4 || resp.Scanned != 4 {
		t.Fatalf("logs = %+v", resp)
	}
	if resp.Lines[2].Level != "ERROR" || resp.Lines[1].Level != "WARN" {
		t.Fatalf("levels = %q %q", resp.Lines[1].Level, resp.Lines[2].Level)
	}

	// Filtering by level and text.
	rec = get(t, h, http.MethodGet, "/api/logs?tail=10&level=WARN", auth, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Matched != 1 || resp.Lines[0].Level != "WARN" {
		t.Fatalf("level filter = %+v", resp)
	}
	rec = get(t, h, http.MethodGet, "/api/logs?tail=1&q=replay", auth, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Lines) != 1 || !strings.Contains(resp.Lines[0].Text, "replay finished") {
		t.Fatalf("text+tail filter = %+v", resp)
	}

	// A path outside the configured list is refused, including traversal.
	for _, name := range []string{"../etc/passwd", "/etc/passwd", "secrets"} {
		rec := get(t, h, http.MethodGet, "/api/logs?file="+url.QueryEscape(name), auth, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("file %q = %d, want 400", name, rec.Code)
		}
	}
	if rec := get(t, h, http.MethodGet, "/api/logs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated logs = %d", rec.Code)
	}
}

func TestLogLineLevel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-11-02T10:00:00Z level=warn msg", "WARN"},
		{"[ERROR] boom", "ERROR"},
		{"2026 INFO started", "INFO"},
		{"WARN: slow", "WARN"},
		{"plain text", ""},
	} {
		if got := lineLevel(tc.in); got != tc.want {
			t.Fatalf("lineLevel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIndexServesLoginThenPanel(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	rec := get(t, h, http.MethodGet, "/", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Dashboard token") {
		t.Fatalf("index without a session = %d", rec.Code)
	}
	if rec := get(t, h, http.MethodGet, "/nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d", rec.Code)
	}
	// The panel itself is a constant, so check it is served and names no
	// external origin.
	for _, bad := range []string{"http://", "https://", "cdn.", "//unpkg"} {
		if strings.Contains(indexHTML, bad) {
			t.Fatalf("panel references an external origin (%q)", bad)
		}
	}
	if !strings.Contains(indexHTML, "/api/events") || !strings.Contains(indexHTML, "/api/logs") {
		t.Fatal("panel does not use the streaming and log endpoints")
	}
}

func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t, nil)
	rec := get(t, s.Handler(), http.MethodGet, "/api/health", nil, nil)
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Fatalf("missing header %s", h)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "script-src 'self' 'unsafe-eval'") {
		t.Fatalf("unexpected CSP %q", csp)
	}
}

// Status must reflect a peer learned by discovery and mark it live only when the
// peer's health endpoint answers.
func TestStatusIncludesProbedPeer(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+peerToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer peerSrv.Close()
	u, _ := url.Parse(peerSrv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	br := &discovery.Browser{TTL: time.Minute}
	beacon := discovery.Beacon{
		V: discovery.Version, Role: discovery.RoleEngine, ID: "peer-1", Name: "piquant-rpi5",
		Port: port, Addr: host,
	}
	// Seed the peer through the public API by sending it over a socket, so the
	// test does not depend on unexported internals.
	send, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	br.Conn = recv
	br.Token = peerToken
	if err := br.Listen(); err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	payload, err := discovery.Encode(beacon, peerToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := send.WriteToUDP(payload, recv.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, ok := br.Best(discovery.RoleEngine); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s := newTestServer(t, func(o *Options) { o.Discover = br })
	rec := get(t, s.Handler(), http.MethodGet, "/api/status", map[string]string{"Authorization": "Bearer " + peerToken}, nil)
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Peers) != 1 {
		t.Fatalf("peers = %+v", st.Peers)
	}
	p := st.Peers[0]
	if p.Role != discovery.RoleEngine || !p.Auth || !p.Live || p.Error != "" {
		t.Fatalf("peer = %+v", p)
	}
	var found bool
	for _, c := range st.Checks {
		if c.Name == "research engine reachable" && c.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("checks do not report a reachable engine: %+v", st.Checks)
	}
}

// A peer that broadcasts but does not answer health must not be shown as live.
func TestBeaconOnlyPeerIsNotLive(t *testing.T) {
	br := &discovery.Browser{TTL: time.Minute}
	beacon := discovery.Beacon{V: discovery.Version, Role: discovery.RoleEngine, ID: "dead", Name: "gone", Port: 9, Addr: "127.0.0.1"}
	send, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer send.Close()
	recv, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	br.Conn = recv
	if err := br.Listen(); err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	payload, _ := discovery.Encode(beacon, "")
	send.WriteToUDP(payload, recv.LocalAddr().(*net.UDPAddr))
	for i := 0; i < 100; i++ {
		if _, ok := br.Best(discovery.RoleEngine); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := newTestServer(t, func(o *Options) {
		o.Discover = br
		o.ProbeTimeout = 300 * time.Millisecond
	})
	st := s.Status(context.Background())
	if len(st.Peers) != 1 {
		t.Fatalf("peers = %+v", st.Peers)
	}
	if st.Peers[0].Live || st.Peers[0].Error == "" {
		t.Fatalf("beacon-only peer reported live: %+v", st.Peers[0])
	}
}

// The SSE stream must deliver a status event promptly and end when the client
// goes away.
func TestEventStream(t *testing.T) {
	s := newTestServer(t, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	// Log in first so the cookie is in the jar.
	resp, err := client.PostForm(srv.URL+"/login", url.Values{"token": {testToken}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	stream, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if ct := stream.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	br := bufio.NewReader(stream.Body)
	var first, data string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early after %q: %v", first, err)
		}
		if line == "\n" {
			break
		}
		switch {
		case first == "" && strings.HasPrefix(line, "event: "):
			first = strings.TrimRight(line, "\r\n")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "data: ")
		}
	}
	if first != "event: status" {
		t.Fatalf("first event = %q", first)
	}
	var st Status
	if err := json.Unmarshal([]byte(data), &st); err != nil {
		t.Fatalf("stream data is not a status document: %v", err)
	}
	if st.V != Version || st.Host.CPUs == 0 {
		t.Fatalf("streamed status = %+v", st)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{{0, "0 B"}, {512, "512 B"}, {1024, "1.0 KiB"}, {8 << 30, "8.0 GiB"}} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:5555"
	if got := clientIP(false, req); got != "10.0.0.5" {
		t.Fatalf("clientIP = %q", got)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(false, req); got != "10.0.0.5" {
		t.Fatalf("clientIP trusted a spoofed header: %q", got)
	}
	if got := clientIP(true, req); got != "203.0.113.9" {
		t.Fatalf("clientIP with a proxy = %q", got)
	}
}
