package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
	"vault/internal/updatepkg"
)

// 启动期更新器（Vault-Updator bootstrap）
//
// 背景：此前"检查更新 / 下载 / 解压"由**本地代理**完成，而代理本身又是被更新的对象之一。
// 于是更新链路天然依赖一个可能已经过期的本地服务，并且代理自更新与客户端本体更新是两条
// 独立通道：谁先完成都会出现"本地服务比客户端新/旧"的中间态（真实反馈）。
//
// 现在的契约（与 frontend/apps/client/electron/bootstrapUpdate.ts 对齐）：
//
//	Vault.exe（外壳）
//	  └─ 启动时**先**拉起 Vault-Updator.exe bootstrap
//	       ├─ 自己读客户端配置，找到更新源（匿名接口，不需要登录会话）
//	       ├─ 拉清单 → 验签 → 版本单调性 → 下载 → sha256 → 解压 → 原子激活
//	       ├─ 发现可用更新：解压到 <ProgramData>\Vault\client\<版本>\ 并写 current.json
//	       └─ 输出 ready，等外壳退出 → 启动新版本 <版本>\Vault.exe（整个应用重启）
//	  └─ 只有确认"没有更新 / 更新不可用"之后，外壳才启动本地服务 Vault-Agent.exe
//
// 关键点：client 产物是**整个程序目录**（Vault.exe + resources/app.asar + Vault-Agent.exe
// + Vault-Updator.exe）。前端与后端永远来自同一个已签名包，要么一起换、要么都不换，
// 不会再出现"前端换了、后端没换"或反过来的版本不一致。

// Bootstrap 事件名（stdout 一行一个 JSON 对象，供 Electron 主进程解析）。
const (
	BootstrapEventStatus = "status"
	BootstrapEventNone   = "none"
	BootstrapEventReady  = "ready"
	BootstrapEventError  = "error"
)

// Bootstrap 阶段（status 事件的 phase 字段）。
const (
	BootstrapPhaseChecking    = "checking"
	BootstrapPhaseDownloading = "downloading"
	BootstrapPhaseVerifying   = "verifying"
	BootstrapPhaseExtracting  = "extracting"
	BootstrapPhaseRestarting  = "restarting"
)

const (
	// bootstrapCheckTimeout 是"拉清单 + 签名"的总超时。
	//
	// 外壳停在启动页等这个结论：更新源不可达时绝不能把用户按在启动页上几分钟。
	// 拿不到结论就放行，后台不再重试（下一次启动会重新检查）。
	bootstrapCheckTimeout = 10 * time.Second
	// bootstrapStartupGuard 是新版启动后等待"前端已加载"的最长时间。
	//
	// 只看进程存活不够：新版 exe 可能起来了却在加载界面时白屏/崩溃，用户同样没有可用界面。
	// 新版 Electron 在界面加载完成后会写 update\boot-ok.json（版本 + 时间戳），
	// 更新器等到这份凭据才认为前后端都真正替换成功；超时则回退旧版本。
	bootstrapStartupGuard = 30 * time.Second
	// bootstrapBootOKFileName 是"新版本界面已成功加载"的凭据文件名。
	bootstrapBootOKFileName = "boot-ok.json"
	// bootstrapSettleDelay 是旧进程退出后、启动新进程前的稳定期（单实例锁/文件句柄释放）。
	//
	// Electron 的单实例锁在进程结束后由系统释放，但 Windows 的句柄回收偶尔滞后。
	// 太短会让新进程拿不到锁、走 takeover/静默退出，旧更新器又因"没等到 boot-ok"回退，
	// 用户看到的就是"更新了一下又回到旧版本"。宁可多等 0.7 秒。
	bootstrapSettleDelay = 1500 * time.Millisecond
	// bootstrapBlockedFileName 是"坏版本"记录文件名（与 client 目录同级）。
	bootstrapBlockedFileName = "blocked.json"
)

