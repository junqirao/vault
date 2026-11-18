//go:build windows

package serverupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// startDetached 以无控制台方式启动一个子进程，stdout/stderr 写入 logPath。
func startDetached(exe string, args []string, workDir, logPath string) (int, error) {
	var logFile *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				logFile = f
			}
		}
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = workDir
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	if logFile != nil {
		_ = logFile.Close()
	}
	return pid, nil
}

// waitProcessExit 等待指定 PID 退出；超时返回错误。
func waitProcessExit(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// 进程已经不存在：视为已退出。
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = uint32((120 * time.Second) / time.Millisecond)
	}
	event, err := windows.WaitForSingleObject(h, ms)
	if err != nil {
		return err
	}
	if event == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("等待旧服务端退出超时")
	}
	return nil
}

// processAlive 判断 PID 是否仍在运行。
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return event == uint32(windows.WAIT_TIMEOUT)
}

// killProcess 强制结束进程（仅 helper 回滚时使用）。
func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.TerminateProcess(h, 1)
}
