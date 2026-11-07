package cluster

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol constants.
const (
	FrameMagic         uint32 = 0x50514C4B // "PQLK" (PiQuant Link)
	FrameVersion       uint8  = 1
	FrameSize                 = 64
	SocketBufferSize          = 256 * 1024 // 256 KiB
	DefaultKeepAlive          = 5 * time.Second
)

// Frame types for inter-Pi multiplexed communication.
const (
	FrameHeartbeat      uint8 = 1
	FrameTelemetry      uint8 = 2
	FrameBlotterSummary uint8 = 3
	FrameTaskDispatch   uint8 = 4
	FrameTaskAck        uint8 = 5
)

// Flags carried in the binary frame.
const (
	FlagACK             uint16 = 1 << 0
	FlagSync            uint16 = 1 << 1
	FlagThermalAlert    uint16 = 1 << 2
	FlagFailover        uint16 = 1 << 3
	FlagOffloadEligible uint16 = 1 << 4
)

// Errors returned by frame parsing and link operations.
var (
	ErrInvalidMagic        = errors.New("cluster: invalid PQLK frame magic")
	ErrIncompatibleVersion = errors.New("cluster: incompatible protocol version")
	ErrInvalidFrameSize    = errors.New("cluster: buffer size must be at least 64 bytes")
	ErrShortRead           = errors.New("cluster: short read on binary frame")
	ErrLinkClosed          = errors.New("cluster: fastlink connection closed")
	ErrAuthFailed          = errors.New("cluster: frame failed HMAC-64 authentication")
)

// BinaryFrame is a 64-byte cache-line aligned binary wire frame optimized for
// zero-allocation inter-Pi communication between Pi 5 and Pi 3B.
//
// Memory Layout ([64]byte):
//
//	Offset  Size  Field
//	 0..3    4    Magic (0x50514C4B "PQLK")
//	 4       1    Version (1)
//	 5       1    FrameType (Heartbeat, Telemetry, BlotterSummary, TaskDispatch, TaskAck)
//	 6..7    2    Flags (ACK, Sync, ThermalAlert, Failover, OffloadEligible)
//	 8..15   8    SeqNum (Monotonic sequence number)
//	16..23   8    SendTimestampNs (Sender nanosecond timestamp t1 or t3)
//	24..31   8    AckTimestampNs (Receiver echo nanosecond timestamp t2)
//	32..33   2    CPUUtilPermille (0..1000 per-mille)
//	34..35   2    MemAvailMB (Available RAM in MB)
//	36..37   2    TempDeciC (Temperature in deci-Celsius, e.g. 455 = 45.5°C)
//	38..39   2    ActiveTasks (Number of active tasks running)
//	40..47   8    PayloadFloat1 (First float64 metric / PnL / Markout)
//	48..55   8    PayloadFloat2 (Second float64 metric / Latency)
//	56..63   8    HMACTag64 (Truncated 64-bit SipHash/FNV tag over bytes 0..55)
type BinaryFrame struct {
	Magic           uint32
	Version         uint8
	FrameType       uint8
	Flags           uint16
	SeqNum          uint64
	SendTimestampNs int64
	AckTimestampNs  int64
	CPUUtilPermille uint16
	MemAvailMB      uint16
	TempDeciC       uint16
	ActiveTasks     uint16
	PayloadFloat1   float64
	PayloadFloat2   float64
	HMACTag64       uint64
}

// EncodeBinaryFrame encodes f directly into dst with zero heap allocations.
// dst must be exactly 64 bytes.
func EncodeBinaryFrame(f *BinaryFrame, dst *[64]byte) {
	binary.LittleEndian.PutUint32(dst[0:4], f.Magic)
	dst[4] = f.Version
	dst[5] = f.FrameType
	binary.LittleEndian.PutUint16(dst[6:8], f.Flags)
	binary.LittleEndian.PutUint64(dst[8:16], f.SeqNum)
	binary.LittleEndian.PutUint64(dst[16:24], uint64(f.SendTimestampNs))
	binary.LittleEndian.PutUint64(dst[24:32], uint64(f.AckTimestampNs))
	binary.LittleEndian.PutUint16(dst[32:34], f.CPUUtilPermille)
	binary.LittleEndian.PutUint16(dst[34:36], f.MemAvailMB)
	binary.LittleEndian.PutUint16(dst[36:38], f.TempDeciC)
	binary.LittleEndian.PutUint16(dst[38:40], f.ActiveTasks)
	binary.LittleEndian.PutUint64(dst[40:48], math.Float64bits(f.PayloadFloat1))
	binary.LittleEndian.PutUint64(dst[48:56], math.Float64bits(f.PayloadFloat2))
	binary.LittleEndian.PutUint64(dst[56:64], f.HMACTag64)
}

