package dashboard

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"piquant-live/internal/secrets"
)

// sessionStore holds the browser sessions the dashboard has issued.
//
// A session is a random 256-bit id in an HttpOnly cookie, mapped to an expiry.
// Nothing else is in the cookie, so there is no token to decode, forge or leak
// through a URL. Sessions live in memory only: restarting the dashboard logs
// every browser out, which is the correct trade for a monitoring panel on a
// home LAN.
type sessionStore struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[string]time.Time // id -> expiry
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: make(map[string]time.Time)}
}

// issue creates a session and returns its id.
func (s *sessionStore) issue(now time.Time) (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	// Opportunistic cleanup keeps the map bounded without a goroutine.
	if len(s.m) > 64 {
		for k, exp := range s.m {
			if now.After(exp) {
				delete(s.m, k)
			}
		}
	}
	s.m[id] = now.Add(s.ttl)
	return id, nil
}

// valid reports whether id is a live session.
func (s *sessionStore) valid(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.m, id)
		return false
	}
	return true
}

// drop removes one session (logout).
func (s *sessionStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// loginLimiter throttles failed logins per client address. Without it a 44-byte
// token is still brute-forceable by a LAN attacker at HTTP speed; with it, five
// guesses per 15 minutes make that hopeless while never locking out a user who
// mistyped once.
type loginLimiter struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	m     map[string]*attempts
	now   func() time.Time
	sweep time.Time
}

type attempts struct {
	count int
	until time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, m: make(map[string]*attempts), now: time.Now}
}

// allow reports whether ip may attempt a login now.
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.m[ip]
	if a == nil {
		return true
	}
	if now.After(a.until) {
		delete(l.m, ip)
		return true
	}
	return a.count < l.max
}

// fail records a failed attempt.
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) > 256 {
		for k, v := range l.m {
			if now.After(v.until) {
				delete(l.m, k)
			}
		}
	}
	a := l.m[ip]
	if a == nil || now.After(a.until) {
		a = &attempts{until: now.Add(l.window)}
		l.m[ip] = a
	}
	a.count++
}

// succeed clears the counter for ip.
func (l *loginLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

// retryAfter reports how long ip must wait.
func (l *loginLimiter) retryAfter(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[ip]
	if a == nil {
		return 0
	}
	if d := a.until.Sub(l.now()); d > 0 {
		return d
	}
	return 0
}

// authKind says which credential a request used.
type authKind int

const (
	authNone authKind = iota
	authSession
	authPeer
)

// authenticate identifies the request. A peer token is accepted for the
// machine-facing endpoints; the browser interface needs a session.
func (s *Server) authenticate(r *http.Request) authKind {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if secrets.Equal(s.opts.PeerToken, strings.TrimSpace(h[len("Bearer "):])) {
			return authPeer
		}
		return authNone
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if s.sess.valid(c.Value, s.opts.Now()) {
			return authSession
		}
	}
	// The health endpoint is also readable with the peer token in a query
	// string, so a shell one-liner can check a board without a header.
	if secrets.Equal(s.opts.PeerToken, r.URL.Query().Get("token")) {
		return authPeer
	}
	return authNone
}

// sessionCookie is the browser session cookie name.
const sessionCookie = "piquant_session"

// clientIP is the address used for rate limiting. It prefers the peer address
// and only trusts X-Forwarded-For when a reverse proxy is explicitly allowed,
// so a direct client cannot pick its own bucket.
func clientIP(trustProxy bool, r *http.Request) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				xff = xff[:i]
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withHeaders adds the security headers every response carries: no framing, no
// MIME sniffing, no referrer, and a CSP that allows only the page's own inline
// stylesheet and script block.
func (s *Server) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
				"img-src 'self' data:; manifest-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}

// withLogging writes one access line per request, through the redactor so a
// token in a query string never reaches the log.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.opts.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		line := s.redact(r.Method + " " + r.URL.Path + " " + r.URL.RawQuery)
		s.logger.Printf("%s %d %s", line, rec.code, s.opts.Now().Sub(start).Round(time.Microsecond))
	})
}

func (s *Server) redact(text string) string {
	if s.opts.Keys == nil {
		return text
	}
	return s.opts.Keys.Redact(text)
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE works through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
