//go:build !windows

package agent

import "syscall"

// fileAllocatedSize 返回文件实际占用的磁盘块字节数（稀疏文件不能看逻辑大小）。
func fileAllocatedSize(path string) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Blocks) * 512, nil
}
