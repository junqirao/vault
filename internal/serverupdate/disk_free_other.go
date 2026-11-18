//go:build !windows

package serverupdate

import "syscall"

// freeDiskBytes 返回路径所在文件系统的可用字节数。
func freeDiskBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
