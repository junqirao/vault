// Package vhdx 直接读取 VHDX 文件的少量元数据，不依赖 Hyper-V / VirtDisk API。
//
// 为什么需要它：客户端按**标称容量**精确匹配挂载上来的 iSCSI 磁盘
// （见 winps/scripts/volume_find_iscsi_disk.ps1），而导入已有磁盘时如果旁边没有
// sidecar 元数据，就只能拿文件大小去猜。VHDX 是稀疏文件，文件大小与标称容量
// 没有任何必然关系（可差几个字节，也可差几个数量级），猜出来的容量会让这块库
// **永远挂载不上**。VHDX 头部本身就写着标称容量，直接读出来即可。
//
// 这里只做只读解析：读文件头 + 区域表（region table）+ 元数据区域（metadata region），
// 不修改文件、不需要管理员权限，也不要求磁盘处于离线状态。
package vhdx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// ErrNotVHDX 表示文件不是 VHDX（签名不符）。上层据此决定"不适用"，而不是报错。
var ErrNotVHDX = errors.New("vhdx: 不是 VHDX 文件")

const (
	// fileSignature VHDX 文件头 8 字节标识。
	fileSignature = "vhdxfile"
	// regionTableOffset1 / regionTableOffset2 主、备区域表在文件中的固定偏移。
	regionTableOffset1 = 192 * 1024
	regionTableOffset2 = 256 * 1024
	// regionTableHeaderSize 区域表头大小（signature + checksum + entry count + reserved）。
	regionTableHeaderSize = 16
	// regionEntrySize 单个区域表项大小（guid + file offset + length + flags）。
	regionEntrySize = 32
	// metadataHeaderSize 元数据区域头大小（signature + reserved + entry count + reserved）。
	metadataHeaderSize = 32
	// metadataEntrySize 单个元数据表项大小（guid + offset + length + flags + reserved）。
	metadataEntrySize = 32
	// maxEntries 区域表固定 64KiB、元数据区域也按 64KiB 量级：超过这个数只可能是坏文件。
	maxEntries = 4096
)

// metadataRegionGUID 是元数据区域在区域表里的 GUID（8B7CA206-4790-4B9A-B8FE-575F050F886E）。
//
// GUID 在文件里按"前 3 段小端、后 8 字节原样"存放，因此这里是转换后的字节序。
var metadataRegionGUID = [16]byte{
	0x06, 0xA2, 0x7C, 0x8B, 0x90, 0x47, 0x9A, 0x4B,
	0xB8, 0xFE, 0x57, 0x5F, 0x05, 0x0F, 0x88, 0x6E,
}

// virtualSizeGUID 是"虚拟磁盘大小"元数据项的 GUID（2FA54224-CD1B-4876-B211-5DBED83BF4B8）。
var virtualSizeGUID = [16]byte{
	0x24, 0x42, 0xA5, 0x2F, 0x1B, 0xCD, 0x76, 0x48,
	0xB2, 0x11, 0x5D, 0xBE, 0xD8, 0x3B, 0xF4, 0xB8,
}

// VirtualSize 返回 VHDX 的标称容量（字节，即 VirtualDiskSize）。
//
// 文件不存在、不是 VHDX、结构损坏时返回错误；调用方应把错误当成"读不到"，
// 而不是让整个导入/挂载流程失败（容量只是辅助信息，猜错才是大问题）。
func VirtualSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, ErrNotVHDX
	}
	fileSize := info.Size()

	var signature [len(fileSignature)]byte
	if err := readAt(f, signature[:], 0); err != nil {
		return 0, fmt.Errorf("vhdx: 读取文件头失败: %w", err)
	}
	if string(signature[:]) != fileSignature {
		return 0, ErrNotVHDX
	}

	regionOffset, regionLength, err := findMetadataRegion(f, fileSize)
	if err != nil {
		return 0, err
	}
	return readVirtualSize(f, regionOffset, regionLength)
}

