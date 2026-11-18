//go:build !windows

package serverupdate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// startDetached 以新会话方式启动子进程，stdout/stderr 写入 logPath。
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

// waitProcessExit 轮询 PID 直到退出或超时。
func waitProcessExit(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		if processIsZombie(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待旧服务端退出超时")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// processIsZombie 判断 PID 是否已经是僵尸进程。
//
// kill(pid, 0) 对僵尸仍返回成功；如果父进程没有及时 wait，helper 会白等到超时。
// 直接读 /proc/<pid>/stat 的第三个字段（state）判断更可靠。
func processIsZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// 格式：pid (comm) state ...；comm 可能含空格/括号，取最后一个 ')' 之后。
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 >= len(s) {
		return false
	}
	return s[idx+2] == 'Z'
}

// processAlive 判断 PID 是否仍在运行（僵尸进程视为已退出）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return !processIsZombie(pid)
}

// killProcess 强制结束进程（仅 helper 回滚时使用）。
func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
