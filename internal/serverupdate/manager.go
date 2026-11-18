package serverupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"vault/internal/config"
	"vault/internal/releasever"
	"vault/internal/version"
)

const (
	// checkCacheWindow 防止用户连续点击检查把 GitHub 匿名配额打满。
	checkCacheWindow = 2 * time.Minute
	// downloadTimeout 是单次下载的总上限；GitHub 大包通常几分钟内完成。
	downloadTimeout = 2 * time.Hour
)

// Manager 管理服务端自更新的状态、下载、暂存与应用。
type Manager struct {
	log *slog.Logger
	cfg *config.Watcher

	mu     sync.Mutex
	state  State
	latest *ReleaseInfo
	err    *UpdateError

	progress     *Progress
	lastResult   *LastResult
	checkedAt    time.Time
	stageRoot    string
	newExe       string
	downloadZip  string
	downloadPart string

	cancelDownload context.CancelFunc
	downloading    bool
	lastProgressAt int64
}

// NewManager 构造服务端更新管理器，并读取上一次更新的结果。
func NewManager(log *slog.Logger, cfg *config.Watcher) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, cfg: cfg, state: StateIdle}
	m.loadLastResult()
	// 新服务端启动后清理上一次更新残留的 helper/备份/暂存目录。
	// 下载目录不清理：失败的 .part 需要保留给断点续传。
	if m.lastResult != nil && m.lastResult.OK {
		base := m.updateDir()
		// pending.json 还在说明 helper 可能仍在收尾，不能动它的暂存目录。
		if _, err := os.Stat(PendingPath(base)); os.IsNotExist(err) {
			for _, dir := range []string{"helper", "backup", "staging"} {
				_ = os.RemoveAll(filepath.Join(base, dir))
			}
		}
	}
	return m
}

func (m *Manager) currentConfig() config.Config {
	if m.cfg != nil {
		if loaded := m.cfg.Current(); loaded != nil {
			return loaded.Raw
		}
	}
	return config.Config{}
}

func (m *Manager) configPath() string {
	if m.cfg != nil {
		return m.cfg.Path()
	}
	return ""
}

func (m *Manager) configDir() string {
	path := m.configPath()
	if strings.TrimSpace(path) == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Dir(path)
	}
	return filepath.Dir(abs)
}

func (m *Manager) updateDir() string {
	base := m.configDir()
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "update", "server")
}

func (m *Manager) installDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

func (m *Manager) currentExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe
	}
	return abs
}

// artifactsDir 与 internal/app.UpdateArtifactsDir 保持同一口径：
// 绝对路径原样使用，相对路径按配置文件目录解析。
func (m *Manager) artifactsDir() string {
	raw := m.currentConfig()
	dir := strings.TrimSpace(raw.Update.ArtifactsDir)
	if dir == "" {
		return filepath.Join(m.installDir(), "updates")
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	base := m.configDir()
	if base == "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return dir
	}
	return filepath.Join(base, dir)
}

func (m *Manager) currentInfo() CurrentInfo {
	return CurrentInfo{
		ReleaseVersion:  strings.TrimSpace(version.ReleaseVersion),
		ReleaseChannel:  normalizeChannel(version.ReleaseChannel),
		InternalVersion: version.Version,
		Platform:        runtime.GOOS,
		Arch:            runtime.GOARCH,
	}
}

// currentReleaseVersion 把“基础版本 + 通道”合成为可比较的 semver。
func currentReleaseVersion() (releasever.Version, error) {
	base := strings.TrimSpace(version.ReleaseVersion)
	if base == "" {
		return releasever.Version{}, fmt.Errorf("当前构建未注入 RELEASE_VERSION")
	}
	ch := normalizeChannel(version.ReleaseChannel)
	raw := base
	if ch != "stable" && !strings.Contains(base, "-") {
		raw = base + "-" + ch
	}
	return releasever.Parse(raw)
}

