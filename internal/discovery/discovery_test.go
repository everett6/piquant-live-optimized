package discovery

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sample(role Role) Beacon {
	return Beacon{
		V:      Version,
		Role:   role,
		ID:     "id-" + string(role),
		Name:   "piquant-test",
		App:    "0.1.0",
		Addr:   "192.168.1.20",
		Port:   8080,
		Seq:    7,
		SentAt: 1_700_000_000,
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, token := range []string{"", "peer-token"} {
		b := sample(RoleEngine)
		data, err := Encode(b, token)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(data, token)
		if err != nil {
			t.Fatalf("token=%q: %v", token, err)
		}
		if got.Role != b.Role || got.ID != b.ID || got.Port != b.Port || got.Addr != b.Addr || got.Seq != b.Seq {
			t.Fatalf("token=%q: round trip lost fields: %+v", token, got)
		}
	}
}

// A token on either side must never be bypassed: an unsigned beacon is refused
// by a token-holding peer, and a forged signature is refused too.
func TestAuthenticationIsNotOptional(t *testing.T) {
	b := sample(RoleEngine)
	unsigned, err := Encode(b, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(unsigned, "peer-token"); err == nil {
		t.Fatal("a token-configured browser accepted an unsigned beacon")
	}

	signed, err := Encode(b, "peer-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(signed, "other-token"); err == nil {
		t.Fatal("a beacon signed with another token was accepted")
	}
	// Flip one byte of the payload: the MAC must catch it.
	tampered := append([]byte{}, signed...)
	tampered[5] ^= 0x01
	if _, err := Decode(tampered, "peer-token"); err == nil {
		t.Fatal("a tampered beacon was accepted")
	}
	// A signed beacon still decodes for a peer with no token, because the
	// payload is valid JSON followed by a signature it does not check.
	if _, err := Decode(signed, ""); err == nil {
		t.Fatal("an unexpectedly signed beacon was accepted without a token")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"truncated", `{"v":1,"role":"engine"`},
		{"wrong version", `{"v":2,"role":"engine","id":"x","port":80}`},
		{"unknown role", `{"v":1,"role":"attacker","id":"x","port":80}`},
		{"no id", `{"v":1,"role":"engine","port":80}`},
		{"bad port", `{"v":1,"role":"engine","id":"x","port":0}`},
		{"not json", "hello"},
	} {
		if _, err := Decode([]byte(tc.body), ""); err == nil {
			t.Fatalf("%s: accepted %q", tc.name, tc.body)
		}
	}
	if _, err := Decode(make([]byte, MaxDatagram+1), ""); err == nil {
		t.Fatal("oversized datagram accepted")
	}
}

func TestBeaconValidate(t *testing.T) {
	b := sample(RoleDashboard)
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(Beacon{V: Version, Role: RoleEngine, ID: "x"}, ""); err == nil {
		t.Fatal("Encode accepted a beacon with no port")
	}
}

func TestLocalAndBroadcastAddrs(t *testing.T) {
	for _, a := range LocalAddrs() {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			t.Fatalf("LocalAddrs returned %q", a)
		}
		if ip.IsLoopback() {
			t.Fatalf("LocalAddrs returned a loopback address %q", a)
		}
	}
	if addrs := BroadcastAddrs(); len(addrs) == 0 || addrs[0] != "255.255.255.255" {
		t.Fatalf("BroadcastAddrs = %v", addrs)
	}
}

func TestSameSubnet(t *testing.T) {
	if !sameSubnet("192.168.1.20", "192.168.1.99") {
		t.Fatal("addresses on one /24 not recognised")
	}
	if sameSubnet("192.168.1.20", "10.0.0.5") || sameSubnet("bad", "10.0.0.5") {
		t.Fatal("different subnets accepted")
	}
}