// findMetadataRegion 从主区域表（读不到时退回备份区域表）里找出元数据区域的位置。
func findMetadataRegion(f *os.File, fileSize int64) (offset, length int64, err error) {
	for _, tableOffset := range []int64{regionTableOffset1, regionTableOffset2} {
		header := make([]byte, regionTableHeaderSize)
		if readAt(f, header, tableOffset) != nil {
			continue
		}
		if string(header[0:4]) != "regi" {
			continue
		}
		count := int(binary.LittleEndian.Uint32(header[8:12]))
		if count <= 0 || count > maxEntries {
			continue
		}
		entries := make([]byte, count*regionEntrySize)
		if readAt(f, entries, tableOffset+regionTableHeaderSize) != nil {
			continue
		}
		for i := 0; i < count; i++ {
			entry := entries[i*regionEntrySize : (i+1)*regionEntrySize]
			if !bytes.Equal(entry[0:16], metadataRegionGUID[:]) {
				continue
			}
			offset = int64(binary.LittleEndian.Uint64(entry[16:24]))
			length = int64(binary.LittleEndian.Uint32(entry[24:28]))
			if offset <= 0 || length < metadataHeaderSize || offset+length > fileSize {
				return 0, 0, fmt.Errorf("vhdx: 元数据区域越界（offset=%d length=%d file=%d）", offset, length, fileSize)
			}
			return offset, length, nil
		}
	}
	return 0, 0, errors.New("vhdx: 未找到元数据区域")
}

// readVirtualSize 在元数据区域里定位 VirtualDiskSize 项并读出 8 字节小端整数。
func readVirtualSize(f *os.File, regionOffset, regionLength int64) (int64, error) {
	header := make([]byte, metadataHeaderSize)
	if err := readAt(f, header, regionOffset); err != nil {
		return 0, fmt.Errorf("vhdx: 读取元数据区域头失败: %w", err)
	}
	if string(header[0:8]) != "metadata" {
		return 0, errors.New("vhdx: 元数据区域签名不符")
	}
	count := int(binary.LittleEndian.Uint16(header[10:12]))
	if count <= 0 || count > maxEntries {
		return 0, errors.New("vhdx: 元数据项数量非法")
	}

	entries := make([]byte, count*metadataEntrySize)
	if err := readAt(f, entries, regionOffset+metadataHeaderSize); err != nil {
		return 0, fmt.Errorf("vhdx: 读取元数据表失败: %w", err)
	}
	for i := 0; i < count; i++ {
		entry := entries[i*metadataEntrySize : (i+1)*metadataEntrySize]
		if !bytes.Equal(entry[0:16], virtualSizeGUID[:]) {
			continue
		}
		itemOffset := int64(binary.LittleEndian.Uint32(entry[16:20]))
		itemLength := int64(binary.LittleEndian.Uint32(entry[20:24]))
		// 偏移量相对元数据区域起点，而不是文件起点。
		if itemLength < 8 || itemOffset < 0 || itemOffset+8 > regionLength {
			return 0, errors.New("vhdx: 虚拟磁盘大小项越界")
		}
		var raw [8]byte
		if err := readAt(f, raw[:], regionOffset+itemOffset); err != nil {
			return 0, fmt.Errorf("vhdx: 读取虚拟磁盘大小失败: %w", err)
		}
		size := binary.LittleEndian.Uint64(raw[:])
		if size == 0 || size > math.MaxInt64 {
			return 0, fmt.Errorf("vhdx: 虚拟磁盘大小非法: %d", size)
		}
		return int64(size), nil
	}
	return 0, errors.New("vhdx: 未找到虚拟磁盘大小项")
}

// readAt 从固定偏移读满 buf；读不满（文件被截断/结构损坏）返回错误。
//
// 单独包一层是为了让"短读"和"越界"都变成同一个清晰的错误，而不是把
// io.EOF 传出去让调用方误以为文件正常结束。
func readAt(f *os.File, buf []byte, offset int64) error {
	if offset < 0 {
		return errors.New("vhdx: 负偏移")
	}
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("vhdx: 读取不完整（期望 %d 字节，实际 %d）", len(buf), n)
	}
	return nil
}
