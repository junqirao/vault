package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestReconcileUpdateClearsPendingWhenCurrentNotOlder 锁定自更新对账的幂等边界：
// 当前版本**高于** pending 里的目标版本时，这次更新显然已经生效（或被客户端本体更新
// 带来的新代理取代），必须清理 pending；否则会被误判成"更新没生效"，累计 3 次后回滚到
// 旧备份 —— 现场表现就是"自动更新完之后本地服务版本还是旧的"。
func TestReconcileUpdateClearsPendingWhenCurrentNotOlder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
	}{
		{name: "完全相同", current: "0.2.0"},
		{name: "当前更新", current: "0.2.1"},
		{name: "当前领先一个大版本", current: "1.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			backup := filepath.Join(dir, "backup", "Vault-Agent.exe")
			if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
				t.Fatalf("建备份目录失败: %v", err)
			}
			if err := os.WriteFile(backup, []byte("old-agent"), 0o600); err != nil {
				t.Fatalf("写备份失败: %v", err)
			}
			pendingPath := filepath.Join(dir, pendingFileName)
			if err := writePendingUpdate(pendingPath, &pendingUpdate{
				FromVersion: "0.1.0",
				ToVersion:   "0.2.0",
				StagedPath:  filepath.Join(dir, "staged.exe"),
				TargetPath:  filepath.Join(dir, "Vault-Agent.exe"),
				BackupPath:  backup,
			}); err != nil {
				t.Fatalf("写 pending 失败: %v", err)
			}

			res, err := ReconcileUpdate(dir, tc.current,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatalf("对账失败: %v", err)
			}
			if res.RolledBack {
				t.Fatal("当前版本不低于目标版本时不应回滚")
			}
			if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
				t.Fatalf("pending 应已清理，err=%v", err)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatalf("备份应已清理，err=%v", err)
			}
		})
	}
}

// TestReconcileUpdateKeepsPendingWhenCurrentOlder 锁定另一侧：版本确实没变/更旧时才
// 保留 pending（等待下一次启动重试），不能误清。
func TestReconcileUpdateKeepsPendingWhenCurrentOlder(t *testing.T) {
	dir := t.TempDir()
	pendingPath := filepath.Join(dir, pendingFileName)
	if err := writePendingUpdate(pendingPath, &pendingUpdate{
		FromVersion: "0.1.0",
		ToVersion:   "0.2.0",
		StagedPath:  filepath.Join(dir, "staged.exe"),
		TargetPath:  filepath.Join(dir, "Vault-Agent.exe"),
	}); err != nil {
		t.Fatalf("写 pending 失败: %v", err)
	}

	res, err := ReconcileUpdate(dir, "0.1.0",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if res.RolledBack {
		t.Fatal("首次发现未完成更新时不应立即回滚")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending 应保留供下次重试: %v", err)
	}
}