// Two browsers talking over loopback exercise the real socket path: encode,
// send, receive, decode, learn.
func TestBrowserLearnsAZeroConfPeer(t *testing.T) {
	// One socket for the sender, one bound for the receiver, both on loopback.
	send, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	b := &Browser{Conn: recv, Token: "tok", TTL: time.Second, SelfID: "me", Wanted: RoleEngine}
	if err := b.Listen(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, ok := b.Best(RoleEngine); ok {
		t.Fatal("a browser with no traffic already has a peer")
	}

	payload, err := Encode(sample(RoleEngine), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := send.WriteToUDP(payload, recv.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}

	var peer Peer
	for i := 0; i < 100; i++ {
		if p, ok := b.Best(RoleEngine); ok {
			peer = p
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if peer.ID == "" {
		t.Fatal("browser never learned the peer")
	}
	if !peer.Auth {
		t.Fatal("peer should be authenticated")
	}
	if peer.Endpoint() != "http://"+net.JoinHostPort(peer.Addr, "8080") {
		t.Fatalf("endpoint = %q", peer.Endpoint())
	}

	// A peer whose beacon is signed with the wrong token is not learned.
	bad, _ := Encode(sample(RoleDashboard), "wrong")
	if _, err := send.WriteToUDP(bad, recv.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := b.Best(RoleDashboard); ok {
		t.Fatal("a wrongly signed beacon was learned")
	}

	// The peer's own beacons are ignored, so two processes on one host do
	// not deadlock on each other.
	own, _ := Encode(sample(RoleEngine), "tok")
	var self Beacon
	_ = json.Unmarshal(own, &self)
	self.ID = "me"
	own, _ = Encode(self, "tok")
	if _, err := send.WriteToUDP(own, recv.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if p, ok := b.Best(RoleEngine); !ok || p.ID == "me" {
		t.Fatalf("own beacon was learned: %+v", p)
	}
}

func TestPeerExpires(t *testing.T) {
	b := &Browser{TTL: 20 * time.Millisecond}
	b.observe(sample(RoleEngine), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 1})
	if _, ok := b.Best(RoleEngine); !ok {
		t.Fatal("peer not learned")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := b.Best(RoleEngine); ok {
		t.Fatal("stale peer still reported")
	}
}

// The advertised address is replaced by the datagram source when the two are on
// different subnets, which is what a broadcast leaking across an interface
// looks like.
func TestSourceAddressWinsAcrossSubnets(t *testing.T) {
	b := &Browser{TTL: time.Minute}
	beacon := sample(RoleEngine)
	beacon.Addr = "172.16.0.4"
	b.observe(beacon, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 5000})
	p, ok := b.Best(RoleEngine)
	if !ok {
		t.Fatal("peer not learned")
	}
	if p.Addr != "192.168.1.50" || p.SrcIP != "192.168.1.50" {
		t.Fatalf("address = %q src = %q", p.Addr, p.SrcIP)
	}

	// On the same /24 the advertised address is kept: a multi-homed board
	// knows better than the switch which of its addresses to use.
	b2 := &Browser{TTL: time.Minute}
	beacon.Addr = "192.168.1.10"
	b2.observe(beacon, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 5000})
	if p, _ := b2.Best(RoleEngine); p.Addr != "192.168.1.10" {
		t.Fatalf("same-subnet advertised address replaced: %q", p.Addr)
	}
}

func TestWaitTimesOutAndReturns(t *testing.T) {
	b := &Browser{TTL: time.Minute}
	b.observe(sample(RoleDashboard), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 1})
	if p, ok := b.Wait(RoleDashboard, 100*time.Millisecond, 5*time.Millisecond); !ok || p.Role != RoleDashboard {
		t.Fatalf("Wait did not return the live peer: %+v %v", p, ok)
	}
	start := time.Now()
	if _, ok := b.Wait(RoleEngine, 60*time.Millisecond, 5*time.Millisecond); ok {
		t.Fatal("Wait reported a peer that is not there")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("Wait returned after %v, before the timeout", elapsed)
	}
}

func TestInstanceIDIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	a, err := InstanceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := InstanceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("instance id changed: %s vs %s", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("instance id = %q", a)
	}
	info, err := os.Stat(filepath.Join(dir, "instance-id"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("instance-id mode = %04o", perm)
	}
	other, err := InstanceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Fatal("two installations share an instance id")
	}
}

// A directory that is not writable must surface as an error, not as a silent
// empty id that would make every peer look like itself.
func TestInstanceIDError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permissions do not apply")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := InstanceID(filepath.Join(dir, "blocked")); err == nil {
		t.Fatal("InstanceID wrote into a read-only directory")
	}
}

func TestAnnouncerStartStop(t *testing.T) {
	send, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	a := &Announcer{Beacon: sample(RoleEngine), Conn: send, Interval: 10 * time.Millisecond, Port: 1}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	a.Stop()
	send.Close()
}
