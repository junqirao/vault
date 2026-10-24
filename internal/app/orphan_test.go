package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/store"
)

// orphanTestDisk 只补 Kind()：孤儿扫描的前提是 Windows 的 VHDX 目录布局（见孤立扫描的平台判定），
// 本测试用不到 DiskBackend 的其它方法，让它们保持 nil 即可（调用会 panic，正是我们想要的信号）。
type orphanTestDisk struct{ platform.DiskBackend }

func (orphanTestDisk) Kind() platform.Kind { return platform.KindWindows }

// newOrphanTestApp 构造带真实配置（storage.disks_dir / orphan_dir）、Store 与假磁盘后端的 App，
// 并登记一个启用存储根 root。
//
// 配置必须真的走一遍 config 加载：磁盘目录名是"根 + disks_dir"拼出来的，缺了它
// Resolve("") 会退化成根本身 —— 那正是自动清理最危险的形态（见 disksDirOf）。
func newOrphanTestApp(t *testing.T, root string) (*App, *store.Store) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	// 单引号标量：Windows 路径里的反斜杠在 YAML 双引号里是转义字符，必须原样保留。
	cfgYAML := "database:\n  driver: sqlite\nstorage:\n  whitelist_roots:\n    - '" + root + "'\n" +
		"  disks_dir: disks\n  orphan_dir: meta/orphan\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("写测试配置失败：%v", err)
	}
	watcher, err := config.NewWatcher(cfgPath, nil)
	if err != nil {
		t.Fatalf("加载测试配置失败：%v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	st := openAppTestStore(t)
	app := &App{Deps: Deps{
		Store: st,
		Cfg:   watcher,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Disk:  orphanTestDisk{},
	}}
	if err := st.CreateStorage(context.Background(), &domain.Storage{
		Name:    "orphan-test",
		Path:    root,
		Enabled: true,
	}); err != nil {
		t.Fatalf("登记存储根失败：%v", err)
	}
	return app, st
}

// writeOrphanFile 建出目录与文件，返回绝对路径。
func writeOrphanFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	return p
}

// TestReconcileOrphanFilesAutoDeletes 锁定"存储范围内全权自动管理"的口径：
// 超过观察窗口的未登记 VHDX（含差异盘/母盘/其它）与历史 meta/orphan/ 文件都由对账
// 自动删除；已登记的盘与刚出现（保护导入）的文件必须保留。
func TestReconcileOrphanFilesAutoDeletes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newOrphanTestApp(t, root)

	old := time.Now().Add(-48 * time.Hour)
	orphanDiff := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "old"), "orphan.vhdx", 4096)
	orphanOther := writeOrphanFile(t, filepath.Join(root, "disks", "misc"), "stray.vhdx", 512)
	fresh := writeOrphanFile(t, filepath.Join(root, "disks", "import"), "new.vhdx", 256)
	registered := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "keep"), "in-use.vhdx", 1024)
	staged := writeOrphanFile(t, filepath.Join(root, "meta", "orphan"), "old.vhdx", 64)
	for _, p := range []string{orphanDiff, orphanOther, staged} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("调整文件时间失败：%v", err)
		}
	}

	repoID := seedRepo(t, st)
	if err := st.CreateDisk(ctx, &domain.Disk{
		RepoID:   repoID,
		Kind:     domain.DiskKindDiff,
		VHDXPath: registered,
		State:    domain.DiskStateReady,
	}); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	rep := &ReconcileReport{Issues: []ReconcileIssue{}, NotChecked: []string{}}
	report := func(kind, ref, detail string, fixed bool) {
		rep.Issues = append(rep.Issues, ReconcileIssue{Kind: kind, Ref: ref, Detail: detail, AutoFixed: fixed})
	}
	app.reconcileOrphanFiles(ctx, rep, report, app.Log, map[string]bool{})

	for _, p := range []string{orphanDiff, orphanOther, staged} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("过期孤儿文件应已被自动删除：%s（err=%v）", p, err)
		}
	}
	for _, p := range []string{registered, fresh} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("登记盘/观察窗口内的新文件不得被删除：%s（err=%v）", p, err)
		}
	}
	deleted := 0
	for _, issue := range rep.Issues {
		if issue.AutoFixed && (issue.Kind == "orphan_vhdx" || issue.Kind == "orphan_vhdx.delete_failed") {
			deleted++
		}
	}
	if deleted < 3 {
		t.Fatalf("应记录自动删除的 3 个文件，实际 issues=%+v", rep.Issues)
	}
}
