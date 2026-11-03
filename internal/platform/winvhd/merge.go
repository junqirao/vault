//go:build windows

package winvhd

import (
	"context"
	"net/http"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// CodeMergeUnavailable 缺少 Hyper-V 模块（Merge-VHD），无法合并差异盘。
const CodeMergeUnavailable = "platform.merge_unavailable"

// MergeDiff 把差异盘 childPath 合并进母盘 parentPath（离线操作）。
//
// 走 Hyper-V 模块的 Merge-VHD：脚本把子盘的全部改动写进母盘，成功后删除子盘。
// 调用方必须先清理同一母盘下的其它差异盘，并确保两张盘都没有被挂载或被 iSCSI 打开。
//
// 无 Hyper-V 模块时返回 platform.merge_unavailable（501），由上层把入口置为不可用/
// 给出可操作的提示，而不是把一句看不懂的 PowerShell 报错甩给用户。
func (m *Manager) MergeDiff(ctx context.Context, childPath, parentPath string) error {
	if strings.TrimSpace(childPath) == "" || strings.TrimSpace(parentPath) == "" {
		return apperr.InvalidParam("path")
	}
	if strings.EqualFold(childPath, parentPath) {
		return apperr.InvalidParam("path")
	}
	if !m.hyperVAvailable {
		m.logger.Warn("缺少 Hyper-V 模块，无法合并差异盘", "child", childPath, "parent", parentPath)
		return apperr.New(CodeMergeUnavailable, http.StatusNotImplemented).
			WithArg("reason", "hyperv_unavailable")
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	params := []winps.Param{
		winps.String("Path", childPath),
		winps.String("DestinationPath", parentPath),
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVHDMerge, params, nil); err != nil {
		return err
	}
	m.logger.Info("已把差异盘合并进母盘", "child", childPath, "parent", parentPath)
	return nil
}
