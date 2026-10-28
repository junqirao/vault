// Command vault-updator 是客户端本体更新的"切换器"——一个**独立的可执行文件**。
//
// 为什么必须是独立 exe（而不是让 Vault.exe 自己替换自己）：
//   - Windows 不允许覆盖正在运行的 exe 镜像，也不允许目录里的文件在被持有时被删除/替换；
//   - Vault.exe 一旦退出，就没有代码能再去启动新版本了；
//   - 由代理临时复制自身来充当切换器虽然也能工作，但进程名是随机串、位置在
//     `%ProgramData%\Vault\update\` 下，现场排障时"在客户端里看不到 updator"
//     （真实反馈），也不便于用户/安全软件确认它是谁。
//
// 时序（与客户端的契约，见 frontend/apps/client/electron/clientUpdate.ts）：
//
//	客户端：检测到新版本 → 拉起本程序 → 隐藏窗口 → 停代理 → app.exit(0)
//	本程序：等客户端 PID 退出 → 800ms 稳定期 → 启动新版本 exe
//	        （起不来则回退旧版本，并用环境变量屏蔽该版本，避免来回重启）→ 自己退出
//
// 本程序刻意**不初始化日志、不取单实例锁、不启动 HTTP**：它必须在客户端与代理都已退出
// 之后独立完成工作。运行记录写入 `%ProgramData%\Vault\update\updator.log`。
//
// 用法（由客户端内部调用，不需要手工执行）：
//
//	Vault-Updator.exe -pid 1234 -target C:\ProgramData\Vault\client\0.1.217\Vault.exe ^
//	    -version 0.1.217 -fallback C:\ProgramData\Vault\client\0.1.216\Vault.exe -timeout 120s
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vault/internal/agent"
)

/** 默认等待旧客户端退出的超时。 */
const defaultClientExitTimeout = 120 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
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
