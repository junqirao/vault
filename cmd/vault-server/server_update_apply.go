package main

import (
	"flag"
	"fmt"
	"os"

	"vault/internal/serverupdate"
)

// applyUpdateSubcommand 是服务端自更新的替换器子命令。
//
// 由更新管理器把当前 Vault-Server.exe 复制到 update/server/helper/ 后启动。
// 它不加载配置、不获取单实例锁、不启动 HTTP，只负责在旧进程退出后替换文件并拉起新版本。
const applyUpdateSubcommand = "apply-update"

func runApplyUpdate(args []string) int {
	fs := flag.NewFlagSet("Vault-Server apply-update", flag.ContinueOnError)
	pendingPath := fs.String("pending", "", "更新任务文件（内部使用）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *pendingPath == "" {
		fmt.Fprintln(os.Stderr, "apply-update: 缺少 -pending")
		return 2
	}
	if err := serverupdate.RunApply(*pendingPath); err != nil {
		fmt.Fprintln(os.Stderr, "apply-update:", err)
		return 1
	}
	return 0
}