// Encode serializes f into dst.
func (f *BinaryFrame) Encode(dst *[64]byte) {
	EncodeBinaryFrame(f, dst)
}

// DecodeBinaryFrame parses a 64-byte buffer into f with zero heap allocations.
func DecodeBinaryFrame(src *[64]byte, f *BinaryFrame) error {
	magic := binary.LittleEndian.Uint32(src[0:4])
	if magic != FrameMagic {
		return ErrInvalidMagic
	}
	ver := src[4]
	if ver != FrameVersion {
		return ErrIncompatibleVersion
	}
	f.Magic = magic
	f.Version = ver
	f.FrameType = src[5]
	f.Flags = binary.LittleEndian.Uint16(src[6:8])
	f.SeqNum = binary.LittleEndian.Uint64(src[8:16])
	f.SendTimestampNs = int64(binary.LittleEndian.Uint64(src[16:24]))
	f.AckTimestampNs = int64(binary.LittleEndian.Uint64(src[24:32]))
	f.CPUUtilPermille = binary.LittleEndian.Uint16(src[32:34])
	f.MemAvailMB = binary.LittleEndian.Uint16(src[34:36])
	f.TempDeciC = binary.LittleEndian.Uint16(src[36:38])
	f.ActiveTasks = binary.LittleEndian.Uint16(src[38:40])
	f.PayloadFloat1 = math.Float64frombits(binary.LittleEndian.Uint64(src[40:48]))
	f.PayloadFloat2 = math.Float64frombits(binary.LittleEndian.Uint64(src[48:56]))
	f.HMACTag64 = binary.LittleEndian.Uint64(src[56:64])
	return nil
}

// Decode unmarshals src into f.
func (f *BinaryFrame) Decode(src *[64]byte) error {
	return DecodeBinaryFrame(src, f)
}

// ComputeTag64 calculates a high-speed, zero-allocation 64-bit SipHash-2-4 MAC
// over the first 56 bytes of buf using the 64-bit secret key.
func ComputeTag64(buf *[64]byte, key uint64) uint64 {
	k0 := key
	k1 := key ^ 0x736f6d6570736575 // arbitrary salt

	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573

	// Process 7 8-byte blocks (56 bytes total)
	for i := 0; i < 56; i += 8 {
		m := binary.LittleEndian.Uint64(buf[i : i+8])
		v3 ^= m
		// 2 SipRounds
		v0 += v1
		v1 = (v1 << 13) | (v1 >> (64 - 13))
		v1 ^= v0
		v0 = (v0 << 32) | (v0 >> 32)
		v2 += v3
		v3 = (v3 << 16) | (v3 >> (64 - 16))
		v3 ^= v2
		v0 += v3
		v3 = (v3 << 21) | (v3 >> (64 - 21))
		v3 ^= v0
		v2 += v1
		v1 = (v1 << 17) | (v1 >> (64 - 17))
		v1 ^= v2
		v2 = (v2 << 32) | (v2 >> 32)

		v0 += v1
		v1 = (v1 << 13) | (v1 >> (64 - 13))
		v1 ^= v0
		v0 = (v0 << 32) | (v0 >> 32)
		v2 += v3
		v3 = (v3 << 16) | (v3 >> (64 - 16))
		v3 ^= v2
		v0 += v3
		v3 = (v3 << 21) | (v3 >> (64 - 21))
		v3 ^= v0
		v2 += v1
		v1 = (v1 << 17) | (v1 >> (64 - 17))
		v1 ^= v2
		v2 = (v2 << 32) | (v2 >> 32)

		v0 ^= m
	}

	// Finalization with byte length 56
	b := uint64(56) << 56
	v3 ^= b

	// 2 rounds
	for r := 0; r < 2; r++ {
		v0 += v1
		v1 = (v1 << 13) | (v1 >> (64 - 13))
		v1 ^= v0
		v0 = (v0 << 32) | (v0 >> 32)
		v2 += v3
		v3 = (v3 << 16) | (v3 >> (64 - 16))
		v3 ^= v2
		v0 += v3
		v3 = (v3 << 21) | (v3 >> (64 - 21))
		v3 ^= v0
		v2 += v1
		v1 = (v1 << 17) | (v1 >> (64 - 17))
		v1 ^= v2
		v2 = (v2 << 32) | (v2 >> 32)
	}

	v2 ^= 0xff

	// 4 final rounds
	for r := 0; r < 4; r++ {
		v0 += v1
		v1 = (v1 << 13) | (v1 >> (64 - 13))
		v1 ^= v0
		v0 = (v0 << 32) | (v0 >> 32)
		v2 += v3
		v3 = (v3 << 16) | (v3 >> (64 - 16))
		v3 ^= v2
		v0 += v3
		v3 = (v3 << 21) | (v3 >> (64 - 21))
		v3 ^= v0
		v2 += v1
		v1 = (v1 << 17) | (v1 >> (64 - 17))
		v1 ^= v2
		v2 = (v2 << 32) | (v2 >> 32)
	}

	return v0 ^ v1 ^ v2 ^ v3
}

