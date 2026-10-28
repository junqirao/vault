package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// 客户端本体切换器（client-switch）
//
// 为什么需要它：Electron 的 Vault.exe 无法"自己替换自己"，也无法在正在运行时被别的进程
// 覆盖（镜像文件被持有）。此前客户端本体的切换方式是新版本进程与旧进程**抢单实例锁**，
// 新进程接管后杀掉旧进程、再 `app.relaunch()` 重启自己 —— 这条路上每一步都可能翻车：
//
//   - 旧进程被 SIGKILL，来不及隐藏窗口，用户看到界面闪一下消失；
//   - 新进程接管后自己又 relaunch 一次，窗口再消失/出现一次（第二段闪屏）；
//   - 旧进程的代理子进程没人回收，仍占着 `Global\VaultAgent`，新客户端的代理起不来，
//     界面上就是一片 network.error，只能手动点"重启服务"；
//   - 接管失败（版本更高/读不到版本）时新进程静默退出，用户看到的就是"根本沒更新"。
//
// 现在把时序交给一个**独立进程**：
//
//	首选：客户端目录里的 Vault-Updator.exe（cmd/vault-updator，随客户端一起分发）；
//	兜底：updator 缺失时，代理把自身复制为临时副本并跑 client-switch 子命令（本文件）。
//
// 两条路径执行的是同一个 RunClientSwitch：
//
//	客户端：拉起切换器 → 隐藏窗口 → 让代理退出 → 自己立即退出
//	切换器：等客户端进程退出 → 启动新版本 exe（失败则回退旧版本）→ 自己退出
//
// 这样旧进程是自己"干净退出"的（单实例锁、文件句柄、代理都由它按顺序释放），
// 新进程启动时没有任何竞争者，不需要 takeover、不需要 relaunch，也就没有第二段闪屏。

const (
	// clientSwitchSubcommand 是切换器子命令名（与 cmd/vault-agent/main.go 的常量保持一致）。
	clientSwitchSubcommand = "client-switch"
	// clientSwitchPrefix 是切换器临时副本的文件名前缀。
	clientSwitchPrefix = "client-switch-"
	// clientSwitchLogName 是切换器的独立排障日志名。
	//
	// 统一叫 updator.log：无论切换器是独立 exe（cmd/vault-updator）还是代理临时副本，
	// 现场排障只需要看这一个文件。
	clientSwitchLogName = "updator.log"
	// clientUpdateBlockedEnv 是传给"回退启动的旧客户端"的环境变量名，值为启动失败的版本号。
	// 客户端读到它就跳过该版本的自动切换，避免来回重启（见 clientUpdate.ts 的 requestSwitch）。
	clientUpdateBlockedEnv = "VAULT_CLIENT_UPDATE_BLOCKED"
	// clientSwitchSettleDelay 是旧客户端退出后、启动新客户端前的稳定期。
	//
	// Electron 的单实例锁与 asar 文件句柄由内核在进程退出时释放，但 Windows 上句柄回收
	// 偶尔滞后几十到几百毫秒。抢跑的话新进程会拿不到锁，进而走 takeover/blocked 分支
	// ——那正是"没有更新"与"闪屏"的来源。留出这段稳定期比任何重试都可靠。
	clientSwitchSettleDelay = 800 * time.Millisecond
)

// ClientSwitchOptions 是 client-switch 子命令的入参。
type ClientSwitchOptions struct {
	// ClientPID 是旧客户端的进程 PID（替换器等它退出后才启动新版本）。
	ClientPID int
	// TargetExe 是新版本客户端主程序（绝对路径，必填）。
	TargetExe string
	// TargetVersion 是新版本号（仅用于日志与传递给新进程的环境变量）。
	TargetVersion string
	// FallbackExe 是新版本启动失败时要回退的旧版本主程序（可为空）。
	FallbackExe string
	// Timeout 是等待旧客户端退出的超时。
	Timeout time.Duration
}

