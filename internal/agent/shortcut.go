package agent

import (
	"context"
	"fmt"
	"strings"

	"vault/internal/platform/winps"
)

// shortcutResult 是 winps/scripts/shortcut.ps1 的 JSON 输出。
type shortcutResult struct {
	OK      bool   `json:"ok"`
	Action  string `json:"action"`
	Path    string `json:"path"`
	Removed bool   `json:"removed"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// shortcutTarget 返回快捷方式应指向的目标。
//
// 盘符模式的挂载记录是 "E:"，快捷方式的目标补成根路径 "E:\"（与资源管理器里双击盘符等价）；
// 目录模式直接用挂载目录。前端手动打开挂载点时也是这个口径（见 openTargetOf）。
func shortcutTarget(mountMode, mountPath string) string {
	target := strings.TrimSpace(mountPath)
	if strings.EqualFold(strings.TrimSpace(mountMode), mountModeLetter) && target != "" {
		target = strings.TrimRight(target, `\/`) + `\`
	}
	return target
}

// createRepoShortcut 为刚挂载成功、且开启了「自动创建快捷方式」的库创建 .lnk。
//
// 只记录脚本返回的完整路径；失败由调用方按 best effort 记日志，绝不影响挂载本身。
func (a *Agent) createRepoShortcut(ctx context.Context, mountMode, mountPath, repoName string) (string, error) {
	target := shortcutTarget(mountMode, mountPath)
	if target == "" {
		return "", fmt.Errorf("agent: 快捷方式目标为空")
	}
	var out shortcutResult
	params := []winps.Param{
		winps.String("Action", "create"),
		winps.String("Target", target),
		winps.String("Name", strings.TrimSpace(repoName)),
		// 空串 = 系统桌面；RunScriptJSON 会跳过空值参数，脚本按默认桌面处理。
		winps.String("Dir", a.cfg.Get().ShortcutDir),
	}
	if err := a.ps.RunScriptJSON(ctx, winps.ScriptShortcut, params, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("agent: 创建快捷方式失败: %s", out.Reason)
	}
	path := strings.TrimSpace(out.Path)
	if path == "" {
		return "", fmt.Errorf("agent: 创建快捷方式后未返回路径")
	}
	return path, nil
}

// removeRepoShortcut 删除挂载记录里记下的那个自动快捷方式。
//
// 按完整路径删除（而不是用当前配置重新拼名字/目录）：用户之后可能改过快捷方式目录或库名，
// 重新拼会漏删甚至删错。路径为空表示这次挂载没有自动创建快捷方式，直接跳过。
func (a *Agent) removeRepoShortcut(ctx context.Context, shortcutPath string) error {
	shortcutPath = strings.TrimSpace(shortcutPath)
	if shortcutPath == "" {
		return nil
	}
	var out shortcutResult
	params := []winps.Param{
		winps.String("Action", "remove"),
		winps.String("Path", shortcutPath),
	}
	if err := a.ps.RunScriptJSON(ctx, winps.ScriptShortcut, params, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("agent: 删除快捷方式失败: %s", out.Reason)
	}
	return nil
}
