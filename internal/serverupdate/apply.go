package serverupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Pending 是服务端写给 helper 的替换任务（`update/server/pending.json`）。
type Pending struct {
	ParentPID int `json:"parent_pid"`

	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`

	CurrentExe   string   `json:"current_exe"`
	NewExe       string   `json:"new_exe"`
	StageRoot    string   `json:"stage_root"`
	ArtifactsDir string   `json:"artifacts_dir"`
	InstallDir   string   `json:"install_dir"`
	UpdateDir    string   `json:"update_dir"`
	WorkDir      string   `json:"work_dir"`
	LogPath      string   `json:"log_path"`
	Args         []string `json:"args"`

	DownloadPath string `json:"download_path,omitempty"`
	Listen       string `json:"listen,omitempty"`

	RestartMode string `json:"restart_mode,omitempty"`
	SystemdUnit string `json:"systemd_unit,omitempty"`
	UseSystemd  bool   `json:"use_systemd,omitempty"`
}

// PendingPath 返回 helper 任务文件路径。
func PendingPath(updateDir string) string { return filepath.Join(updateDir, "pending.json") }

// helperStartedPath 是 helper 已读取 pending 的标记，供服务端在停机前确认替换器真的起来了。
func helperStartedPath(updateDir string) string { return filepath.Join(updateDir, "helper.started") }

// WritePending 原子写入 pending.json。
func WritePending(path string, p *Pending) error {
	return writeJSONAtomic(path, p)
}

// ReadPending 读取 pending.json。
func ReadPending(path string) (*Pending, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RunApply 是 `Vault-Server apply-update -pending <path>` 的入口。
//
// 它在旧服务端退出后执行：备份旧 exe → 替换程序/脚本/updates → 启动新服务端 →
// 健康检查；失败则恢复旧版本。所有结果写入 last-result.json。
func RunApply(pendingPath string) error {
	p, err := ReadPending(pendingPath)
	if err != nil {
		return fmt.Errorf("读取 pending 失败: %w", err)
	}
	_ = os.WriteFile(helperStartedPath(p.UpdateDir), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
	logger, closeLog := newApplyLogger(p.LogPath)
	defer closeLog()
	logger.Info("开始应用服务端更新", "from", p.FromVersion, "to", p.ToVersion, "pending", pendingPath)

	result := &LastResult{FromVersion: p.FromVersion, ToVersion: p.ToVersion}
	if err := waitProcessExit(p.ParentPID, 120*time.Second); err != nil {
		result.Error = "等待旧服务端退出失败: " + err.Error()
		writeLastResult(p.UpdateDir, result)
		return errors.New(result.Error)
	}

	rollback, err := replaceInstall(p, logger)
	if err != nil {
		result.Error = "替换文件失败: " + err.Error()
		// 替换阶段失败：尽量把已经改动的文件恢复。
		if rollback != nil {
			if rbErr := rollback(); rbErr != nil {
				result.Error += "；回滚失败: " + rbErr.Error()
			}
		}
		// 旧进程已经退出，回滚后必须把旧版本重新拉起来。
		if _, startErr := startServer(p, logger); startErr != nil {
			result.Error += "；旧版本重启失败: " + startErr.Error()
		} else if waitErr := waitListen(p.Listen, 60*time.Second); waitErr != nil {
			result.Error += "；旧版本健康检查失败: " + waitErr.Error()
		}
		writeLastResult(p.UpdateDir, result)
		return errors.New(result.Error)
	}
	if p.RestartMode == "manual" {
		// 外部 supervisor 或运维会负责启动；helper 只负责把文件换好。
		result.OK = true
		result.FinishedAt = time.Now().UnixMilli()
		writeLastResult(p.UpdateDir, result)
		cleanupAfterSuccess(p, logger)
		logger.Info("服务端文件已替换，restart_mode=manual，等待人工/外部进程启动")
		return nil
	}

	newPID, startErr := startServer(p, logger)
	if startErr == nil {
		if waitErr := waitListen(p.Listen, 90*time.Second); waitErr != nil {
			startErr = waitErr
		} else {
			// 监听成功后再观察几秒：有些启动错误（配置/迁移）会在 HTTP 起来后立刻退出。
			time.Sleep(2 * time.Second)
			if newPID > 0 && !processAlive(newPID) {
				startErr = fmt.Errorf("新服务端启动后立即退出")
			}
		}
	}
	if startErr != nil {
		logger.Error("新服务端启动失败，开始回滚", "error", startErr)
		result.Error = "新服务端启动失败: " + startErr.Error()
		if newPID > 0 {
			_ = killProcess(newPID)
		}
		if rbErr := rollback(); rbErr != nil {
			result.Error += "；回滚失败: " + rbErr.Error()
			writeLastResult(p.UpdateDir, result)
			return errors.New(result.Error)
		}
		// 恢复旧版本后重新启动。
		oldPID, oldErr := startServer(p, logger)
		if oldErr != nil {
			result.Error += "；旧版本重启失败: " + oldErr.Error()
		} else if waitErr := waitListen(p.Listen, 60*time.Second); waitErr != nil {
			result.Error += "；旧版本健康检查失败: " + waitErr.Error()
		} else {
			_ = oldPID
		}
		writeLastResult(p.UpdateDir, result)
		return errors.New(result.Error)
	}

	// 成功：清理备份、下载包与解压目录。
	result.OK = true
	result.FinishedAt = time.Now().UnixMilli()
	writeLastResult(p.UpdateDir, result)
	cleanupAfterSuccess(p, logger)
	logger.Info("服务端更新完成", "to", p.ToVersion)
	return nil
}

// replaceInstall 执行替换并返回回滚函数。
func replaceInstall(p *Pending, logger *slog.Logger) (func() error, error) {
	if p.CurrentExe == "" || p.NewExe == "" || p.ArtifactsDir == "" {
		return nil, fmt.Errorf("pending 缺少必要路径")
	}
	backupDir := filepath.Join(p.UpdateDir, "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return nil, err
	}
	stamp := time.Now().Format("20060102-150405") + fmt.Sprintf("-%d", os.Getpid())

	// 1) 备份并替换 exe。
	exeBackup := filepath.Join(backupDir, filepath.Base(p.CurrentExe)+"."+stamp)
	if err := moveFile(p.CurrentExe, exeBackup); err != nil {
		return nil, fmt.Errorf("备份旧服务端失败: %w", err)
	}
	if err := copyFile(p.NewExe, p.CurrentExe, 0o755); err != nil {
		_ = moveFile(exeBackup, p.CurrentExe)
		return nil, fmt.Errorf("写入新服务端失败: %w", err)
	}

	restoreExe := func() error {
		_ = os.Remove(p.CurrentExe)
		return moveFile(exeBackup, p.CurrentExe)
	}

	// 2) config.example.yaml（只更新模板，不碰 config.yaml）。
	configExample := filepath.Join(p.StageRoot, "config.example.yaml")
	if _, err := os.Stat(configExample); err == nil {
		if err := copyFile(configExample, filepath.Join(p.InstallDir, "config.example.yaml"), 0o644); err != nil {
			logger.Warn("更新 config.example.yaml 失败", "error", err)
		}
	}

	// 3) scripts/：先整体换掉，失败时恢复旧目录。
	var scriptsBackup string
	srcScripts := filepath.Join(p.StageRoot, "scripts")
	if info, err := os.Stat(srcScripts); err == nil && info.IsDir() {
		dstScripts := filepath.Join(p.InstallDir, "scripts")
		backupCandidate := filepath.Join(backupDir, "scripts."+stamp)
		if _, statErr := os.Stat(dstScripts); statErr == nil {
			if err := moveDir(dstScripts, backupCandidate); err != nil {
				_ = restoreExe()
				return nil, fmt.Errorf("备份 scripts 失败: %w", err)
			}
			scriptsBackup = backupCandidate
		}
		if err := copyDir(srcScripts, dstScripts); err != nil {
			_ = os.RemoveAll(dstScripts)
			if scriptsBackup != "" {
				_ = moveDir(scriptsBackup, dstScripts)
			}
			_ = restoreExe()
			return nil, fmt.Errorf("替换 scripts 失败: %w", err)
		}
	}

	// 4) updates/：先复制到临时目录，再原子替换 artifacts_dir。
	var updatesBackup string
	srcUpdates := filepath.Join(p.StageRoot, "updates")
	if info, err := os.Stat(srcUpdates); err == nil && info.IsDir() {
		backupCandidate := filepath.Join(backupDir, "updates."+stamp)
		if _, statErr := os.Stat(p.ArtifactsDir); statErr == nil {
			if err := moveDir(p.ArtifactsDir, backupCandidate); err != nil {
				_ = restoreExe()
				return nil, fmt.Errorf("备份 updates 失败: %w", err)
			}
			updatesBackup = backupCandidate
		}
		if err := copyDir(srcUpdates, p.ArtifactsDir); err != nil {
			_ = os.RemoveAll(p.ArtifactsDir)
			if updatesBackup != "" {
				_ = moveDir(updatesBackup, p.ArtifactsDir)
			}
			if scriptsBackup != "" {
				_ = os.RemoveAll(filepath.Join(p.InstallDir, "scripts"))
				_ = moveDir(scriptsBackup, filepath.Join(p.InstallDir, "scripts"))
			}
			_ = restoreExe()
			return nil, fmt.Errorf("替换 updates 失败: %w", err)
		}
	}

	rollback := func() error {
		var errs []string
		if updatesBackup != "" {
			_ = os.RemoveAll(p.ArtifactsDir)
			if err := moveDir(updatesBackup, p.ArtifactsDir); err != nil {
				errs = append(errs, "恢复 updates: "+err.Error())
			}
		} else {
			// 原来没有 artifacts_dir：把新写入的目录清掉，回到“不存在”的状态。
			if err := os.RemoveAll(p.ArtifactsDir); err != nil {
				errs = append(errs, "清理新 updates: "+err.Error())
			}
		}
		if scriptsBackup != "" {
			dst := filepath.Join(p.InstallDir, "scripts")
			_ = os.RemoveAll(dst)
			if err := moveDir(scriptsBackup, dst); err != nil {
				errs = append(errs, "恢复 scripts: "+err.Error())
			}
		} else if _, err := os.Stat(filepath.Join(p.InstallDir, "scripts")); err == nil {
			if err := os.RemoveAll(filepath.Join(p.InstallDir, "scripts")); err != nil {
				errs = append(errs, "清理新 scripts: "+err.Error())
			}
		}
		if err := restoreExe(); err != nil {
			errs = append(errs, "恢复服务端程序: "+err.Error())
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "；"))
		}
		return nil
	}
	return rollback, nil
}

func startServer(p *Pending, logger *slog.Logger) (int, error) {
	if p.RestartMode == "manual" {
		return 0, fmt.Errorf("restart_mode=manual，需要人工启动服务端")
	}
	if p.UseSystemd {
		unit := strings.TrimSpace(p.SystemdUnit)
		if unit == "" {
			unit = "vault-server.service"
		}
		logger.Info("通过 systemd 重启服务端", "unit", unit)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("systemctl restart %s 失败: %w（%s）", unit, err, strings.TrimSpace(string(out)))
		}
		return 0, nil
	}
	args := sanitizeServerArgs(p.Args)
	logger.Info("启动新服务端", "exe", p.CurrentExe, "args", strings.Join(args, " "))
	return startDetached(p.CurrentExe, args, p.WorkDir, p.LogPath)
}

func sanitizeServerArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	hasDetached := false
	for _, a := range args {
		if a == "--background" || a == "-background" {
			continue
		}
		if a == "-"+detachedFlag {
			hasDetached = true
		}
		out = append(out, a)
	}
	if !hasDetached {
		out = append(out, "-"+detachedFlag)
	}
	return out
}

const detachedFlag = "detached"

func waitListen(listen string, timeout time.Duration) error {
	addr := dialAddress(listen)
	if addr == "" {
		time.Sleep(2 * time.Second)
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待服务端监听 %s 超时", addr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func dialAddress(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func cleanupAfterSuccess(p *Pending, logger *slog.Logger) {
	_ = os.Remove(PendingPath(p.UpdateDir))
	_ = os.Remove(helperStartedPath(p.UpdateDir))
	if p.DownloadPath != "" {
		if err := os.Remove(p.DownloadPath); err != nil && !os.IsNotExist(err) {
			logger.Warn("删除下载的更新包失败", "path", p.DownloadPath, "error", err)
		}
	}
	for _, path := range []string{
		filepath.Join(p.UpdateDir, "backup"),
		filepath.Join(p.UpdateDir, "downloads"),
		p.StageRoot,
	} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			logger.Warn("清理更新临时目录失败", "path", path, "error", err)
		}
	}
}

func writeLastResult(updateDir string, result *LastResult) {
	if result.FinishedAt == 0 {
		result.FinishedAt = time.Now().UnixMilli()
	}
	_ = writeJSONAtomic(filepath.Join(updateDir, "last-result.json"), result)
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newApplyLogger(path string) (*slog.Logger, func()) {
	var writers []io.Writer
	var file *os.File
	if strings.TrimSpace(path) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				file = f
				writers = append(writers, f)
			}
		}
	}
	writers = append(writers, os.Stderr)
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() {
		if file != nil {
			_ = file.Close()
		}
	}
}

func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 跨卷 rename 会失败：退化为复制 + 删除。
	if err := copyFile(src, dst, 0o755); err != nil {
		return err
	}
	return os.Remove(src)
}

// moveDir 移动目录；跨卷时退化为复制 + 删除。
func moveDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if mode&0o111 != 0 {
		_ = os.Chmod(dst, mode)
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("更新包包含不支持的文件类型: %s", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		return copyFile(path, target, mode)
	})
}
