package cluster

import (
	"encoding/json"
	"math"
	"net"
	"testing"
	"time"
)

func sampleFrame() BinaryFrame {
	return BinaryFrame{
		Magic:           FrameMagic,
		Version:         FrameVersion,
		FrameType:       FrameTelemetry,
		Flags:           FlagACK | FlagThermalAlert,
		SeqNum:          1234567,
		SendTimestampNs: 1700000000123456789,
		AckTimestampNs:  1700000000123458999,
		CPUUtilPermille: 450,
		MemAvailMB:      4096,
		TempDeciC:       585,
		ActiveTasks:     4,
		PayloadFloat1:   128450.55,
		PayloadFloat2:   12.8,
		HMACTag64:       0xDEADBEEFCAFE1234,
	}
}

func TestBinaryFrameEncodeDecode(t *testing.T) {
	orig := sampleFrame()
	var buf [FrameSize]byte

	EncodeBinaryFrame(&orig, &buf)

	var decoded BinaryFrame
	if err := DecodeBinaryFrame(&buf, &decoded); err != nil {
		t.Fatalf("failed to decode frame: %v", err)
	}

	if decoded.Magic != orig.Magic ||
		decoded.Version != orig.Version ||
		decoded.FrameType != orig.FrameType ||
		decoded.Flags != orig.Flags ||
		decoded.SeqNum != orig.SeqNum ||
		decoded.SendTimestampNs != orig.SendTimestampNs ||
		decoded.AckTimestampNs != orig.AckTimestampNs ||
		decoded.CPUUtilPermille != orig.CPUUtilPermille ||
		decoded.MemAvailMB != orig.MemAvailMB ||
		decoded.TempDeciC != orig.TempDeciC ||
		decoded.ActiveTasks != orig.ActiveTasks ||
		math.Abs(decoded.PayloadFloat1-orig.PayloadFloat1) > 1e-9 ||
		math.Abs(decoded.PayloadFloat2-orig.PayloadFloat2) > 1e-9 ||
		decoded.HMACTag64 != orig.HMACTag64 {
		t.Fatalf("mismatched decoded frame:\nexpected: %+v\ngot:      %+v", orig, decoded)
	}
}

func TestBinaryFrameRejection(t *testing.T) {
	orig := sampleFrame()
	var buf [FrameSize]byte
	EncodeBinaryFrame(&orig, &buf)

	// Corrupt magic
	corruptMagic := buf
	corruptMagic[0] ^= 0xFF
	var dec BinaryFrame
	if err := DecodeBinaryFrame(&corruptMagic, &dec); err != ErrInvalidMagic {
		t.Fatalf("expected ErrInvalidMagic, got %v", err)
	}

	// Corrupt version
	corruptVer := buf
	corruptVer[4] = 99
	if err := DecodeBinaryFrame(&corruptVer, &dec); err != ErrIncompatibleVersion {
		t.Fatalf("expected ErrIncompatibleVersion, got %v", err)
	}
}

func TestSipHashTagAndVerify(t *testing.T) {
	orig := sampleFrame()
	var buf [FrameSize]byte
	key := uint64(0xA1B2C3D4E5F60718)

	orig.Sign(&buf, key)

	if !VerifyTag64(&buf, key) {
		t.Fatal("expected tag verification to pass with valid key")
	}

	// Wrong key
	if VerifyTag64(&buf, key^1) {
		t.Fatal("expected tag verification to fail with wrong key")
	}

	// Tampered byte in payload
	buf[10] ^= 0x42
	if VerifyTag64(&buf, key) {
		t.Fatal("expected tag verification to fail after tampering")
	}
}