// BootstrapOptions 是启动期更新器的入参。
type BootstrapOptions struct {
	// ParentPID 是正在运行的客户端（Electron）PID；更新器等它退出后启动新版本。
	ParentPID int
	// CurrentVersion 是**正在运行的客户端版本**，作为版本单调性的基准。
	CurrentVersion string
	// ServerURL 更新源（通常取客户端配置里的活动服务端）。
	ServerURL string
	// CertSHA256 更新源证书指纹（TOFU 固定；为空时走系统默认链校验）。
	CertSHA256 string
	// Channel 更新通道；为空按 stable。
	Channel string
	// Timeout 等待旧客户端退出的超时。
	Timeout time.Duration
	// FallbackExe 新版本启动失败时回退的旧客户端主程序（通常是当前 Vault.exe）。
	FallbackExe string
	// ClientRoot 客户端本体根目录；为空时取 %ProgramData%\Vault\client。
	ClientRoot string
	// UpdateDir 更新工作目录；为空时取 %ProgramData%\Vault\update。
	UpdateDir string
	// NoRestart 只检查/准备，不等待退出也不启动新版本（排障与测试用）。
	NoRestart bool
}

// BootstrapEvent 是更新器输出给外壳的进度/结果事件。
type BootstrapEvent struct {
	Event string `json:"event"`
	Phase string `json:"phase,omitempty"`
	// Version / FromVersion 目标版本与当前版本。
	Version     string `json:"version,omitempty"`
	FromVersion string `json:"from_version,omitempty"`
	// Exe 新版本主程序路径（ready 事件）。
	Exe string `json:"exe,omitempty"`
	// Reason 稳定原因码（none/error 事件）：not_newer / no_artifact / bad_signature / ...
	Reason string `json:"reason,omitempty"`
	// Message 人类可读的错误信息（仅日志用，界面按 Reason 翻译）。
	Message string `json:"message,omitempty"`
	// ReceivedBytes / TotalBytes 下载进度。
	ReceivedBytes int64 `json:"received_bytes,omitempty"`
	TotalBytes    int64 `json:"total_bytes,omitempty"`
}

// BootstrapResult 是启动期更新器的结果。
type BootstrapResult struct {
	// Updated 为 true 表示新版本已激活并已启动（本进程退出后由新版本接管）。
	Updated bool
	// Version 目标版本（没有可用更新时为空）。
	Version string
	// Exe 新版本主程序路径。
	Exe string
}