// Status 返回当前更新状态。
func (m *Manager) Status() Status {
	// helper 可能在“新服务端已经启动、旧管理器已经不存在”的窗口里写结果，
	// 因此每次查询都尝试从磁盘刷新一次，避免新服务端启动早期看不到 last_result。
	m.reloadLastResult()
	m.mu.Lock()
	defer m.mu.Unlock()
	raw := m.currentConfig()
	out := Status{
		Current:     m.currentInfo(),
		State:       m.state,
		Latest:      cloneRelease(m.latest),
		Progress:    cloneProgress(m.progress),
		Error:       cloneError(m.err),
		LastResult:  m.freshLastResultLocked(),
		RestartMode: strings.ToLower(strings.TrimSpace(raw.Update.RestartMode)),
		SystemdUnit: strings.TrimSpace(raw.Update.SystemdUnit),
	}
	if out.RestartMode == "" {
		out.RestartMode = "auto"
	}
	if !m.checkedAt.IsZero() {
		out.CheckedAt = m.checkedAt.UnixMilli()
	}
	return out
}

// Check 查询 GitHub Release。
func (m *Manager) Check(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if m.state == StateChecking || m.state == StateDownloaded || m.downloading || m.state == StateApplying || m.state == StateRestarting {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, fmt.Errorf("server_update.busy")
	}
	if !m.checkedAt.IsZero() && time.Since(m.checkedAt) < checkCacheWindow {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, nil
	}
	m.state = StateChecking
	m.err = nil
	m.mu.Unlock()

	current, err := currentReleaseVersion()
	if err != nil {
		return m.fail("server_update.version_unknown", err.Error())
	}
	opt := checkOptions{
		Current:     current,
		CurrentChan: normalizeChannel(version.ReleaseChannel),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
	}

	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	result, uerr := githubCheck(checkCtx, opt)
	if uerr != nil {
		return m.failUpdate(uerr)
	}

	m.mu.Lock()
	m.checkedAt = time.Now()
	m.err = nil
	if result.Info == nil {
		m.latest = nil
		m.state = StateIdle
	} else {
		m.latest = result.Info
		m.state = StateAvailable
	}
	status := m.statusLocked()
	m.mu.Unlock()
	return status, nil
}

// StartDownload 启动下载并在后台解压、校验。
func (m *Manager) StartDownload(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if m.state == StateChecking || m.downloading || m.state == StateApplying || m.state == StateRestarting {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, fmt.Errorf("server_update.busy")
	}
	if m.latest == nil {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, fmt.Errorf("server_update.not_ready")
	}
	latest := cloneRelease(m.latest)
	m.state = StateDownloading
	m.err = nil
	m.progress = &Progress{TotalBytes: latest.AssetSize, Percent: 0}
	m.lastProgressAt = time.Now().UnixMilli()
	m.downloading = true
	dlCtx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	m.cancelDownload = cancel
	status := m.statusLocked()
	m.mu.Unlock()

	go m.downloadAndStage(dlCtx, latest)
	return status, nil
}

// CancelDownload 取消下载；已下载的 .part 保留，下次可续传。
func (m *Manager) CancelDownload(_ context.Context) (Status, error) {
	m.mu.Lock()
	if m.cancelDownload != nil {
		m.cancelDownload()
		m.cancelDownload = nil
	}
	m.downloading = false
	if m.state == StateDownloading {
		m.state = StateAvailable
		m.progress = nil
	}
	if m.state == StateDownloaded {
		_ = os.Remove(m.downloadZip)
		if m.stageRoot != "" {
			_ = os.RemoveAll(filepath.Dir(m.stageRoot))
		}
		m.stageRoot = ""
		m.newExe = ""
		m.downloadZip = ""
		m.state = StateAvailable
		m.progress = nil
	}
	status := m.statusLocked()
	m.mu.Unlock()
	return status, nil
}