// RunClientSwitch 以"客户端切换器"身份运行：等旧客户端退出 → 启动新客户端 → 退出。
//
// 它必须运行在**另一个进程**里：切换器要能在旧客户端退出的瞬间动手，而不是被旧客户端的
// 退出流程牵连。生产路径由 cmd/vault-updator 调用；代理侧兜底见 startClientSwitchCopy。
func RunClientSwitch(ctx context.Context, opt ClientSwitchOptions, logger *slog.Logger) error {
	opt.TargetExe = strings.TrimSpace(opt.TargetExe)
	opt.TargetVersion = strings.TrimSpace(opt.TargetVersion)
	opt.FallbackExe = strings.TrimSpace(opt.FallbackExe)

	log := newApplyLogger(clientSwitchLogPath(opt.TargetExe), logger)
	if err := validateClientSwitchOptions(opt); err != nil {
		log.error("参数校验失败，放弃本次切换", "error", err)
		return err
	}
	log.info("客户端切换器启动", "client_pid", opt.ClientPID, "target", opt.TargetExe,
		"version", opt.TargetVersion, "fallback", opt.FallbackExe, "timeout", opt.Timeout.String())

	// ---- ① 等旧客户端退出（只等待，绝不强杀）----
	if opt.ClientPID > 0 {
		if err := waitForProcessExit(ctx, opt.ClientPID, opt.Timeout); err != nil {
			log.error("等待旧客户端退出的过程中超时/被取消，放弃本次切换（未启动任何进程）",
				"pid", opt.ClientPID, "error", err)
			return err
		}
		log.info("旧客户端已退出", "pid", opt.ClientPID)
	}
	// ---- ② 稳定期：等单实例锁、asar 句柄真正释放 ----
	time.Sleep(clientSwitchSettleDelay)

	// ---- ③ 启动新版本；失败则回退旧版本，绝不让用户没有界面 ----
	if err := startClientExe(opt.TargetExe, opt.TargetVersion); err != nil {
		log.error("启动新版本客户端失败", "exe", opt.TargetExe, "error", err)
		if opt.FallbackExe != "" {
			// 告诉回退启动的旧客户端：这个版本刚启动失败过，别再自动切回去。
			// 否则会形成"旧版启动 → 发现新版就绪 → 切换 → 新版起不来 → 回退旧版"的
			// 无限重启循环，用户的观感就是窗口反复闪。
			if ferr := startClientExe(opt.FallbackExe, "",
				clientUpdateBlockedEnv+"="+opt.TargetVersion); ferr != nil {
				log.error("回退启动旧版本也失败，需要人工介入", "exe", opt.FallbackExe, "error", ferr)
				return fmt.Errorf("启动新版本失败(%v)，回退旧版本也失败: %w", err, ferr)
			}
			log.warn("已回退启动旧版本客户端（本次更新未生效）", "exe", opt.FallbackExe)
			return nil
		}
		return err
	}
	log.info("已启动新版本客户端，切换器退出", "exe", opt.TargetExe, "version", opt.TargetVersion)
	return nil
}

