// Command vault-updator 是客户端版本更新的**独立入口**。
//
// 它有两条职责完全不同的路径：
//
//  1. bootstrap（新架构，首选）：在客户端还**没有启动本地服务**时先检查更新。
//     自己读客户端配置找到更新源 → 拉清单 → 验签 → 下载 → 校验 → 解压 → 原子激活，
//     然后等 Vault.exe 退出、启动新版本（整个应用重启）。
//     这样"版本更新"不再依赖本地代理，也不依赖渲染层；client 产物里同时包含
//     Vault.exe / resources/app.asar / Vault-Agent.exe，前后端永远一起换。
//
//  2. switch（旧架构，保留兼容）：老客户端（尚未升级到 bootstrap 架构）已经把新版本
//     解压好并写了 current.json，只要求切换器"等旧进程退出 → 启动新版本 exe"。
//     升级完成后新客户端会自动走 bootstrap，这条路径只服务于跨版本迁移。
//
// 两条路径都**不初始化统一日志、不取单实例锁、不启动 HTTP**：它们必须在客户端与代理
// 都已退出（或尚未启动）之后独立完成工作。运行记录写入
// `%ProgramData%\Vault\update\updator.log`。
//
// 用法（由客户端内部调用，不需要手工执行）：
//
//	Vault-Updator.exe bootstrap -pid 1234 -current-version 0.1.233 -json
//	Vault-Updator.exe -pid 1234 -target C:\ProgramData\Vault\client\0.1.234\Vault.exe ^
//	    -version 0.1.234 -fallback C:\...\Vault.exe -timeout 120s
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"vault/internal/agent"
	appversion "vault/internal/version"
)

/** 默认等待旧客户端退出的超时。 */
const defaultClientExitTimeout = 120 * time.Second

// bootstrapSubcommand 是启动期更新器子命令名（与 Electron 主进程约定）。
const bootstrapSubcommand = "bootstrap"

func main() {
	// 被客户端拉起时不应出现控制台黑框；从命令行手工运行时该函数不会动手。
	agent.HideOwnConsoleWindow()
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == bootstrapSubcommand {
		return runBootstrap(args[1:])
	}
	// 兼容 `-bootstrap` 写法（便于手工排障）；过滤掉这个标记本身再交给 flag 解析。
	for _, arg := range args {
		if arg == "-bootstrap" || arg == "--bootstrap" {
			filtered := make([]string, 0, len(args))
			for _, item := range args {
				if item == "-bootstrap" || item == "--bootstrap" {
					continue
				}
				filtered = append(filtered, item)
			}
			return runBootstrap(filtered)
		}
	}
	return runSwitch(args)
}

