package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDiffSyncPlanAndApply 覆盖"从目录创建差异盘"的核心：只把新增/修改/删除写进目标。
func TestDiffSyncPlanAndApply(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(src, "same.txt"), "same")
	writeFile(t, filepath.Join(src, "sub", "changed.txt"), "new-content")
	writeFile(t, filepath.Join(src, "added.txt"), "added")

	writeFile(t, filepath.Join(dst, "same.txt"), "same")
	writeFile(t, filepath.Join(dst, "sub", "changed.txt"), "old")
	writeFile(t, filepath.Join(dst, "extra.txt"), "extra")

	srcFiles, _, srcWarnings, err := scanDiffTree(ctx, src, DiffSyncModeSize, nil)
	if err != nil {
		t.Fatalf("扫描源目录失败: %v", err)
	}
	dstFiles, _, dstWarnings, err := scanDiffTree(ctx, dst, DiffSyncModeSize, nil)
	if err != nil {
		t.Fatalf("扫描目标目录失败: %v", err)
	}
	if len(srcWarnings) != 0 || len(dstWarnings) != 0 {
		t.Fatalf("不应有警告: src=%v dst=%v", srcWarnings, dstWarnings)
	}

	plan, _ := buildDiffSyncPlan(srcFiles, dstFiles, DiffSyncModeSize, true)
	if plan.added != 1 || plan.modified != 1 || plan.deleted != 1 {
		t.Fatalf("计划不正确: add=%d modify=%d delete=%d", plan.added, plan.modified, plan.deleted)
	}
	if len(plan.add) != 1 || plan.add[0].Path != "added.txt" {
		t.Fatalf("新增清单不正确: %+v", plan.add)
	}
	if len(plan.modify) != 1 || plan.modify[0].Path != "sub/changed.txt" {
		t.Fatalf("修改清单不正确: %+v", plan.modify)
	}
	if len(plan.del) != 1 || plan.del[0].Path != "extra.txt" {
		t.Fatalf("删除清单不正确: %+v", plan.del)
	}

	if _, err := applyDiffSyncPlan(ctx, src, dst, plan, nil); err != nil {
		t.Fatalf("应用计划失败: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "added.txt")); got != "added" {
		t.Fatalf("新增文件内容不正确: %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "sub", "changed.txt")); got != "new-content" {
		t.Fatalf("修改文件内容不正确: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("多余文件应被删除，实际 err=%v", err)
	}
	if got := readFile(t, filepath.Join(dst, "same.txt")); got != "same" {
		t.Fatalf("未变化文件被破坏: %q", got)
	}
}

// TestDiffSyncHashDetectsSameSizeChange 锁定 hash 模式能发现"大小相同、内容不同"的文件。
func TestDiffSyncHashDetectsSameSizeChange(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := t.TempDir()
	writeFile(t, filepath.Join(src, "same-size.txt"), "aaaa")
	writeFile(t, filepath.Join(dst, "same-size.txt"), "bbbb")

	srcFiles, _, _, err := scanDiffTree(ctx, src, DiffSyncModeHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	dstFiles, _, _, err := scanDiffTree(ctx, dst, DiffSyncModeHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := buildDiffSyncPlan(srcFiles, dstFiles, DiffSyncModeHash, true)
	if len(plan.modify) != 1 || plan.modify[0].Reason != "hash" {
		t.Fatalf("hash 模式应识别出内容变化: %+v", plan.modify)
	}
}

// TestDiffSyncKeepExtra 锁定 delete_extra=false 的语义：只增改，不删除。
func TestDiffSyncKeepExtra(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "a")
	writeFile(t, filepath.Join(dst, "keep.txt"), "keep")

	srcFiles, _, _, err := scanDiffTree(ctx, src, DiffSyncModeSize, nil)
	if err != nil {
		t.Fatal(err)
	}
	dstFiles, _, _, err := scanDiffTree(ctx, dst, DiffSyncModeSize, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := buildDiffSyncPlan(srcFiles, dstFiles, DiffSyncModeSize, false)
	if plan.deleted != 0 {
		t.Fatalf("delete_extra=false 时不应产生删除: %+v", plan.del)
	}
	if _, err := applyDiffSyncPlan(ctx, src, dst, plan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Fatalf("多余文件应保留: %v", err)
	}
}

// TestDiffSyncAbsRejectsEscape 防止相对路径逃出挂载根。
func TestDiffSyncAbsRejectsEscape(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../x", "a/../../x", "/etc/passwd", ""} {
		if _, err := diffSyncAbs(root, rel); err == nil {
			t.Fatalf("路径 %q 应被拒绝", rel)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