// validateClientSwitchOptions 校验切换参数。
func validateClientSwitchOptions(opt ClientSwitchOptions) error {
	if strings.TrimSpace(opt.TargetExe) == "" {
		return fmt.Errorf("-target 必填")
	}
	if !filepath.IsAbs(opt.TargetExe) {
		return fmt.Errorf("-target 必须是绝对路径: %q", opt.TargetExe)
	}
	info, err := os.Stat(opt.TargetExe)
	if err != nil {
		return fmt.Errorf("-target 不可用: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("-target 指向目录")
	}
	if opt.FallbackExe != "" && !filepath.IsAbs(opt.FallbackExe) {
		return fmt.Errorf("-fallback 必须是绝对路径: %q", opt.FallbackExe)
	}
	if opt.Timeout <= 0 {
		return fmt.Errorf("-timeout 必须为正")
	}
	return nil
}

// startClientExe 以分离会话启动客户端主程序。
//
// VAULT_CLIENT_HANDOFF=1 保留给主进程的接管分支：正常情况下旧进程已退出、单实例锁空闲，
// 这个变量不会被用到；一旦出现极端时序（旧进程迟迟不退），主进程会据此静默退出而不是
// 弹一个"已有一个客户端在运行"的对话框吓用户。
func startClientExe(exe, version string, extraEnv ...string) error {
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	env := append(os.Environ(), "VAULT_CLIENT_HANDOFF=1")
	if strings.TrimSpace(version) != "" {
		env = append(env, "VAULT_CLIENT_UPDATED_TO="+strings.TrimSpace(version))
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd.Start()
}

// clientSwitchLogPath 返回切换器日志路径：%ProgramData%\Vault\update\client-switch.log。
func clientSwitchLogPath(target string) string {
	if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
		return filepath.Join(pd, "Vault", updateDirName, clientSwitchLogName)
	}
	dir := filepath.Dir(target)
	if dir == "" || dir == "." {
		return clientSwitchLogName
	}
	return filepath.Join(dir, clientSwitchLogName)
}

// ClientSwitchRequest 是 POST /agent/update/client/switch 的请求体。
type ClientSwitchRequest struct {
	// ClientPID 是发起请求的客户端进程 PID（切换器等它退出后才启动新版本）。
	ClientPID int `json:"client_pid"`
	// TargetVersion 要切换到的版本；为空时取 current.json 指向的版本。
	TargetVersion string `json:"target_version"`
	// FallbackExe 新版本启动失败时回退的旧客户端 exe（可选）。
	FallbackExe string `json:"fallback_exe"`
}

// switchClient 校验目标版本并启动独立的切换器进程。
//
// 幂等：一个代理进程生命周期内只启动一个切换器。切换器启动后客户端会在几秒内退出，
// 重复请求只返回当前状态 —— 拉起第二个切换器会导致两个新客户端同时启动、抢单实例锁。
func (a *Agent) switchClient(req ClientSwitchRequest) (ClientUpdateState, error) {
	version := strings.TrimSpace(req.TargetVersion)
	if version == "" {
		if cur, ok := readClientCurrent(a.clientCurrentPath()); ok {
			version = strings.TrimSpace(cur.Version)
		}
	}
	if version == "" {
		return a.clientUpdateState(), errClientUpdateFailed("no_target_version", nil)
	}
	if strings.Contains(version, "..") || strings.ContainsAny(version, `/\`) {
		return a.clientUpdateState(), errClientUpdateFailed("bad_target_version", nil)
	}
	dir := a.clientVersionDir(version)
	if !isCompleteClientDir(dir) {
		return a.clientUpdateState(), errClientUpdateFailed("incomplete", nil)
	}

	if !a.clientSwitchStarted.CompareAndSwap(false, true) {
		a.logger.Info("客户端切换器已在运行，忽略重复请求", "version", version,
			"client_pid", req.ClientPID)
		return a.clientUpdateState(), nil
	}

	// 回退目标只接受"存在的绝对路径"：路径不可用时宁可不给回退，也不能让切换器
	// 去启动一个不存在的程序（那才是真的把用户锁在门外）。
	fallback := strings.TrimSpace(req.FallbackExe)
	if fallback != "" {
		if !filepath.IsAbs(fallback) {
			fallback = ""
		} else if info, err := os.Stat(fallback); err != nil || info.IsDir() {
			fallback = ""
		}
	}

	target := filepath.Join(dir, clientExeName)
	if _, err := startClientSwitchCopy(ClientSwitchOptions{
		ClientPID:     req.ClientPID,
		TargetExe:     target,
		TargetVersion: version,
		FallbackExe:   fallback,
		Timeout:       DefaultUpdateTimeout,
	}, a.logger); err != nil {
		// 启动失败必须复位，允许客户端再次请求（否则这一轮更新就永久卡死）。
		a.clientSwitchStarted.Store(false)
		return a.clientUpdateState(), errClientUpdateFailed("switch_start", err)
	}

	if a.client != nil {
		a.client.setState(func(s *ClientUpdateState) {
			s.State = ClientStateSwitching
			s.ReadyVersion = version
			s.Error = ""
		})
	}
	a.logger.Info("客户端切换器已启动，等待客户端退出后切换", "version", version,
		"client_pid", req.ClientPID, "target", target, "fallback", fallback)
	return a.clientUpdateState(), nil
}

// startClientSwitchCopy 把**当前进程的 exe** 复制为切换器副本，并以 client-switch 子命令
// 分离启动它。
//
// 用代理自身而不是新下载的客户端：切换器必须在旧客户端退出后仍然存在，而此刻唯一
// 确定已提权、且不依附于 Electron 生命周期的可执行文件就是代理本体。
func startClientSwitchCopy(opt ClientSwitchOptions, logger *slog.Logger) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("agent: 解析当前可执行文件路径失败: %w", err)
	}
	dir := filepath.Join(DataDir(), updateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agent: 创建切换器副本目录失败: %w", err)
	}
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	copyPath := filepath.Join(dir, fmt.Sprintf("%s%d-%s.exe", clientSwitchPrefix, os.Getpid(), suffix))
	// 副本可能已在运行（上一次切换未完成）：覆盖即可，运行中的镜像不受影响。
	if err := copyFile(exe, copyPath); err != nil {
		return "", fmt.Errorf("agent: 复制自身为切换器副本失败: %w", err)
	}

	args := []string{
		clientSwitchSubcommand,
		"-pid", fmt.Sprintf("%d", opt.ClientPID),
		"-target", opt.TargetExe,
		"-version", opt.TargetVersion,
		"-timeout", opt.Timeout.String(),
	}
	if opt.FallbackExe != "" {
		args = append(args, "-fallback", opt.FallbackExe)
	}
	cmd := detachedCommand(copyPath, args...)
	if err := cmd.Start(); err != nil {
		return copyPath, fmt.Errorf("agent: 启动切换器副本失败: %w", err)
	}
	if logger != nil {
		logger.Info("已启动客户端切换器副本", "copy", copyPath, "target", opt.TargetExe,
			"client_pid", opt.ClientPID)
	}
	return copyPath, nil
}