func (m *Manager) downloadAndStage(ctx context.Context, latest *ReleaseInfo) {
	updateDir := m.updateDir()
	downloadDir := filepath.Join(updateDir, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		m.finishDownloadError("server_update.download_prepare", err)
		return
	}
	// 需要同时容纳：下载 zip、解压目录、旧 updates 备份、新 updates。
	// 资产大小通常约 120MB，但按 4 倍预留，避免备份阶段把磁盘写满。
	if free, err := freeDiskBytes(downloadDir); err == nil && latest.AssetSize > 0 {
		need := uint64(latest.AssetSize)*4 + 64<<20
		if free < need {
			m.finishDownloadError("server_update.disk_full", fmt.Errorf("可用空间不足：需要约 %d MiB，当前 %d MiB", need>>20, free>>20))
			return
		}
	}
	zipPath := filepath.Join(downloadDir, safeFilename(latest.AssetName))
	partPath := zipPath + downloadPartSuffix

	sha := strings.ToLower(strings.TrimSpace(latest.SHA256))
	if sha == "" {
		sumSHA, uerr := githubSHA256ForAsset(ctx, latest.Tag, latest.AssetName)
		if uerr != nil {
			m.finishDownloadError("server_update.digest_unavailable", fmt.Errorf("GitHub 未返回资产摘要，且 SHA256SUMS.txt 不可用"))
			return
		}
		sha = sumSHA
		m.mu.Lock()
		if m.latest != nil {
			m.latest.SHA256 = sha
		}
		m.mu.Unlock()
	}

	client := downloadHTTPClient()
	resolve := func(rctx context.Context) (string, error) {
		info, uerr := m.refreshAsset(rctx, latest)
		if uerr != nil {
			return "", fmt.Errorf("%s", uerr.Code)
		}
		return info.AssetURL, nil
	}
	prog := func(received, total int64) { m.updateProgress(received, total) }
	if err := downloadAsset(ctx, client, latest.AssetURL, resolve, partPath, latest.AssetSize, sha, prog); err != nil {
		if ctx.Err() != nil {
			m.mu.Lock()
			if m.state == StateDownloading {
				m.state = StateAvailable
				m.progress = nil
			}
			m.downloading = false
			m.mu.Unlock()
			return
		}
		m.finishDownloadError("server_update.download_failed", err)
		return
	}
	_ = os.Remove(zipPath)
	if err := os.Rename(partPath, zipPath); err != nil {
		m.finishDownloadError("server_update.download_commit", err)
		return
	}
	if ctx.Err() != nil {
		m.mu.Lock()
		if m.state == StateDownloading {
			m.state = StateAvailable
			m.progress = nil
		}
		m.downloading = false
		m.mu.Unlock()
		return
	}

	// 解压到独立暂存目录，校验服务端程序与客户端更新清单。
	_ = os.RemoveAll(filepath.Join(updateDir, "staging"))
	stageDir := filepath.Join(updateDir, "staging", safeTag(latest.Tag))
	_ = os.RemoveAll(stageDir)
	if err := extractArchive(zipPath, stageDir); err != nil {
		_ = os.RemoveAll(stageDir)
		m.finishDownloadError("server_update.extract_failed", err)
		return
	}
	root, err := findPackageRoot(stageDir)
	if err != nil {
		_ = os.RemoveAll(stageDir)
		m.finishDownloadError("server_update.package_invalid", err)
		return
	}
	if err := validateStage(root); err != nil {
		_ = os.RemoveAll(stageDir)
		m.finishDownloadError("server_update.package_invalid", err)
		return
	}
	if ctx.Err() != nil {
		_ = os.RemoveAll(stageDir)
		m.mu.Lock()
		if m.state == StateDownloading {
			m.state = StateAvailable
			m.progress = nil
		}
		m.downloading = false
		m.mu.Unlock()
		return
	}
	// 更新包里的客户端更新通道必须与服务端配置一致：否则替换后客户端会拿到
	// channel_unavailable，等于把客户端更新链路弄断。宁可在这里明确拒绝。
	manifestChannel, err := readManifestChannel(filepath.Join(root, "updates", "manifest.json"))
	if err != nil {
		_ = os.RemoveAll(stageDir)
		m.finishDownloadError("server_update.package_invalid", err)
		return
	}
	cfgChannel := normalizeChannel(m.currentConfig().Update.Channel)
	if manifestChannel != "" && !strings.EqualFold(manifestChannel, cfgChannel) {
		_ = os.RemoveAll(stageDir)
		m.finishDownloadError("server_update.channel_mismatch", fmt.Errorf("更新包通道 %s 与服务端配置 update.channel=%s 不一致", manifestChannel, cfgChannel))
		return
	}

	m.mu.Lock()
	m.state = StateDownloaded
	m.downloading = false
	m.cancelDownload = nil
	m.downloadZip = zipPath
	m.stageRoot = root
	m.newExe = filepath.Join(root, serverExeName())
	m.progress = &Progress{ReceivedBytes: latest.AssetSize, TotalBytes: latest.AssetSize, Percent: 100}
	m.mu.Unlock()
	m.log.Info("服务端更新包已下载并校验完成", "version", latest.Version, "zip", zipPath)
}

