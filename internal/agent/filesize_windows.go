//go:build windows

package agent

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetCompressedFileSizeW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// fileAllocatedSize 返回文件在磁盘上**实际占用**的字节数。
//
// L2 文件是稀疏文件：逻辑大小可以到几十 GiB，但只写了少数槽位时实际只占几 GiB。
// os.FileInfo.Size() 报的是逻辑大小，不能拿来当缓存占用。
func fileAllocatedSize(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSizeW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&high)),
	)
	// GetCompressedFileSizeW 失败时返回 INVALID_FILE_SIZE（低 32 位全 1）。
	if uint32(low) == 0xFFFFFFFF {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return 0, errno
		}
	}
	return int64(uint32(low)) | int64(high)<<32, nil
}
