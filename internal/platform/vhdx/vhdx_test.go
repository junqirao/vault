package vhdx

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	testMetadataOffset = 512 * 1024
	testMetadataLength = 64 * 1024
)

// writeTestVHDX 造一个最小但结构合法的 VHDX 头：文件标识 + 区域表 + 元数据区域。
//
// 只写解析器真正会读的字段（签名、项数、GUID、偏移），不模拟完整磁盘内容：
// 这里的测试对象是"能不能从固定偏移把标称容量读出来"，不是 VHDX 规范本身。
func writeTestVHDX(t *testing.T, size int64, regionTableOffset int64) string {
	t.Helper()

	buf := make([]byte, testMetadataOffset+testMetadataLength)
	copy(buf[0:], fileSignature)

	// 区域表：1 项，指向元数据区域。
	copy(buf[regionTableOffset:], "regi")
	binary.LittleEndian.PutUint32(buf[regionTableOffset+8:], 1)
	entry := buf[regionTableOffset+regionTableHeaderSize:]
	copy(entry[0:16], metadataRegionGUID[:])
	binary.LittleEndian.PutUint64(entry[16:24], testMetadataOffset)
	binary.LittleEndian.PutUint32(entry[24:28], testMetadataLength)

	// 元数据区域：1 项，指向区域内的 0x100 偏移。
	copy(buf[testMetadataOffset:], "metadata")
	binary.LittleEndian.PutUint16(buf[testMetadataOffset+10:], 1)
	item := buf[testMetadataOffset+metadataHeaderSize:]
	copy(item[0:16], virtualSizeGUID[:])
	binary.LittleEndian.PutUint32(item[16:20], 0x100)
	binary.LittleEndian.PutUint32(item[20:24], 8)
	binary.LittleEndian.PutUint64(buf[testMetadataOffset+0x100:], uint64(size))

	path := filepath.Join(t.TempDir(), "disk.vhdx")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("写测试 VHDX 失败: %v", err)
	}
	return path
}

func TestVirtualSize(t *testing.T) {
	// 刻意用一个"不是 1MiB 对齐"的值：这正是按文件大小估算时会出现的样子，
	// 解析器必须原样读出来，而不是替调用方取整。
	const want = int64(198870176638)
	path := writeTestVHDX(t, want, regionTableOffset1)

	got, err := VirtualSize(path)
	if err != nil {
		t.Fatalf("读取标称容量失败: %v", err)
	}
	if got != want {
		t.Fatalf("标称容量应为 %d，实际 %d", want, got)
	}
}

func TestVirtualSizeFallsBackToBackupRegionTable(t *testing.T) {
	const want = int64(64 << 30)
	path := writeTestVHDX(t, want, regionTableOffset2)

	got, err := VirtualSize(path)
	if err != nil {
		t.Fatalf("主区域表不可用时应退回备份区域表: %v", err)
	}
	if got != want {
		t.Fatalf("标称容量应为 %d，实际 %d", want, got)
	}
}

func TestVirtualSizeRejectsNonVHDX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.vhdx")
	if err := os.WriteFile(path, []byte("not a vhdx file at all"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	if _, err := VirtualSize(path); !errors.Is(err, ErrNotVHDX) {
		t.Fatalf("非 VHDX 文件应返回 ErrNotVHDX，实际 %v", err)
	}
}

func TestVirtualSizeRejectsTruncatedFile(t *testing.T) {
	path := writeTestVHDX(t, 64<<30, regionTableOffset1)
	// 截断到区域表之后、元数据区域之前：签名还在，但元数据读不到。
	if err := os.Truncate(path, testMetadataOffset/2); err != nil {
		t.Fatalf("截断文件失败: %v", err)
	}

	if _, err := VirtualSize(path); err == nil {
		t.Fatal("截断的 VHDX 不应返回成功")
	}
}