// RunClientBootstrap 执行一次完整的启动期更新检查与安装。
//
// 返回值语义：
//   - Updated=true：新版本已解压、已写激活指针，并已启动新进程；
//   - Updated=false：没有需要安装的更新（或更新不可用），调用方应继续用当前版本启动；
//   - err != nil：更新流程失败。调用方仍应继续启动当前版本（绝不把用户锁在启动页）。
func RunClientBootstrap(ctx context.Context, opt BootstrapOptions, emit func(BootstrapEvent)) (BootstrapResult, error) {
	if emit == nil {
		emit = func(BootstrapEvent) {}
	}
	opt.CurrentVersion = strings.TrimSpace(opt.CurrentVersion)
	opt.ServerURL = strings.TrimSpace(opt.ServerURL)
	opt.CertSHA256 = strings.TrimSpace(opt.CertSHA256)
	opt.Channel = strings.TrimSpace(opt.Channel)
	opt.FallbackExe = strings.TrimSpace(opt.FallbackExe)
	if opt.CurrentVersion == "" {
		opt.CurrentVersion = defaultVersion("")
	}
	if opt.Channel == "" {
		opt.Channel = "stable"
	}
	if opt.Timeout <= 0 {
		opt.Timeout = DefaultUpdateTimeout
	}
	if opt.ClientRoot == "" {
		opt.ClientRoot = filepath.Join(DataDir(), clientDirName)
	}
	if opt.UpdateDir == "" {
		opt.UpdateDir = filepath.Join(DataDir(), updateDirName)
	}

	log := newApplyLogger(filepath.Join(opt.UpdateDir, updaterLogName), nil)
	// newApplyLogger 传入 nil 时只写文件；复用它的辅助函数（下载/清理）仍需要一个
	// 非空 slog.Logger，这里补一个丢弃输出的默认实例。
	if log.logger == nil {
		log.logger = slog.Default()
	}
	log.info("启动期更新器开始", "current", opt.CurrentVersion, "server", opt.ServerURL,
		"channel", opt.Channel, "pid", opt.ParentPID, "no_restart", opt.NoRestart)

	// 清理上次被中断留下的临时解压目录（更新器被结束/断电都会留下 *.tmp-* 目录）。
	cleanupClientTemp(opt.ClientRoot, log.logger)

	// 磁盘上可能已有"上次已解压好、只是没来得及重启"的版本。
	//
	// ⚠️ 不能一发现它就立刻切换：如果服务端已经发布了更新的版本，直接切到旧的就绪包会让
	// 新版本启动后再更新一次，用户看到的就是"窗口闪两下才到最新版"（真实反馈）。
	// 正确做法是：先问服务端，挑**最新**的那个；服务端不可达或没有更新时再退回就绪包。
	prepared, hasPrepared := preparedClientVersion(opt)
	if hasPrepared {
		log.info("发现上次已就绪的客户端版本（稍后与更新源比较）", "version", prepared.Version, "exe", prepared.Exe)
	}

	switchPrepared := func() (BootstrapResult, error) {
		emit(BootstrapEvent{Event: BootstrapEventStatus, Phase: BootstrapPhaseRestarting,
			Version: prepared.Version, FromVersion: opt.CurrentVersion})
		return switchToPreparedVersion(ctx, opt, prepared, emit, log)
	}
	// noUpdate 在"服务端没有更高版本/不可用"时调用：有就绪包就切过去，否则保持当前版本。
	noUpdate := func(reason string) (BootstrapResult, error) {
		if hasPrepared {
			log.info("没有比已就绪版本更新的发布，直接切换到已就绪版本", "version", prepared.Version)
			return switchPrepared()
		}
		emit(BootstrapEvent{Event: BootstrapEventNone, Reason: reason, FromVersion: opt.CurrentVersion})
		return BootstrapResult{}, nil
	}

	if opt.ServerURL == "" {
		log.warn("没有可用的更新源（客户端尚未配置服务端），跳过启动期更新")
		return noUpdate("no_source")
	}

	emit(BootstrapEvent{Event: BootstrapEventStatus, Phase: BootstrapPhaseChecking,
		FromVersion: opt.CurrentVersion})
	checkCtx, cancel := context.WithTimeout(ctx, bootstrapCheckTimeout)
	manifest, artifact, updateClient, err := resolveBootstrapUpdate(checkCtx, opt, log)
	cancel()
	if err != nil {
		reason := bootstrapReasonOf(err)
		if hasPrepared {
			// 更新源暂时不可达不应耽误已经下载好的版本。
			log.warn("启动期更新检查失败，改用已就绪版本", "reason", reason, "error", err)
			return switchPrepared()
		}
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: reason, Message: err.Error(),
			FromVersion: opt.CurrentVersion})
		log.warn("启动期更新检查失败，继续使用当前版本", "reason", reason, "error", err)
		return BootstrapResult{}, nil
	}
	if manifest == nil {
		// resolveBootstrapUpdate 内部已记录原因；这里无需再判断。
		return noUpdate("not_newer")
	}
	target := manifest.Version
	// 清单版本不高于已就绪版本：直接切已就绪版本，避免"先切旧包、再更新一次"的双重启。
	if hasPrepared && !mustBeNewer(target, prepared.Version) {
		log.info("更新源没有比已就绪版本更新的内容，直接切换已就绪版本",
			"manifest_version", target, "prepared", prepared.Version)
		return switchPrepared()
	}
	if isBlockedVersion(opt, target) {
		log.warn("目标版本上次启动失败已被标记为坏版本，本轮跳过", "version", target)
		return noUpdate("blocked")
	}

	// 下载到 update\staged，边下边校验 sha256。
	stagedDir := filepath.Join(opt.UpdateDir, stagedDirName)
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		err = fmt.Errorf("创建暂存目录失败: %w", err)
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "prepare_staging", Message: err.Error()})
		return BootstrapResult{}, err
	}
	partPath := filepath.Join(stagedDir, artifact.Filename+partSuffix)
	prog := newBootstrapProgress(emit, target, opt.CurrentVersion)
	if err := downloadArtifact(ctx, log.logger, updateClient, artifact, partPath, prog); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: bootstrapReasonOf(err), Message: err.Error()})
		log.error("下载客户端更新包失败", "error", err)
		return BootstrapResult{}, err
	}
	stagedPath := strings.TrimSuffix(partPath, partSuffix)
	_ = os.Remove(stagedPath)
	if err := os.Rename(partPath, stagedPath); err != nil {
		err = fmt.Errorf("提交暂存文件失败: %w", err)
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "commit_staged", Message: err.Error()})
		return BootstrapResult{}, err
	}
	defer func() { _ = os.Remove(stagedPath) }()

	// 下载阶段已边下边算过 sha256；这里再独立复核一次（磁盘写入异常也能挡住）。
	emit(BootstrapEvent{Event: BootstrapEventStatus, Phase: BootstrapPhaseVerifying, Version: target,
		FromVersion: opt.CurrentVersion})
	if err := verifyArtifactFile(stagedPath, artifact); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "verify", Message: err.Error()})
		return BootstrapResult{}, err
	}

	// 解压到 <clientRoot>\<版本>.tmp-<random>，校验完整后才原子改名为正式目录。
	emit(BootstrapEvent{Event: BootstrapEventStatus, Phase: BootstrapPhaseExtracting, Version: target,
		FromVersion: opt.CurrentVersion})
	suffix, err := randomHex(4)
	if err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "extract", Message: err.Error()})
		return BootstrapResult{}, err
	}
	if err := os.MkdirAll(opt.ClientRoot, 0o755); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "prepare_dir", Message: err.Error()})
		return BootstrapResult{}, err
	}
	tmpDir := filepath.Join(opt.ClientRoot, target+webTempPrefix+suffix)
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "extract", Message: err.Error()})
		return BootstrapResult{}, err
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	// 解压上限按产物大小外推（与渲染层热更共用同一套 zip slip / zip bomb 防护）。
	if err := extractWebZip(stagedPath, tmpDir, webExtractMaxBytes(artifact.Size)); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "extract", Message: err.Error()})
		return BootstrapResult{}, err
	}
	if err := validateBootstrapClientDir(tmpDir, target); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "incomplete", Message: err.Error()})
		return BootstrapResult{}, err
	}

	finalDir := filepath.Join(opt.ClientRoot, target)
	// 最后一道安全阀：绝不删除"当前正在运行的程序目录"。
	// 正常情况下 target 严格新于当前版本，不会命中；注入版本异常时这里兜住。
	if runningDir, err := os.Executable(); err == nil && sameDir(finalDir, filepath.Dir(runningDir)) {
		return BootstrapResult{}, fmt.Errorf("拒绝覆盖正在运行的程序目录: %s", finalDir)
	}
	if err := os.RemoveAll(finalDir); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "activate", Message: err.Error()})
		return BootstrapResult{}, err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "activate", Message: err.Error()})
		return BootstrapResult{}, err
	}
	activated = true

	if err := writeClientCurrent(filepath.Join(opt.ClientRoot, clientCurrentFileName), clientCurrentPointer{
		Version:     target,
		Dir:         target,
		ActivatedAt: time.Now().UnixMilli(),
	}); err != nil {
		// 目录已就位但指针没写成功：current.json 仍指向旧版本，旧版本保持可用。
		emit(BootstrapEvent{Event: BootstrapEventError, Reason: "activate_pointer", Message: err.Error()})
		return BootstrapResult{}, err
	}
	// 清理历史版本时排除"当前正在运行的程序目录"：更新器就运行在其中，删掉它会把
	// 用户正在使用的客户端变成残缺目录（Windows 只保护被占用的 exe，其它文件照删）。
	runningDir := ""
	if exe, err := os.Executable(); err == nil {
		runningDir = filepath.Dir(exe)
	}
	pruneClientVersionsExcept(opt.ClientRoot, target, runningDir, log.logger)
	// 已经准备好新版本，清理"坏版本"记录：目标版本与历史记录不同，记录只针对具体版本。
	_ = os.Remove(blockedPath(opt))

	return switchToPreparedVersion(ctx, opt, preparedClient{Version: target, Exe: filepath.Join(finalDir, clientExeName)}, emit, log)
}

