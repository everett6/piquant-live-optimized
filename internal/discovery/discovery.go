// Package discovery finds the other machine in the two-Pi topology without
// anyone typing an IP address.
//
// # The problem it solves
//
// A deployment is two boards on the same switch: the Pi 5 runs the research
// engine, the Pi 3B serves the dashboard. Asking the user to find the Pi 5's
// address, copy it into a config file on the other board, and repeat that after
// every DHCP lease change is the main source of setup friction. Instead both
// processes broadcast a small beacon on the local network; each learns the
// other's address, role, HTTP port and liveness, and the dashboard configures
// its upstream automatically.
//
// # What a beacon contains
//
// A beacon is one JSON line carrying a role ("engine" or "dashboard"), a stable
// instance id, the host name, the application version, the address and port
// peers should connect to, and a monotonically increasing counter. It carries
// no secret and no market data.
//
// When both boards share a peer token (written by `piquant setup`), every
// beacon is authenticated with HMAC-SHA256 over its payload. A beacon that
// fails the check is ignored, so an unrelated program replying to the port
// cannot make the dashboard point at itself. Without a token, discovery still
// works but the result is marked unauthenticated, and the dashboard shows that
// rather than silently trusting the LAN.
//
// # Scope
//
// The beacon is broadcast only. There is no listening TCP port, no inbound
// connection, and no command that changes the remote machine; discovery only
// answers "where is the other board and is it alive".
package discovery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Role names a participant in the topology.
type Role string

// Roles that announce themselves.
const (
	RoleEngine    Role = "engine"    // Pi 5: replay, features, models, experiments
	RoleDashboard Role = "dashboard" // Pi 3B: read-only web interface
)

// DefaultPort is the UDP port beacons are sent to. It is in the dynamic range
// so it will not collide with a service port.
const DefaultPort = 47821

// DefaultInterval is how often a beacon is repeated. Three missed beacons mark
// a peer as gone, so a peer disappears from the UI within about 15 s of dying.
const DefaultInterval = 5 * time.Second

// FastDiscoveryInterval is for rapid initial startup pairing when 0 peers are known,
// smoothly transitioning to DefaultInterval once paired.
const FastDiscoveryInterval = 500 * time.Millisecond

// DefaultTTL is how long a peer is reported after its last beacon.
const DefaultTTL = 18 * time.Second

// MaxDatagram bounds an accepted beacon. A JSON line is a few hundred bytes;
// anything larger is not ours and is dropped without parsing.
const MaxDatagram = 2048

// Version is the beacon schema version. A peer with another version is reported
// as incompatible rather than silently misread.
const Version = 1

// Beacon is the payload one machine broadcasts.
type Beacon struct {
	V         int    `json:"v"`
	Role      Role   `json:"role"`
	ID        string `json:"id"`           // stable per install, random on first run
	Name      string `json:"name"`         // host name
	App       string `json:"app"`          // application version
	Addr      string `json:"addr"`         // address peers should use
	Port      int    `json:"port"`         // HTTP port for peers
	Seq       uint64 `json:"seq"`          // per-sender counter, detects restarts
	StartedAt int64  `json:"started_at"`   // unix seconds
	SentAt    int64  `json:"sent_at_unix"` // unix seconds, coarse clock check
}

// Validate rejects a beacon that cannot be used.
func (b *Beacon) Validate() error {
	if b.V != Version {
		return fmt.Errorf("discovery: beacon version %d, this build speaks %d", b.V, Version)
	}
	if b.Role != RoleEngine && b.Role != RoleDashboard {
		return fmt.Errorf("discovery: unknown role %q", b.Role)
	}
	if b.ID == "" {
		return errors.New("discovery: beacon without an instance id")
	}
	if b.Port <= 0 || b.Port > 65535 {
		return fmt.Errorf("discovery: beacon port %d out of range", b.Port)
	}
	return nil
}

// Peer is a learned remote machine.
type Peer struct {
	Beacon
	LastSeen time.Time
	SrcIP    string // source address of the datagram, for diagnosis
	Auth     bool   // the beacon carried a valid MAC
}

// Age returns how long ago the peer was last heard from.
func (p Peer) Age(now time.Time) time.Duration { return now.Sub(p.LastSeen) }

// Endpoint returns the base URL peers should talk to.
func (p Peer) Endpoint() string {
	host := p.Addr
	if host == "" {
		host = p.SrcIP
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(p.Port))
}

// Wire format ---------------------------------------------------------------

// Encode renders a beacon. With a non-empty token the payload is followed by a
// space and the hex HMAC; the MAC covers the JSON exactly as sent, so a peer
// verifies the bytes it received rather than a re-encoded copy.
func Encode(b Beacon, token string) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("discovery: cannot encode beacon: %w", err)
	}
	if token == "" {
		return body, nil
	}
	out := make([]byte, 0, len(body)+1+64)
	out = append(out, body...)
	out = append(out, ' ')
	out = append(out, mac(body, token)...)
	return out, nil
}