func (m *Manager) refreshAsset(ctx context.Context, latest *ReleaseInfo) (*ReleaseInfo, *UpdateError) {
	return githubReleaseByTag(ctx, latest.Tag)
}

func (m *Manager) updateProgress(received, total int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.progress == nil {
		m.progress = &Progress{}
	}
	now := time.Now()
	prevReceived := m.progress.ReceivedBytes
	m.progress.ReceivedBytes = received
	if total > 0 {
		m.progress.TotalBytes = total
		m.progress.Percent = int(min64(100, received*100/total))
	} else {
		m.progress.Percent = -1
	}
	// 用两次回调之间的差值估算速度；回调间隔由 256KiB 写入粒度决定，足够平滑。
	if elapsed := now.UnixMilli() - m.lastProgressAt; elapsed >= 500 && elapsed > 0 {
		delta := received - prevReceived
		if delta > 0 {
			m.progress.SpeedBytesPerSec = delta * 1000 / elapsed
		}
		m.lastProgressAt = now.UnixMilli()
	}
}

func (m *Manager) finishDownloadError(code string, err error) {
	m.mu.Lock()
	m.state = StateFailed
	m.downloading = false
	m.cancelDownload = nil
	m.err = &UpdateError{Code: code, Message: err.Error()}
	m.mu.Unlock()
	m.log.Warn("服务端更新下载失败", "code", code, "error", err)
}