// Sign encodes f into dst and generates the authenticating SipHash-64 tag.
func (f *BinaryFrame) Sign(dst *[64]byte, key uint64) {
	f.Magic = FrameMagic
	f.Version = FrameVersion
	f.Encode(dst)
	tag := ComputeTag64(dst, key)
	f.HMACTag64 = tag
	binary.LittleEndian.PutUint64(dst[56:64], tag)
}

// VerifyTag checks whether the SipHash-64 tag in src matches key.
func VerifyTag64(src *[64]byte, key uint64) bool {
	expected := ComputeTag64(src, key)
	actual := binary.LittleEndian.Uint64(src[56:64])
	return expected == actual
}

// LinkTracker maintains running microsecond RTT, clock skew, and exponentially
// weighted moving average jitter between Pi 5 and Pi 3B using NTP 4-timestamp math.
type LinkTracker struct {
	mu           sync.RWMutex
	rttMicros    float64
	jitterMicros float64
	clockOffset  int64
	sampleCount  uint64
	lastUpdate   time.Time
}

// NewLinkTracker creates an initialized LinkTracker.
func NewLinkTracker() *LinkTracker {
	return &LinkTracker{}
}

// UpdateNTP updates link statistics using NTP 4-timestamp calculations:
//   - t1Ns: Sender transmit timestamp (client)
//   - t2Ns: Receiver receipt timestamp (server)
//   - t3Ns: Server response transmit timestamp
//   - t4Ns: Client receipt timestamp
//
// Math:
//
//	RTT = (t4 - t1) - (t3 - t2)
//	ClockSkew = ((t2 - t1) + (t3 - t4)) / 2
//	Jitter (RFC 3550 EWMA) = J + (|RTT_new - RTT_prev| - J) / 16
func (lt *LinkTracker) UpdateNTP(t1Ns, t2Ns, t3Ns, t4Ns int64) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	remoteProc := t3Ns - t2Ns
	if remoteProc < 0 {
		remoteProc = 0
	}
	rttNs := (t4Ns - t1Ns) - remoteProc
	if rttNs < 0 {
		rttNs = t4Ns - t1Ns
		if rttNs < 0 {
			rttNs = 0
		}
	}
	rttUs := float64(rttNs) / 1000.0
	offsetNs := ((t2Ns - t1Ns) + (t3Ns - t4Ns)) / 2

	if lt.sampleCount == 0 {
		lt.rttMicros = rttUs
		lt.jitterMicros = 0
	} else {
		diff := math.Abs(rttUs - lt.rttMicros)
		lt.rttMicros = 0.85*lt.rttMicros + 0.15*rttUs
		lt.jitterMicros += (diff - lt.jitterMicros) / 16.0
	}
	lt.clockOffset = offsetNs
	lt.sampleCount++
	lt.lastUpdate = time.Now()
}

// UpdateRTT directly records a measured round-trip time in microseconds.
func (lt *LinkTracker) UpdateRTT(rttUs float64) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	if rttUs < 0 {
		rttUs = 0
	}
	if lt.sampleCount == 0 {
		lt.rttMicros = rttUs
		lt.jitterMicros = 0
	} else {
		diff := math.Abs(rttUs - lt.rttMicros)
		lt.rttMicros = 0.85*lt.rttMicros + 0.15*rttUs
		lt.jitterMicros += (diff - lt.jitterMicros) / 16.0
	}
	lt.sampleCount++
	lt.lastUpdate = time.Now()
}

// RTTMicros returns the current estimated round-trip latency in microseconds.
func (lt *LinkTracker) RTTMicros() float64 {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.rttMicros
}

// ClockOffsetNs returns the estimated clock skew in nanoseconds between the nodes.
func (lt *LinkTracker) ClockOffsetNs() int64 {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.clockOffset
}

// JitterMicros returns the smoothed microsecond jitter.
func (lt *LinkTracker) JitterMicros() float64 {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.jitterMicros
}

// SampleCount returns the total number of NTP measurements.
func (lt *LinkTracker) SampleCount() uint64 {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.sampleCount
}