// runSwitch 是旧架构的"切换器"：等旧客户端退出 → 启动新版本 exe（失败回退旧版本）。
func runSwitch(args []string) int {
	fs := flag.NewFlagSet("vault-updator", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pid := fs.Int("pid", 0, "需要等待退出的旧客户端 PID")
	target := fs.String("target", "", "新版本客户端主程序（必填，绝对路径）")
	version := fs.String("version", "", "新版本号（仅用于日志与传递给新进程）")
	fallback := fs.String("fallback", "", "新版本启动失败时回退的旧客户端主程序（可选）")
	timeout := fs.Duration("timeout", defaultClientExitTimeout, "等待旧客户端退出的超时")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 收到中断信号时优雅放弃：此时**不会**启动任何进程，用户仍停在原版本上。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opt := agent.ClientSwitchOptions{
		ClientPID:     *pid,
		TargetExe:     *target,
		TargetVersion: *version,
		FallbackExe:   *fallback,
		Timeout:       *timeout,
	}
	// 日志传 nil：该路径刻意不接入统一日志，切换器需要的是独立可读的 updator.log。
	if err := agent.RunClientSwitch(ctx, opt, nil); err != nil {
		fmt.Fprintln(os.Stderr, "Vault-Updator:", err)
		return 1
	}
	return 0
}

// runBootstrap 是启动期更新器：检查、下载、解压、激活、切换。
func runBootstrap(args []string) int {
	fs := flag.NewFlagSet("vault-updator bootstrap", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pid := fs.Int("pid", 0, "正在运行的客户端 PID（更新器等它退出后启动新版本）")
	currentVersion := fs.String("current-version", "", "当前客户端版本（作为防降级基准）")
	serverURL := fs.String("server-url", "", "更新源地址（为空时从客户端配置读取）")
	certSHA256 := fs.String("cert-sha256", "", "更新源证书指纹（为空时从客户端配置读取）")
	channel := fs.String("channel", "", "更新通道（为空时从本地配置读取，默认 stable）")
	timeout := fs.Duration("timeout", defaultClientExitTimeout, "等待旧客户端退出的超时")
	fallback := fs.String("fallback", "", "新版本启动失败时回退的旧客户端主程序")
	jsonLines := fs.Bool("json", false, "以 JSON Lines 输出事件（Electron 使用）")
	noRestart := fs.Bool("no-restart", false, "只检查/准备，不等待退出也不启动新版本（排障用）")
	clientRoot := fs.String("client-root", "", "客户端本体根目录（默认为 %ProgramData%\\Vault\\client）")
	updateDir := fs.String("update-dir", "", "更新工作目录（默认为 %ProgramData%\\Vault\\update）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 事件输出：stdout 只写 JSON Lines，其它日志全部走 updator.log / stderr。
	emit := newEventEmitter(*jsonLines)

	// 配置来源：更新源默认取客户端里"当前活动/自动连接"的那台服务端。
	// update 接口是匿名的，因此这里不需要登录会话，也不依赖本地代理。
	local := loadLocalSettings(*serverURL, *certSHA256, *channel)
	if !local.autoUpdate {
		emit(agent.BootstrapEvent{Event: agent.BootstrapEventNone, Reason: "auto_update_off"})
		return 0
	}

	version := strings.TrimSpace(*currentVersion)
	if version == "" {
		version = strings.TrimSpace(appversion.Version)
	}
	fallbackExe := strings.TrimSpace(*fallback)
	if fallbackExe == "" {
		// 更新器与 Vault.exe 同级；这是"新版本起不来时把用户捞回来"的目标。
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), "Vault.exe")
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				fallbackExe = candidate
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	result, err := agent.RunClientBootstrap(ctx, agent.BootstrapOptions{
		ParentPID:      *pid,
		CurrentVersion: version,
		ServerURL:      local.serverURL,
		CertSHA256:     local.certSHA256,
		Channel:        local.channel,
		Timeout:        *timeout,
		FallbackExe:    fallbackExe,
		ClientRoot:     strings.TrimSpace(*clientRoot),
		UpdateDir:      strings.TrimSpace(*updateDir),
		NoRestart:      *noRestart,
	}, emit)
	if err != nil {
		// 事件里已经带过错误原因；这里只负责返回非 0 退出码供排障。
		return 1
	}
	if result.Updated {
		return 0
	}
	return 0
}

// eventEmitter 把更新器事件写到 stdout（JSON Lines）或 stderr（人类可读）。
type eventEmitter struct {
	json bool
	mu   sync.Mutex
}

func newEventEmitter(jsonLines bool) func(agent.BootstrapEvent) {
	e := &eventEmitter{json: jsonLines}
	return e.emit
}

func (e *eventEmitter) emit(ev agent.BootstrapEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.json {
		// 写失败（父进程已退出、管道关闭）不影响更新本身：忽略。
		_ = json.NewEncoder(os.Stdout).Encode(ev)
		return
	}
	switch ev.Event {
	case agent.BootstrapEventStatus:
		fmt.Fprintf(os.Stderr, "[%s] %s %s\n", ev.Phase, ev.Version, ev.Reason)
	case agent.BootstrapEventNone:
		fmt.Fprintf(os.Stderr, "无需更新：%s\n", ev.Reason)
	case agent.BootstrapEventReady:
		fmt.Fprintf(os.Stderr, "新版本已就绪：%s → %s\n", ev.FromVersion, ev.Version)
	case agent.BootstrapEventError:
		fmt.Fprintf(os.Stderr, "更新失败：%s %s\n", ev.Reason, ev.Message)
	}
}

// localSettings 是更新器需要的本地配置（与 Electron 主进程/代理共用同一批文件）。
type localSettings struct {
	serverURL  string
	certSHA256 string
	channel    string
	autoUpdate bool
}

// loadLocalSettings 读取客户端配置与代理配置。
//
// 读取失败时按"没有更新源 / 自动更新开启"处理：更新的失败不能变成启动失败。
func loadLocalSettings(serverURL, certSHA256, channel string) localSettings {
	out := localSettings{
		serverURL:  strings.TrimSpace(serverURL),
		certSHA256: strings.TrimSpace(certSHA256),
		channel:    strings.TrimSpace(channel),
		autoUpdate: true,
	}
	if out.serverURL == "" {
		if server, ok := readActiveServer(); ok {
			out.serverURL = server.baseURL
			out.certSHA256 = server.certSHA256
		}
	} else if out.certSHA256 == "" {
		// 只有地址与配置里的某台服务端一致时才借用它的指纹，避免把 A 的指纹用去连 B。
		if server, ok := readActiveServer(); ok && sameServerURL(server.baseURL, out.serverURL) {
			out.certSHA256 = server.certSHA256
		}
	}
	// 老配置可能只有 trusted-certs.json 里的指纹（TOFU 首次信任时写入），
	// 而 servers[] 还没补上 certSha256：这里兜底读一次，避免自签服务端永远连不上更新接口。
	if out.serverURL != "" && out.certSHA256 == "" {
		out.certSHA256 = trustedCertFor(out.serverURL)
	}
	if out.channel == "" {
		out.channel = "stable"
	}
	// 代理配置里是用户可见的「自动更新」「更新通道」两个开关。
	if cfg, ok := readAgentConfig(); ok {
		if cfg.AutoUpdate != nil {
			out.autoUpdate = *cfg.AutoUpdate
		}
		if strings.TrimSpace(cfg.UpdateChannel) != "" {
			out.channel = strings.TrimSpace(cfg.UpdateChannel)
		}
	}
	return out
}

// clientConfig 是 Electron 渲染层持久化的配置（%APPDATA%\Vault\config.json）中我们关心的字段。
type clientConfig struct {
	ActiveKey string `json:"activeKey"`
	Servers   []struct {
		Key         string `json:"key"`
		BaseURL     string `json:"baseUrl"`
		ServerName  string `json:"serverName"`
		CertSHA256  string `json:"certSha256"`
		AutoConnect *bool  `json:"autoConnect"`
	} `json:"servers"`
}

type activeServer struct {
	baseURL    string
	certSHA256 string
}

// readActiveServer 从客户端配置中选出更新源。
//
// 选择顺序与界面一致：活动服务端 → 第一台开启自动连接的服务端 → 第一台。
func readActiveServer() (activeServer, bool) {
	path := clientConfigPath()
	if path == "" {
		return activeServer{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return activeServer{}, false
	}
	var cfg clientConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return activeServer{}, false
	}
	if cfg.ActiveKey != "" {
		for _, server := range cfg.Servers {
			if server.Key == cfg.ActiveKey && strings.TrimSpace(server.BaseURL) != "" {
				return activeServer{baseURL: strings.TrimSpace(server.BaseURL), certSHA256: strings.TrimSpace(server.CertSHA256)}, true
			}
		}
	}
	for _, server := range cfg.Servers {
		if strings.TrimSpace(server.BaseURL) == "" {
			continue
		}
		if server.AutoConnect != nil && !*server.AutoConnect {
			continue
		}
		return activeServer{baseURL: strings.TrimSpace(server.BaseURL), certSHA256: strings.TrimSpace(server.CertSHA256)}, true
	}
	for _, server := range cfg.Servers {
		if strings.TrimSpace(server.BaseURL) != "" {
			return activeServer{baseURL: strings.TrimSpace(server.BaseURL), certSHA256: strings.TrimSpace(server.CertSHA256)}, true
		}
	}
	return activeServer{}, false
}

// agentConfig 是代理本地配置文件（%ProgramData%\Vault\agent-config.json）中我们关心的字段。
type agentConfig struct {
	AutoUpdate    *bool  `json:"auto_update"`
	UpdateChannel string `json:"update_channel"`
}

func readAgentConfig() (agentConfig, bool) {
	path := filepath.Join(agentDataDir(), "agent-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return agentConfig{}, false
	}
	var cfg agentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return agentConfig{}, false
	}
	return cfg, true
}

// clientConfigPath 返回 Electron 客户端配置路径（%APPDATA%\Vault\config.json）。
func clientConfigPath() string {
	base := strings.TrimSpace(os.Getenv("APPDATA"))
	if base == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		base = dir
	}
	return filepath.Join(base, "Vault", "config.json")
}