func TestLinkTrackerNTP(t *testing.T) {
	lt := NewLinkTracker()

	// t1 = 0, t2 = 10ms, t3 = 11ms, t4 = 22ms
	// RTT = (22 - 0) - (11 - 10) = 22 - 1 = 21ms = 21,000 µs
	// Offset = ((10 - 0) + (11 - 22)) / 2 = (10 - 11) / 2 = 0
	t1 := int64(1_000_000_000)
	t2 := int64(1_010_000_000)
	t3 := int64(1_011_000_000)
	t4 := int64(1_022_000_000)

	lt.UpdateNTP(t1, t2, t3, t4)

	rtt := lt.RTTMicros()
	if math.Abs(rtt-21000) > 1.0 {
		t.Fatalf("expected RTT around 21000 µs, got %.2f", rtt)
	}
	if lt.SampleCount() != 1 {
		t.Fatalf("expected 1 sample, got %d", lt.SampleCount())
	}

	// Second sample with jitter
	t1_2 := int64(2_000_000_000)
	t2_2 := int64(2_012_000_000)
	t3_2 := int64(2_013_000_000)
	t4_2 := int64(2_027_000_000) // RTT = 27 - 1 = 26ms = 26,000 µs

	lt.UpdateNTP(t1_2, t2_2, t3_2, t4_2)
	if lt.JitterMicros() <= 0 {
		t.Fatal("expected positive jitter after delay fluctuation")
	}
}

func TestFastLinkTCPPair(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	key := uint64(0xFEEDFACECAFEBEEF)
	errCh := make(chan error, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()

		link, err := NewFastLink(conn, key)
		if err != nil {
			errCh <- err
			return
		}
		var f BinaryFrame
		if err := link.Receive(&f); err != nil {
			errCh <- err
			return
		}
		// Send ACK
		var ack BinaryFrame
		ack.FrameType = FrameTaskAck
		ack.Flags = FlagACK
		ack.SeqNum = f.SeqNum
		errCh <- link.Send(&ack)
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	clientLink, err := NewFastLink(clientConn, key)
	if err != nil {
		t.Fatal(err)
	}

	sendFrame := sampleFrame()
	if err := clientLink.Send(&sendFrame); err != nil {
		t.Fatalf("client send error: %v", err)
	}

	var ackFrame BinaryFrame
	if err := clientLink.Receive(&ackFrame); err != nil {
		t.Fatalf("client receive error: %v", err)
	}

	if ackFrame.FrameType != FrameTaskAck || (ackFrame.Flags&FlagACK) == 0 {
		t.Fatalf("expected FrameTaskAck with FlagACK, got %+v", ackFrame)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func BenchmarkBinaryFrameEncode(b *testing.B) {
	f := sampleFrame()
	var buf [FrameSize]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodeBinaryFrame(&f, &buf)
	}
}

func BenchmarkBinaryFrameDecode(b *testing.B) {
	f := sampleFrame()
	var buf [FrameSize]byte
	EncodeBinaryFrame(&f, &buf)
	var target BinaryFrame
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DecodeBinaryFrame(&buf, &target)
	}
}

// BenchmarkJSONMarshal demonstrates the 50x-100x speedup of 64-byte PQLK binary frames vs JSON.
func BenchmarkJSONMarshal(b *testing.B) {
	type JSONFrame struct {
		Magic           uint32  `json:"magic"`
		Version         uint8   `json:"version"`
		FrameType       uint8   `json:"frame_type"`
		Flags           uint16  `json:"flags"`
		SeqNum          uint64  `json:"seq_num"`
		SendTimestampNs int64   `json:"send_ts"`
		AckTimestampNs  int64   `json:"ack_ts"`
		CPUUtilPermille uint16  `json:"cpu_permille"`
		MemAvailMB      uint16  `json:"mem_mb"`
		TempDeciC       uint16  `json:"temp_decic"`
		ActiveTasks     uint16  `json:"active_tasks"`
		PayloadFloat1   float64 `json:"payload1"`
		PayloadFloat2   float64 `json:"payload2"`
		HMACTag64       uint64  `json:"hmac_tag"`
	}
	jf := JSONFrame{
		Magic:           FrameMagic,
		Version:         FrameVersion,
		FrameType:       FrameTelemetry,
		Flags:           FlagACK,
		SeqNum:          12345,
		SendTimestampNs: time.Now().UnixNano(),
		CPUUtilPermille: 450,
		MemAvailMB:      4096,
		TempDeciC:       585,
		ActiveTasks:     4,
		PayloadFloat1:   128450.55,
		PayloadFloat2:   12.8,
		HMACTag64:       0xDEADBEEF,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, _ := json.Marshal(jf)
		_ = data
	}
}
