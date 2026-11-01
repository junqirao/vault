package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/domain"
)

// TestCreateExclusiveRepoFromSourceDirWaitsForDisk 锁定"从服务端目录导入的独享库"的等待语义：
//
// 盘是异步拷出来的，库若在数据拷完前就 active，客户端会立刻尝试挂载一块尚未建好的盘。
// 因此它和共享库一样先停在 creating，由建盘任务收尾转 active。
func TestCreateExclusiveRepoFromSourceDirWaitsForDisk(t *testing.T) {
	ctx := context.Background()
	svc, st, root := newRepoFlowService(t)
	owner := createFlowOwner(t, st)

	src := filepath.Join(root, "import-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建源目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.bin"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("写源文件失败：%v", err)
	}

	repo, disk, err := svc.Create(ctx, CreateRepoInput{
		Name:      "exclusive-import",
		Mode:      domain.RepoModeExclusive,
		OwnerID:   owner.ID,
		SourceDir: src,
		ClientOS:  "windows",
	})
	if err != nil {
		t.Fatalf("建库失败：%v", err)
	}
	if repo.State != domain.RepoStateCreating {
		t.Fatalf("导入中的独享库应停在 creating，实际 %q", repo.State)
	}
	if repo.Meta.Prepare == nil {
		t.Fatal("应留下建库进度快照")
	}
	if repo.Meta.Prepare.Phase != domain.RepoPhaseParent {
		t.Fatalf("进度阶段 = %q，期望 parent", repo.Meta.Prepare.Phase)
	}
	if repo.Meta.Prepare.BytesTotal != 5 {
		t.Fatalf("字节总量 = %d，期望 5", repo.Meta.Prepare.BytesTotal)
	}

	jobs, err := st.ListJobs(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	found := false
	for i := range jobs {
		if jobs[i].Type == domain.JobCreateVHDXFromDir && jobs[i].RefID == disk.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("应入队一条 JobCreateVHDXFromDir 建盘任务")
	}

	// 盘建好 → 库收敛为 active。
	diskSvc := &DiskService{Deps: svc.Deps, Repos: svc}
	diskSvc.finishDiskPrepare(ctx, disk)
	got, err := st.GetRepository(ctx, repo.ID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if got.State != domain.RepoStateActive {
		t.Fatalf("盘就绪后库应转为 active，实际 %q", got.State)
	}
	if got.Meta.Prepare != nil {
		t.Fatalf("完成后的进度快照应清空，实际 %+v", got.Meta.Prepare)
	}
}

// TestCopyEtaSeconds 覆盖"预计剩余"的换算口径：速度未知 / 已拷完 / 结果离谱时一律
// 返回 0（界面显示 "--"），不编一个不可信的数字。
func TestCopyEtaSeconds(t *testing.T) {
	cases := []struct {
		name   string
		total  int64
		copied int64
		speed  float64
		want   int64
	}{
		{name: "正常估算", total: 100, copied: 40, speed: 10, want: 6},
		{name: "已拷完", total: 100, copied: 100, speed: 10, want: 0},
		{name: "速度未知", total: 100, copied: 40, speed: 0, want: 0},
		{name: "总量未知", total: 0, copied: 0, speed: 10, want: 0},
		{name: "超过上限", total: 100, copied: 0, speed: 0.000001, want: 0},
	}
	for _, c := range cases {
		if got := copyEtaSeconds(c.total, c.copied, c.speed); got != c.want {
			t.Fatalf("%s：copyEtaSeconds(%d, %d, %v) = %d，期望 %d",
				c.name, c.total, c.copied, c.speed, got, c.want)
		}
	}
}
