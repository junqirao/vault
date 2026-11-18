//go:build windows

package serverupdate

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// freeDiskBytes 返回路径所在卷的可用字节数。
func freeDiskBytes(path string) (uint64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}