// LastUpdate returns when the link tracker was last refreshed.
func (lt *LinkTracker) LastUpdate() time.Time {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.lastUpdate
}

// TuneTCPConn configures a persistent TCP connection with low-latency parameters:
// TCP_NODELAY, SO_KEEPALIVE, 5-second probe intervals, and 256 KiB socket buffers.
func TuneTCPConn(conn *net.TCPConn) error {
	if conn == nil {
		return errors.New("cluster: nil TCPConn")
	}
	if err := conn.SetNoDelay(true); err != nil {
		return fmt.Errorf("cluster: failed to set TCP_NODELAY: %w", err)
	}
	if err := conn.SetKeepAlive(true); err != nil {
		return fmt.Errorf("cluster: failed to set SO_KEEPALIVE: %w", err)
	}
	if err := conn.SetKeepAlivePeriod(DefaultKeepAlive); err != nil {
		return fmt.Errorf("cluster: failed to set keepalive period: %w", err)
	}
	_ = conn.SetReadBuffer(SocketBufferSize)
	_ = conn.SetWriteBuffer(SocketBufferSize)
	return nil
}

// TuneConn applies socket performance tuning to any supported connection type.
func TuneConn(conn net.Conn) error {
	if tcp, ok := conn.(*net.TCPConn); ok {
		return TuneTCPConn(tcp)
	}
	if udp, ok := conn.(*net.UDPConn); ok {
		_ = udp.SetReadBuffer(SocketBufferSize)
		_ = udp.SetWriteBuffer(SocketBufferSize)
		return nil
	}
	return nil
}

// FastLink manages a persistent, multiplexed point-to-point binary connection
// between Pi 5 and Pi 3B.
type FastLink struct {
	conn       net.Conn
	key        uint64
	tracker    *LinkTracker
	seq        uint64
	closed     atomic.Bool
	sendMu     sync.Mutex
	writeBuf   [FrameSize]byte
	recvMu     sync.Mutex
	readBuf    [FrameSize]byte
	onDispatch func(f *BinaryFrame)
	onAck      func(f *BinaryFrame)
}

// NewFastLink creates a tuned FastLink over conn.
func NewFastLink(conn net.Conn, key uint64) (*FastLink, error) {
	if conn == nil {
		return nil, errors.New("cluster: nil connection")
	}
	_ = TuneConn(conn)
	return &FastLink{
		conn:    conn,
		key:     key,
		tracker: NewLinkTracker(),
	}, nil
}

// Tracker returns the link latency and jitter tracker.
func (fl *FastLink) Tracker() *LinkTracker {
	return fl.tracker
}

// Send writes a binary frame directly to the connection with zero allocations.
func (fl *FastLink) Send(f *BinaryFrame) error {
	if fl.closed.Load() {
		return ErrLinkClosed
	}
	fl.sendMu.Lock()
	defer fl.sendMu.Unlock()

	f.SeqNum = atomic.AddUint64(&fl.seq, 1)
	if f.SendTimestampNs == 0 {
		f.SendTimestampNs = time.Now().UnixNano()
	}
	if fl.key != 0 {
		f.Sign(&fl.writeBuf, fl.key)
	} else {
		f.Magic = FrameMagic
		f.Version = FrameVersion
		f.Encode(&fl.writeBuf)
	}

	_, err := fl.conn.Write(fl.writeBuf[:])
	return err
}

// Receive reads the next 64-byte frame from the connection into f.
func (fl *FastLink) Receive(f *BinaryFrame) error {
	if fl.closed.Load() {
		return ErrLinkClosed
	}
	fl.recvMu.Lock()
	defer fl.recvMu.Unlock()

	if _, err := io.ReadFull(fl.conn, fl.readBuf[:]); err != nil {
		return err
	}
	if fl.key != 0 && !VerifyTag64(&fl.readBuf, fl.key) {
		return ErrAuthFailed
	}
	return f.Decode(&fl.readBuf)
}

// SendHeartbeat sends a ping heartbeat with the local timestamp.
func (fl *FastLink) SendHeartbeat(cpuPermille, memAvailMB, tempDeciC, activeTasks uint16) error {
	var f BinaryFrame
	f.FrameType = FrameHeartbeat
	f.CPUUtilPermille = cpuPermille
	f.MemAvailMB = memAvailMB
	f.TempDeciC = tempDeciC
	f.ActiveTasks = activeTasks
	f.SendTimestampNs = time.Now().UnixNano()
	return fl.Send(&f)
}

// Close closes the underlying network connection.
func (fl *FastLink) Close() error {
	fl.closed.Store(true)
	return fl.conn.Close()
}
