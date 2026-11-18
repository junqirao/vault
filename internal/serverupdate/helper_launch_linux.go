//go:build linux

package serverupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// startHelperProcess 在 Linux 上启动替换器。
//
// systemd 场景必须用 systemd-run 创建独立 unit：如果只是 setsid，helper 仍在旧服务的
// cgroup 里，旧服务停止时 systemd 会把 helper 一起杀掉，替换流程会半途而废。
func startHelperProcess(exe string, args []string, workDir, logPath string, useSystemd bool) (int, error) {
	if !useSystemd {
		return startDetached(exe, args, workDir, logPath)
	}
	unit := fmt.Sprintf("vault-server-update-%d-%d", time.Now().Unix(), os.Getpid())
	cmdArgs := []string{
		"--collect",
		"--unit", unit,
		"--description", "Vault server self-update helper",
		"--property=StandardOutput=null",
		"--property=StandardError=null",
		exe,
	}
	cmdArgs = append(cmdArgs, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run", cmdArgs...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("systemd-run 启动更新助手失败: %w（%s）", err, strings.TrimSpace(string(out)))
	}
	// systemd-run 不直接给出主 PID；helper 自己会写 last-result。
	return 0, nil
}