// PrepareApply 校验暂存包、写 pending 并启动 helper。
//
// 注意：这里只负责把 helper 拉起来，不会自己退出；HTTP handler 写完响应后
// 再调用 RequestShutdown，让旧服务端优雅停机。
func (m *Manager) PrepareApply(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if m.state != StateDownloaded || m.stageRoot == "" || m.newExe == "" || m.latest == nil {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, fmt.Errorf("server_update.not_ready")
	}
	latest := cloneRelease(m.latest)
	stageRoot := m.stageRoot
	newExe := m.newExe
	zipPath := m.downloadZip
	m.mu.Unlock()

	raw := m.currentConfig()
	if err := validateStage(stageRoot); err != nil {
		return m.fail("server_update.package_invalid", err.Error())
	}
	if _, err := os.Stat(newExe); err != nil {
		return m.fail("server_update.package_invalid", err.Error())
	}
	manifestChannel, err := readManifestChannel(filepath.Join(stageRoot, "updates", "manifest.json"))
	if err != nil {
		return m.fail("server_update.package_invalid", err.Error())
	}
	if cfgChannel := normalizeChannel(raw.Update.Channel); !strings.EqualFold(manifestChannel, cfgChannel) {
		return m.fail("server_update.channel_mismatch", fmt.Sprintf("更新包通道 %s 与服务端配置 update.channel=%s 不一致", manifestChannel, cfgChannel))
	}

	restartMode := strings.ToLower(strings.TrimSpace(raw.Update.RestartMode))
	if restartMode == "" {
		restartMode = "auto"
	}
	useSystemd := restartMode == "systemd" || (restartMode == "auto" && detectedSystemd())
	if restartMode == "helper" || restartMode == "manual" {
		useSystemd = false
	}
	if useSystemd && runtime.GOOS != "linux" {
		return m.fail("server_update.unsupported", "systemd 重启模式仅支持 Linux")
	}

	installDir := m.installDir()
	updateDir := m.updateDir()
	if err := os.MkdirAll(updateDir, 0o755); err != nil {
		return m.fail("server_update.prepare_failed", err.Error())
	}
	pending := &Pending{
		ParentPID:    os.Getpid(),
		FromVersion:  version.ReleaseVersion,
		ToVersion:    latest.Version,
		CurrentExe:   m.currentExe(),
		NewExe:       newExe,
		StageRoot:    stageRoot,
		ArtifactsDir: m.artifactsDir(),
		InstallDir:   installDir,
		UpdateDir:    updateDir,
		WorkDir:      mustGetwd(),
		LogPath:      filepath.Join(m.configDir(), "logs", "server-update.log"),
		Args:         append([]string(nil), os.Args[1:]...),
		DownloadPath: zipPath,
		Listen:       raw.HTTP.Listen,
		RestartMode:  restartMode,
		SystemdUnit:  raw.Update.SystemdUnit,
		UseSystemd:   useSystemd,
	}
	if pending.Listen == "" {
		pending.Listen = raw.HTTP.Listen
	}
	pendingPath := PendingPath(updateDir)
	if err := WritePending(pendingPath, pending); err != nil {
		return m.fail("server_update.prepare_failed", err.Error())
	}

	helperPath := filepath.Join(updateDir, "helper", serverExeName())
	if err := os.MkdirAll(filepath.Dir(helperPath), 0o755); err != nil {
		return m.fail("server_update.prepare_failed", err.Error())
	}
	if err := copyFile(pending.CurrentExe, helperPath, 0o755); err != nil {
		return m.fail("server_update.prepare_failed", "复制更新助手失败: "+err.Error())
	}
	helperLog := filepath.Join(updateDir, "helper.log")
	_ = os.Remove(helperStartedPath(updateDir))
	pid, err := startHelperProcess(helperPath, []string{"apply-update", "-pending", pendingPath}, updateDir, helperLog, useSystemd)
	if err != nil {
		return m.fail("server_update.prepare_failed", "启动更新助手失败: "+err.Error())
	}
	if err := waitHelperStarted(updateDir, 10*time.Second); err != nil {
		if pid > 0 {
			_ = killProcess(pid)
		}
		return m.fail("server_update.prepare_failed", "更新助手未能读取任务: "+err.Error())
	}
	m.log.Info("服务端更新助手已启动", "pid", pid, "version", latest.Version)

	m.mu.Lock()
	m.state = StateApplying
	status := m.statusLocked()
	m.mu.Unlock()
	// 兜底：helper 若因权限/进程问题没能替换成功，旧服务端仍然活着；3 分钟后
	// 把状态从“应用中”收回，避免界面永久转圈。
	go m.watchApplyTimeout()
	return status, nil
}

func (m *Manager) watchApplyTimeout() {
	time.Sleep(3 * time.Minute)
	m.mu.Lock()
	if m.state != StateApplying {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.reloadLastResult()
	m.mu.Lock()
	if m.state == StateApplying {
		m.state = StateFailed
		msg := "更新助手未能在预期时间内完成"
		if m.lastResult != nil && m.lastResult.Error != "" {
			msg = m.lastResult.Error
		}
		m.err = &UpdateError{Code: "server_update.prepare_failed", Message: msg}
	}
	m.mu.Unlock()
}

// StartAutoCheck 启动后台周期检查（仅在配置开启且当前版本已注入时）。
func (m *Manager) StartAutoCheck(ctx context.Context) {
	raw := m.currentConfig()
	if !raw.Update.ServerAutoCheck {
		return
	}
	interval := raw.Update.ServerCheckInterval.Std()
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	go func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				checkCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
				_, _ = m.Check(checkCtx)
				cancel()
				timer.Reset(interval)
			}
		}
	}()
}