// sameServerURL 判断两个服务端地址是否指向同一台（忽略大小写与尾部斜杠）。
func sameServerURL(a, b string) bool {
	normalize := func(raw string) string {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
		}
		return strings.ToLower(parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.Path, "/"))
	}
	left, right := normalize(a), normalize(b)
	return left != "" && left == right
}

// trustedCertFor 从 Electron 的 TOFU 指纹库中查某个服务源的证书指纹。
//
// trusted-certs.json 的形状是 {"https://host:port": ["<sha256>", ...]}（见 main.ts）。
// 返回第一个指纹；没有记录时返回空串。
func trustedCertFor(serverURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	origin := parsed.Scheme + "://" + parsed.Host
	base := strings.TrimSpace(os.Getenv("APPDATA"))
	if base == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			base = dir
		}
	}
	if base == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(base, "Vault", "trusted-certs.json"))
	if err != nil {
		return ""
	}
	var record map[string][]string
	if err := json.Unmarshal(data, &record); err != nil {
		return ""
	}
	for _, value := range record[origin] {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// agentDataDir 返回代理数据目录（与 internal/agent.DataDir 保持一致）。
func agentDataDir() string {
	if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
		return filepath.Join(pd, "Vault")
	}
	return filepath.Join(os.TempDir(), "Vault")
}
