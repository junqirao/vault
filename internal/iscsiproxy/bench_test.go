package iscsiproxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// The benchmarks in this file exercise the shipped v2 engine, not the v1
// terminating proxy under internal/iscsicache. They speak raw iSCSI to the
// portal, so the numbers include frame parsing, sequence rewriting, batching
// and, for the hot rows, the local Data-In injection path.
//
// "direct" dials the fake backend without the portal; "proxy-passthrough"
// forwards every read and is the cost of the splice; "proxy-hot" warms one
// range first and then serves it from L1 without touching the backend.
//
// The backend is an in-memory LUN on loopback, so the absolute numbers are a
// package-level floor, not a prediction for a real HDD or network. The relative
// columns and backend-reads/op are the parts worth comparing between commits.

const benchCacheBlock = 64 << 10

func benchStartPortal(b *testing.B, be *invBackend, noCache bool) *Portal {
	b.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := NewPortal(PortalConfig{Logger: log})
	if err != nil {
		b.Fatalf("NewPortal: %v", err)
	}
	if err := p.Add(context.Background(), TargetConfig{
		Key:     "k",
		Backend: BackendConfig{Address: be.ln.Addr().String(), TargetIQN: "iqn.2026-01.com.vault:bench"},
		Cache: CacheConfig{
			BlockSize:      benchCacheBlock,
			SectorSize:     4 << 10,
			L1Bytes:        256 << 20,
			FillWorkers:    4,
			FillQueueDepth: 64,
		},
		NoCache: noCache,
	}); err != nil {
		_ = p.Close()
		b.Fatalf("Add: %v", err)
	}
	b.Cleanup(func() { _ = p.Close() })
	return p
}

func benchDialPortal(b *testing.B, p *Portal) net.Conn {
	b.Helper()
	host, port, ok := p.TargetAddr("k")
	if !ok {
		b.Fatal("TargetAddr: no listener")
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		b.Fatalf("dial portal: %v", err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	return conn
}

// benchSniff drains the three identity responses itself: unlike the correctness
// tests, a benchmark cannot leave Data-In PDUs queued in the socket and then
// mistake them for the response to its first read.
func benchSniff(b *testing.B, p *Portal, conn net.Conn) {
	b.Helper()
	invSend(b, conn, 1, iscsi.SCSIInquiry, 0x01, 0x80, 0x00, 0xff, 0x00)
	benchDrainPDU(b, conn)
	invSend(b, conn, 2, iscsi.SCSIInquiry, 0x01, 0x83, 0x00, 0xff, 0x00)
	benchDrainPDU(b, conn)
	invSend(b, conn, 3, iscsi.SCSIReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	benchDrainPDU(b, conn)
	invWait(b, "cache binds identity", func() bool {
		st, ok := p.Stats("k")
		return ok && st.BlockSize == benchCacheBlock
	})
}

func benchDrainPDU(b *testing.B, conn net.Conn) {
	b.Helper()
	if _, err := iscsi.ReadPDU(conn); err != nil {
		b.Fatalf("read identity response: %v", err)
	}
}

func benchSendRead(b *testing.B, w io.Writer, itt uint32, lba uint64, blocks uint32) {
	b.Helper()
	var h iscsi.BHS
	h[0] = iscsi.OpSCSICommand | iscsi.CmdFlagFinal
	h.SetITT(itt)
	cdb := h.CDB()
	cdb[0] = iscsi.SCSIRead10
	binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
	binary.BigEndian.PutUint16(cdb[7:9], uint16(blocks))
	if err := iscsi.WritePDU(w, &iscsi.PDU{Header: h}); err != nil {
		b.Fatalf("send READ(10): %v", err)
	}
}

// benchReadResponse reads Data-In segments until the status-carrying one, using
// the caller's buffer so the benchmark client does not allocate per segment.
func benchReadResponse(b *testing.B, r io.Reader, buf []byte, want int) {
	b.Helper()
	got := 0
	for {
		h, err := iscsi.ReadBHS(r)
		if err != nil {
			b.Fatalf("read response: %v", err)
		}
		dsl := h.DataSegmentLength()
		if dsl > len(buf) {
			b.Fatalf("data segment %d exceeds the %d byte benchmark buffer", dsl, len(buf))
		}
		if err := iscsi.ReadSegment(r, &h, buf[:dsl]); err != nil {
			b.Fatalf("read data segment: %v", err)
		}
		switch h.Opcode() {
		case iscsi.OpDataIn:
			got += dsl
			if h.Flags()&iscsi.DataInFlagStatus != 0 {
				if got != want {
					b.Fatalf("read returned %d of %d bytes", got, want)
				}
				return
			}
		case iscsi.OpSCSIResponse:
			if got != want {
				b.Fatalf("read returned %d of %d bytes", got, want)
			}
			return
		default:
			b.Fatalf("unexpected response opcode 0x%02x", h.Opcode())
		}
	}
}

func benchReadLoop(b *testing.B, conn net.Conn, be *invBackend, lba uint64, blocks uint32, size int) {
	buf := make([]byte, size)
	before := int64(be.totalReads())
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSendRead(b, conn, uint32(i+100), lba, blocks)
		benchReadResponse(b, conn, buf, size)
	}
	b.StopTimer()
	reads := int64(be.totalReads()) - before
	b.ReportMetric(float64(reads)/float64(b.N), "backend-reads/op")
}

// BenchmarkProxyReadOverhead is the loopback floor: there is no network in the
// way, so the extra hop is the whole cost. It is useful for spotting a
// regression in the forwarding path, not for predicting a real mount.
func BenchmarkProxyReadOverhead(b *testing.B) {
	benchProxyReadMatrix(b, 0)
}

// BenchmarkProxyReadRemote adds a synthetic 1ms service time to the backend,
// the shape of a remote target over a network or a fast SSD. This is the
// comparison the product cares about: a miss pays the direct round trip plus a
// small forwarding cost, while a hit never leaves the machine.
func BenchmarkProxyReadRemote(b *testing.B) {
	benchProxyReadMatrix(b, time.Millisecond)
}

// BenchmarkProxyReadHDD uses a 10ms backend service time, the shape of a random
// read on an HDD. It makes the fixed local copy of a large miss a smaller share
// of the total, which is why the README quotes this row for HDD deployments.
func BenchmarkProxyReadHDD(b *testing.B) {
	benchProxyReadMatrix(b, 10*time.Millisecond)
}

func benchProxyReadMatrix(b *testing.B, backendLatency time.Duration) {
	b.Helper()
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"4KiB", 4 << 10},
		{"64KiB", 64 << 10},
		{"1MiB", 1 << 20},
	} {
		size := tc.size
		b.Run("direct/"+tc.name, func(b *testing.B) {
			benchRunReadCase(b, "direct", size, backendLatency)
		})
		b.Run("proxy-passthrough/"+tc.name, func(b *testing.B) {
			benchRunReadCase(b, "proxy-passthrough", size, backendLatency)
		})
		b.Run("proxy-hot/"+tc.name, func(b *testing.B) {
			benchRunReadCase(b, "proxy-hot", size, backendLatency)
		})
	}
}

