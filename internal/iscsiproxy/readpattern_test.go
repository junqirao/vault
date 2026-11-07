package iscsiproxy

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// TestReadPatternObserve 锁定 4K 随机读的判定：只有"4 KiB 且起点不是上一条读的终点"
// 才算随机。判定只做两次整数比较，不分配、不加锁，因此可以放在每条 READ 的路径上。
func TestReadPatternObserve(t *testing.T) {
	const block = 4096
	newPattern := func() readPattern { return readPattern{lastEnd: -1} }

	t.Run("sequential 4K", func(t *testing.T) {
		p := newPattern()
		// 第一条落在偏移 0：lastEnd=-1 让它算随机，之后严格接续都算顺序。
		if !p.observe(0, block) {
			t.Fatal("会话的第一条 4K 读应算随机")
		}
		for i := 1; i < 8; i++ {
			if p.observe(int64(i)*block, block) {
				t.Fatalf("连续 4K 读第 %d 条被误判为随机", i)
			}
		}
	})

	t.Run("random 4K", func(t *testing.T) {
		p := newPattern()
		offsets := []int64{0, 8 * block, 3 * block, 100 * block, 1 * block}
		for _, off := range offsets {
			if !p.observe(off, block) {
				t.Fatalf("跳读 off=%d 应算随机", off)
			}
		}
	})

	t.Run("large read then adjacent 4K", func(t *testing.T) {
		p := newPattern()
		if p.observe(0, 1<<20) {
			t.Fatal("1MiB 读不是 4K，不应计入随机 4K")
		}
		if p.observe(1<<20, block) {
			t.Fatal("紧接在大读末尾的 4K 读是连续的，不应算随机")
		}
	})

	t.Run("non-4K never counted", func(t *testing.T) {
		p := newPattern()
		if p.observe(0, 64<<10) || p.observe(1<<20, 64<<10) {
			t.Fatal("非 4KiB 读不应计入 4K 随机")
		}
	})
}

// TestStatsRandom4KOnWire 端到端锁定计数真的挂在代理的 READ 路径上：纯透传目标
// （不绑定缓存）也要统计，否则"没开缓存的库看不到随机读占比"。
func TestStatsRandom4KOnWire(t *testing.T) {
	bln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("后端监听失败：%v", err)
	}
	defer bln.Close()

	go func() {
		bc, err := bln.Accept()
		if err != nil {
			return
		}
		defer bc.Close()
		for {
			h, err := iscsi.ReadBHS(bc)
			if err != nil {
				return
			}
			if dsl := h.DataSegmentLength(); dsl > 0 {
				if _, err := io.CopyN(io.Discard, bc, int64(dsl+(4-dsl%4)%4)); err != nil {
					return
				}
			}
			cdb, itt := h.CDB(), h.ITT()
			var payload []byte
			switch cdb[0] {
			case iscsi.SCSIInquiry:
				switch cdb[2] {
				case 0x80:
					payload = iscsi.VPDSerial(0x00, "VAULTSERIAL001")
				case 0x83:
					payload = iscsi.VPDDeviceID(0x00, iscsi.T10VendorDescriptor("VAULT", "VAULTSERIAL001"))
				default:
					continue
				}
			case iscsi.SCSIReadCapacity10:
				payload = iscsi.BuildReadCapacity10(1<<20, 4096)
			default:
				continue
			}
			_ = iscsi.WritePDU(bc, iscsi.BuildDataIn(itt, iscsi.ReservedTag, 1, 1, 1, 0, 0,
				payload, true, true, iscsi.StatusGood, 0, false, false))
		}
	}()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := NewPortal(PortalConfig{Logger: log})
	if err != nil {
		t.Fatalf("NewPortal 失败：%v", err)
	}
	defer p.Close()
	if err := p.Add(context.Background(), TargetConfig{
		Key:     "k",
		Backend: BackendConfig{Address: bln.Addr().String(), TargetIQN: "iqn.2026-01.com.vault:test"},
		NoCache: true,
	}); err != nil {
		t.Fatalf("Add 失败：%v", err)
	}

	host, port, ok := p.TargetAddr("k")
	if !ok {
		t.Fatal("TargetAddr 未返回代理地址")
	}
	ic, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer ic.Close()

	cmdSN := uint32(0)
	sendCmd := func(itt uint32, cdb ...byte) {
		cmdSN++
		var h iscsi.BHS
		h[0] = iscsi.OpSCSICommand | 0x80
		h.SetITT(itt)
		h.SetCmdSN(cmdSN)
		copy(h[32:48], cdb)
		if err := iscsi.WritePDU(ic, &iscsi.PDU{Header: h}); err != nil {
			t.Fatalf("发送命令 0x%02x 失败：%v", cdb[0], err)
		}
	}
	readCDB := func(lba uint32) []byte {
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], 1) // 1 × 4096 = 4 KiB
		return cdb
	}

	sendCmd(1, iscsi.SCSIInquiry, 0x01, 0x80, 0x00, 0xff, 0x00)
	sendCmd(2, iscsi.SCSIInquiry, 0x01, 0x83, 0x00, 0xff, 0x00)
	sendCmd(3, iscsi.SCSIReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	// 纯透传目标的 Stats.BlockSize 恒为 0（它只在绑定缓存后回显），所以直接看
	// target 嗅探到的几何。
	p.mu.Lock()
	tgt := p.targets["k"]
	p.mu.Unlock()
	if tgt == nil {
		t.Fatal("门户里找不到目标 k")
	}
	deadline := time.Now().Add(3 * time.Second)
	for tgt.lunBlockSize() != 4096 {
		if time.Now().After(deadline) {
			t.Fatal("等待块大小超时")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 8 条连续 4K（第一条算随机、其余顺序），随后 100 跳读（随机）、101 接续（顺序）、
	// 200 再跳（随机）：随机数应为 3。
	itt := uint32(10)
	for _, lba := range []uint32{0, 1, 2, 3, 4, 5, 6, 7, 100, 101, 200} {
		itt++
		sendCmd(itt, readCDB(lba)...)
	}
	want := int64(3)
	for {
		st, ok := p.Stats("k")
		if ok && st.Proxy.Reads == 11 && st.Proxy.Random4K == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("随机 4K 计数不对：Reads=%d Random4K=%d，期望 Reads=11 Random4K=%d",
				st.Proxy.Reads, st.Proxy.Random4K, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