// Decode parses a datagram. With a non-empty token the MAC must match; a
// datagram without one is an error, not a downgrade, so a token configured on
// one side is never bypassed from the other.
func Decode(data []byte, token string) (Beacon, error) {
	var b Beacon
	if len(data) > MaxDatagram {
		return b, fmt.Errorf("discovery: datagram of %d bytes exceeds %d", len(data), MaxDatagram)
	}
	body := bytesTrimSpace(data)
	if token != "" {
		i := lastSpace(body)
		if i < 0 {
			return b, errors.New("discovery: beacon is not authenticated")
		}
		payload, sum := body[:i], string(body[i+1:])
		if !hmac.Equal([]byte(mac(payload, token)), []byte(sum)) {
			return b, errors.New("discovery: beacon failed authentication")
		}
		body = payload
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return b, fmt.Errorf("discovery: malformed beacon: %w", err)
	}
	if err := b.Validate(); err != nil {
		return b, err
	}
	return b, nil
}

func mac(payload []byte, token string) string {
	h := hmac.New(sha256.New, []byte(token))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

func lastSpace(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ' ' {
			return i
		}
	}
	return -1
}

// Identity ------------------------------------------------------------------

// InstanceID returns a stable random id for this installation, creating it on
// first call. It lives next to the secrets file so both boards keep their id
// across restarts, which lets the dashboard tell "the same Pi 5 came back" from
// "a different Pi 5 joined".
func InstanceID(dir string) (string, error) {
	path := dir + string(os.PathSeparator) + "instance-id"
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("discovery: cannot generate an instance id: %w", err)
	}
	id := hex.EncodeToString(buf[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("discovery: cannot create %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("discovery: cannot write %s: %w", path, err)
	}
	return id, nil
}

// LocalAddrs returns the IPv4 addresses of up interfaces, excluding loopback
// and link-local. The first address is the one a peer on the same LAN will be
// able to reach, which is what a beacon should advertise.
func LocalAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// BroadcastAddrs returns the directed broadcast address of every broadcast
// interface, plus the all-ones broadcast as a fallback. Sending to both means
// discovery works on a plain switch and on a network that filters 255.255.255.255.
func BroadcastAddrs() []string {
	addrs := []string{"255.255.255.255"}
	ifaces, err := net.Interfaces()
	if err != nil {
		return addrs
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 ||
			ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrsIfc, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrsIfc {
			ip, ipnet, err := net.ParseCIDR(a.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			v4 := ip.To4()
			mask := ipnet.Mask
			if len(mask) != 4 {
				continue
			}
			b := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				b[i] = v4[i] | ^mask[i]
			}
			addrs = append(addrs, b.String())
		}
	}
	return addrs
}

// Announcer periodically broadcasts this machine's beacon.
type Announcer struct {
	Beacon      Beacon
	Token       string
	Port        int           // UDP port, 0 = DefaultPort
	Interval    time.Duration // 0 = adaptive (FastDiscoveryInterval until peer confirmed, then DefaultInterval)
	Browser     *Browser      // optional: reference to browser to detect when peers are paired
	PeerChecker func() bool   // optional: returns true when peer is detected

	// Conn, when set, replaces the default connection. Tests use it to pump
	// datagrams through a loopback socket.
	Conn *net.UDPConn

	adaptive bool
	seq      uint64
	once     sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// Start begins announcing. It returns immediately; Stop ends it.
func (a *Announcer) Start() error {
	if err := a.Beacon.Validate(); err != nil {
		return err
	}
	if a.Port == 0 {
		a.Port = DefaultPort
	}
	if a.Interval == 0 {
		a.adaptive = true
		a.Interval = FastDiscoveryInterval
	}
	a.seq = 0
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	conn := a.Conn
	if conn == nil {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
		if err != nil {
			return fmt.Errorf("discovery: cannot open a broadcast socket: %w", err)
		}
		_ = c.SetWriteBuffer(8 << 10)
		conn = c
	}
	go a.loop(conn)
	return nil
}

// Stop ends the announcer and closes a connection it opened itself.
func (a *Announcer) Stop() {
	if a.stop == nil {
		return
	}
	a.once.Do(func() { close(a.stop) })
	<-a.done
}

func (a *Announcer) loop(conn *net.UDPConn) {
	defer close(a.done)
	defer func() {
		if a.Conn == nil {
			conn.Close()
		}
	}()
	currentInterval := a.Interval
	t := time.NewTicker(currentInterval)
	defer t.Stop()
	a.send(conn)
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.send(conn)
			if a.adaptive {
				paired := false
				if a.PeerChecker != nil {
					paired = a.PeerChecker()
				} else if a.Browser != nil {
					paired = len(a.Browser.Peers()) > 0
				} else if a.seq >= 6 {
					paired = true
				}
				nextInterval := FastDiscoveryInterval
				if paired {
					nextInterval = DefaultInterval
				}
				if nextInterval != currentInterval {
					currentInterval = nextInterval
					t.Reset(currentInterval)
				}
			}
		}
	}
}