func (m *Manager) fail(code, message string) (Status, error) {
	return m.failUpdate(&UpdateError{Code: code, Message: message})
}

func (m *Manager) failUpdate(uerr *UpdateError) (Status, error) {
	m.mu.Lock()
	m.state = StateFailed
	m.err = uerr
	status := m.statusLocked()
	m.mu.Unlock()
	if uerr != nil {
		m.log.Warn("服务端更新检查失败", "code", uerr.Code, "message", uerr.Message)
	}
	return status, fmt.Errorf("%s", uerr.Code)
}

func (m *Manager) statusLocked() Status {
	raw := m.currentConfig()
	out := Status{
		Current:     m.currentInfo(),
		State:       m.state,
		Latest:      cloneRelease(m.latest),
		Progress:    cloneProgress(m.progress),
		Error:       cloneError(m.err),
		LastResult:  m.freshLastResultLocked(),
		RestartMode: strings.ToLower(strings.TrimSpace(raw.Update.RestartMode)),
		SystemdUnit: strings.TrimSpace(raw.Update.SystemdUnit),
	}
	if out.RestartMode == "" {
		out.RestartMode = "auto"
	}
	if !m.checkedAt.IsZero() {
		out.CheckedAt = m.checkedAt.UnixMilli()
	}
	return out
}

func (m *Manager) loadLastResult() {
	path := filepath.Join(m.updateDir(), "last-result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var result LastResult
	if err := json.Unmarshal(data, &result); err == nil {
		m.lastResult = &result
	}
}

func (m *Manager) reloadLastResult() {
	path := filepath.Join(m.updateDir(), "last-result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var result LastResult
	if err := json.Unmarshal(data, &result); err != nil {
		return
	}
	m.mu.Lock()
	m.lastResult = &result
	m.mu.Unlock()
}

func (m *Manager) freshLastResultLocked() *LastResult {
	if m.lastResult == nil {
		return nil
	}
	if m.lastResult.FinishedAt > 0 && time.Since(time.UnixMilli(m.lastResult.FinishedAt)) > LastResultLifetime {
		return nil
	}
	out := *m.lastResult
	return &out
}

func cloneRelease(in *ReleaseInfo) *ReleaseInfo {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneProgress(in *Progress) *Progress {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneError(in *UpdateError) *UpdateError {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func safeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "v")
	tag = strings.NewReplacer("/", "_", "\\", "_", ":", "_", "..", "_").Replace(tag)
	if tag == "" {
		return "unknown"
	}
	return tag
}

// safeFilename 只保留资产名的最后一段，避免 GitHub asset 名里的路径分隔符写到目录外。
func safeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, ":", "_")
	if name == "" || name == "." || name == ".." {
		return "server-update.bin"
	}
	return name
}

func waitHelperStarted(updateDir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(helperStartedPath(updateDir)); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 helper.started 超时")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// readManifestChannel 读取更新包内客户端清单的通道。
func readManifestChannel(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var manifest struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(manifest.Channel)
	if raw == "" {
		return "", fmt.Errorf("更新清单缺少 channel")
	}
	return normalizeChannel(raw), nil
}

// detectedSystemd 判断当前进程是否由 systemd 托管。
//
// 只认 INVOCATION_ID：在 systemd 机器上手动执行 Vault-Server 时 /run/systemd/system
// 也存在，但此时服务并不受 systemd 管理，走 systemctl restart 会失败。
func detectedSystemd() bool {
	return strings.TrimSpace(os.Getenv("INVOCATION_ID")) != ""
}

func downloadHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 45 * time.Second
	return &http.Client{Transport: transport, Timeout: 0}
}