// preparedClient 是一个已经解压完整、可以使用的新版本。
type preparedClient struct {
	Version string
	Exe     string
}

// preparedClientVersion 返回磁盘上已就绪且严格新于当前版本的客户端（没有则 ok=false）。
func preparedClientVersion(opt BootstrapOptions) (preparedClient, bool) {
	ptr, ok := readClientCurrent(filepath.Join(opt.ClientRoot, clientCurrentFileName))
	if !ok {
		return preparedClient{}, false
	}
	version := strings.TrimSpace(ptr.Version)
	if version == "" || strings.Contains(version, "..") || strings.ContainsAny(version, `/\`) {
		return preparedClient{}, false
	}
	if !mustBeNewer(version, opt.CurrentVersion) {
		return preparedClient{}, false
	}
	if isBlockedVersion(opt, version) {
		return preparedClient{}, false
	}
	dir := filepath.Join(opt.ClientRoot, version)
	if err := validateBootstrapClientDir(dir, version); err != nil {
		return preparedClient{}, false
	}
	return preparedClient{Version: version, Exe: filepath.Join(dir, clientExeName)}, true
}

// resolveBootstrapUpdate 拉取并验签清单，返回可用的 client 产物。
//
// manifest 为 nil 表示"没有需要安装的更新"（已由日志说明原因），err 表示真实故障。
// 这里刻意只接受 kind=client：版本更新必须整包替换前后端，不再走 agent/client_web 的
// 独立通道（那正是版本不一致的来源）。
func resolveBootstrapUpdate(ctx context.Context, opt BootstrapOptions, log *applyLogger) (*updatepkg.Manifest, *updatepkg.Artifact, *serverClient, error) {
	client, err := newServerClient(opt.ServerURL, "", opt.CertSHA256, log.logger)
	if err != nil {
		return nil, nil, nil, err
	}
	channel := opt.Channel
	manifestBytes, err := client.GetRawBytes(ctx, "/v1/system/update/manifest?channel="+url.QueryEscape(channel))
	if err != nil {
		if apperr.CodeOf(err) == updatepkg.CodeNoManifest {
			log.warn("更新源未提供更新清单（服务端无可用更新包）", "error", err)
			return nil, nil, nil, nil
		}
		return nil, nil, nil, err
	}
	sigBytes, err := client.GetRawBytes(ctx, "/v1/system/update/signature")
	if err != nil {
		if apperr.CodeOf(err) == updatepkg.CodeNoManifest {
			log.warn("更新源未提供清单签名（服务端无可用更新包）", "error", err)
			return nil, nil, nil, nil
		}
		return nil, nil, nil, err
	}
	manifest, err := updatepkg.VerifyAndParse(updatePublicKey, manifestBytes, string(sigBytes), channel)
	if err != nil {
		return nil, nil, nil, err
	}
	if !mustBeNewer(manifest.Version, opt.CurrentVersion) {
		log.info("更新清单版本不高于当前版本，忽略（防降级）",
			"manifest_version", manifest.Version, "current", opt.CurrentVersion)
		return nil, nil, nil, nil
	}
	artifact := manifest.FindArtifact(updatepkg.KindClient, updateTargetOS, runtime.GOARCH)
	if artifact == nil {
		log.warn("更新清单中没有客户端本体产物（kind=client），跳过启动期更新",
			"version", manifest.Version, "platform", updateTargetOS+"/"+runtime.GOARCH)
		return nil, nil, nil, nil
	}
	if artifact.Size > maxUpdateArtifactBytes {
		return nil, nil, nil, fmt.Errorf("更新产物超出大小上限: %d > %d", artifact.Size, maxUpdateArtifactBytes)
	}
	return manifest, artifact, client, nil
}

// switchToPreparedVersion 等待旧客户端退出，然后启动新版本；失败时回退旧版本。
//
// 返回值里的 Updated 在 NoRestart 模式下也为 true：调用方据此知道"包已经就绪"。
func switchToPreparedVersion(ctx context.Context, opt BootstrapOptions, ready preparedClient, emit func(BootstrapEvent), log *applyLogger) (BootstrapResult, error) {
	emit(BootstrapEvent{
		Event:       BootstrapEventReady,
		Version:     ready.Version,
		FromVersion: opt.CurrentVersion,
		Exe:         ready.Exe,
	})
	log.info("新版本已就绪，等待客户端退出后切换", "version", ready.Version, "exe", ready.Exe,
		"no_restart", opt.NoRestart)
	result := BootstrapResult{Updated: true, Version: ready.Version, Exe: ready.Exe}
	if opt.NoRestart {
		return result, nil
	}

	if opt.ParentPID > 0 {
		if err := waitForProcessExit(ctx, opt.ParentPID, opt.Timeout); err != nil {
			log.error("等待旧客户端退出的过程中超时/被取消，放弃本次切换", "pid", opt.ParentPID, "error", err)
			return result, err
		}
		log.info("旧客户端已退出", "pid", opt.ParentPID)
	}
	time.Sleep(bootstrapSettleDelay)

	oldDir := ""
	if opt.FallbackExe != "" {
		oldDir = filepath.Dir(opt.FallbackExe)
	}
	launchAt := time.Now().UnixMilli()
	pid, err := startClientExeTracked(ready.Exe, ready.Version, oldDir)
	if err != nil {
		log.error("启动新版本客户端失败，尝试回退旧版本", "exe", ready.Exe, "error", err)
		return fallbackToOldClient(opt, ready, log, err)
	}
	// 新版必须同时满足：进程仍存活 + 前端已加载（boot-ok.json）。
	// 只看进程存活会把"起来了但白屏/加载即崩"当成成功，用户随后只会看到一块白板。
	if err := waitForBootOK(opt, pid, ready.Version, launchAt, bootstrapStartupGuard); err != nil {
		log.error("新版本客户端未能完成启动，判定为坏版本，回退旧版本",
			"version", ready.Version, "pid", pid, "guard", bootstrapStartupGuard.String(), "error", err)
		killProcess(pid)
		return fallbackToOldClient(opt, ready, log, err)
	}
	log.info("已启动新版本客户端，更新器退出", "exe", ready.Exe, "version", ready.Version, "pid", pid)
	return result, nil
}

// fallbackToOldClient 启动旧版本，并把它标记成"坏版本"以免下次启动再自动切过去。
func fallbackToOldClient(opt BootstrapOptions, ready preparedClient, log *applyLogger, cause error) (BootstrapResult, error) {
	if opt.FallbackExe == "" {
		log.error("没有可回退的旧版本客户端，需要人工介入", "cause", cause)
		return BootstrapResult{Updated: true, Version: ready.Version, Exe: ready.Exe}, cause
	}
	// 先落盘坏版本记录（独立于环境变量）：用户下一次手动启动旧版时也会跳过这个版本，
	// 不会出现"旧版启动 → 又切到坏版本 → 再回退"的无限循环。
	if err := writeBlockedVersion(opt, ready.Version, cause); err != nil {
		log.warn("写入坏版本记录失败（不阻断回退）", "error", err)
	}
	oldDir := filepath.Dir(opt.FallbackExe)
	if err := startClientExe(opt.FallbackExe, "", clientUpdateBlockedEnv+"="+ready.Version,
		"VAULT_TAKEOVER_STALE_ROOTS="+oldDir); err != nil {
		log.error("回退启动旧版本也失败，需要人工介入", "exe", opt.FallbackExe, "error", err)
		return BootstrapResult{Updated: true, Version: ready.Version, Exe: ready.Exe},
			fmt.Errorf("启动新版本失败(%v)，回退旧版本也失败: %w", cause, err)
	}
	log.warn("已回退启动旧版本客户端（本次更新未生效）", "exe", opt.FallbackExe)
	return BootstrapResult{Updated: true, Version: ready.Version, Exe: ready.Exe}, nil
}

// startClientExeTracked 启动客户端并返回 PID；extraEnv 用于把旧安装目录交给新进程清理陈旧代理。
func startClientExeTracked(exe, version, staleRoot string) (int, error) {
	extra := make([]string, 0, 1)
	if staleRoot != "" {
		extra = append(extra, "VAULT_TAKEOVER_STALE_ROOTS="+staleRoot)
	}
	cmd := newClientCommand(exe, version, extra...)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// newClientCommand 构造"分离会话"的客户端启动命令。
//
// 与 clientswitch.go 的 startClientExe 保持同一套环境约定（VAULT_CLIENT_HANDOFF /
// VAULT_CLIENT_UPDATED_TO），差别只在需要拿回 PID 以便观察新版是否启动即崩溃。
func newClientCommand(exe, version string, extraEnv ...string) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	env := append(os.Environ(), "VAULT_CLIENT_HANDOFF=1",
		"VAULT_CLIENT_SKIP_UPDATE=1")
	if strings.TrimSpace(version) != "" {
		env = append(env, "VAULT_CLIENT_UPDATED_TO="+strings.TrimSpace(version))
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd
}

// bootOKRecord 是新版 Electron 在界面加载完成后写下的启动凭据。
type bootOKRecord struct {
	Version string `json:"version"`
	At      int64  `json:"at"`
}

// waitForBootOK 等待新版本进程**真正完成界面加载**。
//
// 判据同时包含"进程仍存活"与"boot-ok.json 的版本/时间戳对得上"：
//   - 进程退出 → 立即失败；
//   - 进程活着但界面没加载（白屏、缺资源、主进程卡死）→ 超时失败；
//   - 两者都满足 → 成功。
//
// launchAt 用于排除上一次同版本运行遗留的旧凭据。
func waitForBootOK(opt BootstrapOptions, pid int, version string, launchAt int64, timeout time.Duration) error {
	if pid <= 0 {
		return errors.New("新版本进程 PID 非法")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("无法观察新版本进程: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	deadline := time.Now().Add(timeout)
	path := filepath.Join(opt.UpdateDir, bootstrapBootOKFileName)
	for {
		event, err := windows.WaitForSingleObject(handle, 0)
		if err != nil {
			return err
		}
		if event == uint32(windows.WAIT_OBJECT_0) {
			// 极端情况下新版本可能已经被"下一轮更新"接管（current.json 指向更高版本）：
			// 这时它的退出不是失败，不要回退旧版本，避免两个版本同时被拉起。
			if ptr, ok := readClientCurrent(filepath.Join(opt.ClientRoot, clientCurrentFileName)); ok &&
				mustBeNewer(ptr.Version, version) {
				if validateBootstrapClientDir(filepath.Join(opt.ClientRoot, ptr.Version), ptr.Version) == nil {
					return nil
				}
			}
			return errors.New("新版本进程已退出")
		}
		if data, err := os.ReadFile(path); err == nil {
			var rec bootOKRecord
			if json.Unmarshal(data, &rec) == nil &&
				sameVersion(rec.Version, version) &&
				rec.At >= launchAt {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待界面就绪超时（%s）", timeout.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// killProcess 结束指定 PID（只用于"新版没能启动成功"的回退路径）。
func killProcess(pid int) {
	if pid <= 0 {
		return
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	_ = windows.TerminateProcess(handle, 1)
}

// sameDir 判断两个目录是否指向同一路径（Windows 大小写不敏感）。
func sameDir(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(absA), filepath.Clean(absB))
}

// bootstrapReasonOf 把错误压成稳定原因码（供界面与日志使用）。
func bootstrapReasonOf(err error) string {
	if err == nil {
		return ""
	}
	if code := apperr.CodeOf(err); code != "" {
		switch code {
		case updatepkg.CodeBadSignature:
			return "bad_signature"
		case updatepkg.CodeBadManifest:
			return "bad_manifest"
		case updatepkg.CodeNoManifest:
			return "no_manifest"
		}
		return code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "update_failed"
}

// validateBootstrapClientDir 判断解压目录是否是**前后端完整**的客户端程序目录。
//
// 与 isCompleteClientDir 的区别：这里额外要求 Vault-Agent.exe 与 Vault-Updator.exe。
// 少了任何一个都不能切换 —— 否则要么本地服务起不来，要么下一次无法再自动更新。
func validateBootstrapClientDir(dir, expectedVersion string) error {
	if !isCompleteClientDir(dir) {
		return errors.New("客户端目录缺少 Vault.exe / resources/app.asar 或文件数不足")
	}
	for _, name := range []string{"Vault-Agent.exe", "Vault-Updator.exe"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.IsDir() {
			return fmt.Errorf("客户端目录缺少 %s", name)
		}
	}
	// 新包会带 VERSION 元数据（build.py 写入 zip）；旧包没有这个文件，按兼容处理。
	// 有它时必须与清单版本完全一致：这是"包名是新的、内容却是旧的"最后一道防线。
	versionFile := filepath.Join(dir, "VERSION")
	if data, err := os.ReadFile(versionFile); err == nil {
		got := strings.TrimSpace(string(data))
		if got != "" && !sameVersion(got, expectedVersion) {
			return fmt.Errorf("客户端包版本不一致：VERSION=%s，清单=%s", got, expectedVersion)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取客户端版本文件失败: %w", err)
	}
	return nil
}

// bootstrapProgress 把下载进度转成 stdout 事件（节流推送）。
type bootstrapProgress struct {
	emit    func(BootstrapEvent)
	version string
	from    string

	mu       sync.Mutex
	throttle progressThrottle
	total    int64
	received int64
}

func newBootstrapProgress(emit func(BootstrapEvent), version, from string) *bootstrapProgress {
	return &bootstrapProgress{emit: emit, version: version, from: from, throttle: newProgressThrottle(0)}
}

func (p *bootstrapProgress) reset(total, received int64) {
	if total > 0 {
		p.total = total
	}
	p.received = received
	p.mu.Lock()
	p.throttle.allow(true, received, time.Now())
	p.mu.Unlock()
	p.report(received)
}

func (p *bootstrapProgress) add(n int64) {
	if n <= 0 {
		return
	}
	p.received += n
	p.mu.Lock()
	ok := p.throttle.allow(false, p.received, time.Now())
	p.mu.Unlock()
	if !ok {
		return
	}
	p.report(p.received)
}

func (p *bootstrapProgress) report(received int64) {
	p.emit(BootstrapEvent{
		Event:         BootstrapEventStatus,
		Phase:         BootstrapPhaseDownloading,
		Version:       p.version,
		FromVersion:   p.from,
		ReceivedBytes: received,
		TotalBytes:    p.total,
	})
}

// ---- 坏版本记录 ----

type blockedRecord struct {
	Version string `json:"version"`
	Reason  string `json:"reason,omitempty"`
	At      int64  `json:"at"`
}

func blockedPath(opt BootstrapOptions) string {
	return filepath.Join(opt.ClientRoot, bootstrapBlockedFileName)
}

func isBlockedVersion(opt BootstrapOptions, version string) bool {
	data, err := os.ReadFile(blockedPath(opt))
	if err != nil {
		return false
	}
	var rec blockedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return false
	}
	return rec.Version != "" && sameVersion(rec.Version, version)
}

func writeBlockedVersion(opt BootstrapOptions, version string, cause error) error {
	rec := blockedRecord{Version: version, Reason: cause.Error(), At: time.Now().UnixMilli()}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opt.ClientRoot, 0o755); err != nil {
		return err
	}
	path := blockedPath(opt)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	// Windows 上 os.Rename 不能覆盖已存在文件；坏版本记录可能被重复写入。
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