func (a *Announcer) send(conn *net.UDPConn) {
	a.seq++
	b := a.Beacon
	b.Seq = a.seq
	b.SentAt = time.Now().Unix()
	if b.Name == "" {
		b.Name, _ = os.Hostname()
	}
	if b.Addr == "" {
		if addrs := LocalAddrs(); len(addrs) > 0 {
			b.Addr = addrs[0]
		}
	}
	data, err := Encode(b, a.Token)
	if err != nil {
		return
	}
	for _, host := range BroadcastAddrs() {
		dst, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, fmt.Sprint(a.Port)))
		if err != nil {
			continue
		}
		_, _ = conn.WriteToUDP(data, dst)
	}
}

// Browser listens for beacons and keeps the live peer table.
type Browser struct {
	Token  string
	Port   int           // UDP port, 0 = DefaultPort
	TTL    time.Duration // 0 = DefaultTTL
	Conn   *net.UDPConn  // test hook
	SelfID string        // ignore this instance's own beacons
	Wanted Role          // when set, only peers with this role are reported

	mu    sync.RWMutex
	peers map[string]Peer
	stop  chan struct{}
	done  chan struct{}
}

// Listen binds the discovery port and starts collecting. Binding the wildcard
// address is required to receive a broadcast on Linux.
func (b *Browser) Listen() error {
	if b.Port == 0 {
		b.Port = DefaultPort
	}
	if b.TTL == 0 {
		b.TTL = DefaultTTL
	}
	b.mu.Lock()
	if b.peers == nil {
		b.peers = make(map[string]Peer)
	}
	b.mu.Unlock()
	conn := b.Conn
	if conn == nil {
		c, err := b.bind()
		if err != nil {
			return err
		}
		conn = c
	}
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	go b.loop(conn)
	return nil
}

// bind opens the discovery socket. A port already in use usually means a second
// copy of this application is running; reporting that clearly is more useful
// than retrying on a random port, where nothing would ever be heard.
func (b *Browser) bind() (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: b.Port})
	if err != nil {
		return nil, fmt.Errorf("discovery: cannot listen on udp/%d: %w (another PiQuant may already be running)", b.Port, err)
	}
	_ = conn.SetReadBuffer(64 << 10)
	return conn, nil
}

// Close stops the browser.
func (b *Browser) Close() {
	if b.stop == nil {
		return
	}
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	<-b.done
}

func (b *Browser) loop(conn *net.UDPConn) {
	defer close(b.done)
	defer func() {
		if b.Conn == nil {
			conn.Close()
		}
	}()
	buf := make([]byte, MaxDatagram+1)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isClosed(err) {
				return
			}
			select {
			case <-b.stop:
				return
			default:
			}
			continue // timeout, or a datagram we could not read
		}
		select {
		case <-b.stop:
			return
		default:
		}
		if n <= 0 || n > MaxDatagram {
			continue
		}
		beacon, err := Decode(buf[:n], b.Token)
		if err != nil {
			continue
		}
		b.observe(beacon, src)
	}
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func (b *Browser) observe(beacon Beacon, src *net.UDPAddr) {
	if b.SelfID != "" && beacon.ID == b.SelfID {
		return
	}
	if b.Wanted != "" && beacon.Role != b.Wanted {
		return
	}
	p := Peer{Beacon: beacon, LastSeen: time.Now(), Auth: b.Token != ""}
	if src != nil {
		p.SrcIP = src.IP.String()
		// A beacon that advertises an address on another subnet is almost
		// always a broadcast leaking across an interface; the source address
		// is the one that can actually be reached.
		if p.Addr == "" || !sameSubnet(p.Addr, p.SrcIP) {
			p.Addr = p.SrcIP
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.peers == nil {
		b.peers = make(map[string]Peer)
	}
	b.peers[beacon.ID] = p
}

func sameSubnet(a, b string) bool {
	ipA, ipB := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if ipA == nil || ipB == nil {
		return false
	}
	// Without the peer's mask, compare the /24 prefix, which is what every
	// home and office LAN uses.
	return binary.BigEndian.Uint32(ipA)&0xffffff00 == binary.BigEndian.Uint32(ipB)&0xffffff00
}

// Peers returns the live peers, most recently seen first. Peers older than TTL
// are dropped.
func (b *Browser) Peers() []Peer {
	now := time.Now()
	ttl := b.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Peer, 0, len(b.peers))
	for id, p := range b.peers {
		if now.Sub(p.LastSeen) > ttl {
			delete(b.peers, id)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Best returns the most suitable peer for role: the most recently seen one,
// preferring an authenticated beacon. It returns false when no live peer has
// the role.
func (b *Browser) Best(role Role) (Peer, bool) {
	var best Peer
	found := false
	for _, p := range b.Peers() {
		if p.Role != role {
			continue
		}
		if !found || (!best.Auth && p.Auth) {
			best, found = p, true
		}
	}
	return best, found
}

// Wait blocks until a live peer with role is seen or the timeout expires. It is
// what the setup flow uses to say "turn on the Pi 5" and then continue on its
// own.
func (b *Browser) Wait(role Role, timeout, poll time.Duration) (Peer, bool) {
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		if p, ok := b.Best(role); ok {
			return p, true
		}
		if time.Now().After(deadline) {
			return Peer{}, false
		}
		time.Sleep(poll)
	}
}