func benchRunReadCase(b *testing.B, mode string, size int64, backendLatency time.Duration) {
	b.Helper()
	blocks := uint32(size / invBlockSize)
	be := newInvBackend(b)
	be.keep = false
	be.lat = backendLatency

	switch mode {
	case "direct":
		conn, err := net.Dial("tcp", be.ln.Addr().String())
		if err != nil {
			b.Fatalf("dial backend: %v", err)
		}
		defer conn.Close()
		benchReadLoop(b, conn, be, 0, blocks, int(size))

	case "proxy-passthrough":
		p := benchStartPortal(b, be, true)
		benchReadLoop(b, benchDialPortal(b, p), be, 0, blocks, int(size))

	case "proxy-hot":
		p := benchStartPortal(b, be, false)
		conn := benchDialPortal(b, p)
		benchSniff(b, p, conn)

		buf := make([]byte, size)
		benchSendRead(b, conn, 10, 0, blocks)
		benchReadResponse(b, conn, buf, int(size))
		invWait(b, fmt.Sprintf("%d KiB enters L1", size>>10), func() bool {
			st, ok := p.Stats("k")
			return ok && st.Cache.L1UsedBytes >= size
		})

		// Fills are asynchronous, so L1 being allocated does not yet prove every
		// sector of this range is valid. Probe until one read stays local, then
		// use that count as the baseline: a loaded machine must not turn a slow
		// fill into a benchmark failure.
		var before int64
		hot := false
		for attempt := 0; attempt < 100; attempt++ {
			before = int64(be.totalReads())
			benchSendRead(b, conn, 11, 0, blocks)
			benchReadResponse(b, conn, buf, int(size))
			if int64(be.totalReads()) == before {
				hot = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !hot {
			b.Fatal("warm read kept reaching the backend: the fill never completed")
		}

		benchReadLoop(b, conn, be, 0, blocks, int(size))
		if got := int64(be.totalReads()) - before; got != 0 {
			b.Fatalf("hot loop reached the backend %d times", got)
		}

	default:
		b.Fatalf("unknown benchmark mode %q", mode)
	}
}
