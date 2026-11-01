package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
	"vault/internal/store"
)

// createTestDisk 只实现建库/分配路径用到的只读方法；其余由嵌入接口兜底。
type createTestDisk struct {
	platform.DiskBackend
}

func (createTestDisk) Kind() platform.Kind { return platform.KindWindows }

func (createTestDisk) DiskRef(root, rel string) (string, error) {
	return filepath.Join(root, rel), nil
}

func (createTestDisk) Exists(string) bool { return false }

func (createTestDisk) Fingerprint(string) (string, error) { return "fp-create-test", nil }

func (createTestDisk) PhysicalSize(string) (int64, error) { return 0, nil }

// newRepoFlowService 造一个带真实 Store / 存储根 / 未启动 worker 的 RepoService。
//
// worker 不启动：本文件的用例只断言"任务是否按正确的类型入队"，以及库/盘状态是否
// 在正确的时机推进，不需要真的跑建盘。
func newRepoFlowService(t *testing.T) (*RepoService, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := "database:\n  driver: sqlite\nstorage:\n  whitelist_roots:\n    - '" + root + "'\n" +
		"  disks_dir: disks\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("写测试配置失败：%v", err)
	}
	watcher, err := config.NewWatcher(cfgPath, nil)
	if err != nil {
		t.Fatalf("加载测试配置失败：%v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	st := openAppTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	locks := lock.NewKeyed()
	svc := &RepoService{Deps: Deps{
		Store: st,
		Cfg:   watcher,
		Log:   logger,
		Disk:  createTestDisk{},
		Locks: locks,
		Jobs:  job.NewWorker(st, locks, job.Options{Logger: logger}),
	}}
	if err := st.CreateStorage(context.Background(), &domain.Storage{
		Name:    "flow-test",
		Path:    root,
		Enabled: true,
	}); err != nil {
		t.Fatalf("登记测试存储失败：%v", err)
	}
	return svc, st, root
}

func createFlowOwner(t *testing.T, st *store.Store) *domain.User {
	t.Helper()
	user := &domain.User{Username: "flow-owner", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("建用户失败：%v", err)
	}
	return user
}

// TestCreateSharedRepoWithoutPreallocateWaitsForParent 锁定"没勾预创建"这条路的核心契约：
//
// 共享库的母盘是异步建出来的。库若在母盘文件出现之前就 active，用户点分配/挂载就会
// 抢在母盘前面派生差异盘 —— 派生以"父文件不存在"失败（不是共享冲突，不走退避），
// 几次重试在几十毫秒内耗尽后盘被标成 error，从此既挂不上也卸不掉（现场反馈）。
//
// 因此不预创建的共享库同样先停在 creating，母盘建好后才转 active；"预创建"只决定
// 要不要顺带把池位（差异盘 + 目标）也提前建好。
func TestCreateSharedRepoWithoutPreallocateWaitsForParent(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newRepoFlowService(t)
	owner := createFlowOwner(t, st)

	repo, disk, err := svc.Create(ctx, CreateRepoInput{
		Name:      "no-pool",
		Mode:      domain.RepoModeShared,
		OwnerID:   owner.ID,
		SizeBytes: 1 << 20,
		ClientOS:  "windows",
		// Preallocate 缺省 false：不预建池位。
	})
	if err != nil {
		t.Fatalf("建库失败：%v", err)
	}
	if repo.State != domain.RepoStateCreating {
		t.Fatalf("不预创建的共享库也应先等母盘（state=creating），实际 %q", repo.State)
	}
	if repo.Meta.Pool {
		t.Fatal("没勾预创建时不应标记为池化库")
	}
	if repo.Meta.Prepare == nil || repo.Meta.Prepare.Phase != domain.RepoPhaseParent {
		t.Fatalf("应留下母盘阶段的进度快照，实际 %+v", repo.Meta.Prepare)
	}
	if disk.Kind != domain.DiskKindParent {
		t.Fatalf("共享库应建母盘，实际 kind=%q", disk.Kind)
	}

	jobs, err := st.ListJobs(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	found := false
	for i := range jobs {
		if jobs[i].Type == domain.JobCreateVHDX && jobs[i].RefID == disk.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("应入队一条 JobCreateVHDX 建母盘任务")
	}

	// 母盘建好 → 库收敛为 active（JobCreateVHDX 的收尾，见 DiskService.finishDiskPrepare）。
	diskSvc := &DiskService{Deps: svc.Deps, Repos: svc}
	diskSvc.finishDiskPrepare(ctx, disk)
	got, err := st.GetRepository(ctx, repo.ID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if got.State != domain.RepoStateActive {
		t.Fatalf("母盘就绪后库应转为 active，实际 %q", got.State)
	}
	if got.Meta.Prepare != nil {
		t.Fatalf("完成后的进度快照应清空，实际 %+v", got.Meta.Prepare)
	}
}

// TestCreateSharedRepoWithoutPreallocateMarksErrorWhenParentFails 锁定失败回滚：母盘任务
// 彻底失败时，库必须从 creating 转 error，否则前端永远"创建中"、既分配不了也挂载不了。
func TestCreateSharedRepoWithoutPreallocateMarksErrorWhenParentFails(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newRepoFlowService(t)
	owner := createFlowOwner(t, st)

	repo, disk, err := svc.Create(ctx, CreateRepoInput{
		Name:      "no-pool-fail",
		Mode:      domain.RepoModeShared,
		OwnerID:   owner.ID,
		SizeBytes: 1 << 20,
		ClientOS:  "windows",
	})
	if err != nil {
		t.Fatalf("建库失败：%v", err)
	}

	if err := svc.MarkPrepareFailedByDisk(ctx, disk.ID, errors.New("parent build failed")); err != nil {
		t.Fatalf("回滚存储库失败：%v", err)
	}
	got, err := st.GetRepository(ctx, repo.ID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if got.State != domain.RepoStateError {
		t.Fatalf("母盘任务失败后库应为 error，实际 %q", got.State)
	}
	if got.Meta.Prepare == nil || got.Meta.Prepare.Error == "" {
		t.Fatalf("应留下失败码供界面展示，实际 %+v", got.Meta.Prepare)
	}
}

// TestAllocateReplacesFailedDiffDisk 锁定"历史坏盘自愈"：差异盘被标成 error 后不能再被
// 复用（父文件可能根本不存在），分配要作废旧盘并重建 —— 否则用户每次挂载都只得到
// disk.create_failed，而且客户端没有删除分配的入口，等于永久锁死。
func TestAllocateReplacesFailedDiffDisk(t *testing.T) {
	ctx := context.Background()
	svc, st, root := newRepoFlowService(t)
	owner := createFlowOwner(t, st)

	cond := domain.ParentIdle
	repo := &domain.Repository{
		Name:            "rebuild",
		Mode:            domain.RepoModeShared,
		OwnerID:         owner.ID,
		MaxDiffDisks:    5,
		State:           domain.RepoStateActive,
		ParentCondition: &cond,
	}
	if err := st.CreateRepository(ctx, repo); err != nil {
		t.Fatalf("建存储库失败：%v", err)
	}
	parent := &domain.Disk{
		RepoID:    repo.ID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join(root, "parents", repo.ID, "base.vhdx"),
		SizeBytes: 1 << 20,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘失败：%v", err)
	}
	parentID := parent.ID
	repo.ParentDiskID = &parentID
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("回写母盘关联失败：%v", err)
	}

	bad := &domain.Disk{
		RepoID:    repo.ID,
		Kind:      domain.DiskKindDiff,
		ParentID:  &parentID,
		VHDXPath:  filepath.Join(root, "diffs", repo.ID, "broken.vhdx"),
		SizeBytes: parent.SizeBytes,
		State:     domain.DiskStateError,
	}
	if err := st.CreateDisk(ctx, bad); err != nil {
		t.Fatalf("建坏差异盘失败：%v", err)
	}
	old := &domain.Allocation{
		RepoID: repo.ID,
		DiskID: bad.ID,
		UserID: owner.ID,
		State:  domain.AllocationStateAllocated,
	}
	if err := st.CreateAllocation(ctx, old); err != nil {
		t.Fatalf("建旧分配失败：%v", err)
	}

	got, err := svc.Allocate(ctx, AllocateInput{RepoID: repo.ID, UserID: owner.ID, OperatorID: owner.ID})
	if err != nil {
		t.Fatalf("分配应重建坏盘而不是失败：%v", err)
	}
	if got.DiskID == bad.ID {
		t.Fatal("分配不能复用 state=error 的差异盘")
	}
	if _, err := st.GetAllocation(ctx, old.ID); !isNotFound(err) {
		t.Fatalf("旧分配应被删除，err=%v", err)
	}

	jobs, err := st.ListJobs(ctx, "", 50, 0)
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	var rebuild, cleanup bool
	for i := range jobs {
		switch {
		case jobs[i].Type == domain.JobCreateDiff && jobs[i].RefID == got.DiskID:
			rebuild = true
		case jobs[i].Type == domain.JobDeleteDisk && jobs[i].RefID == bad.ID:
			cleanup = true
		}
	}
	if !rebuild {
		t.Fatal("应入队新差异盘的派生任务")
	}
	if !cleanup {
		t.Fatal("应入队旧坏盘的删除任务")
	}
}
