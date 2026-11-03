package app

import (
	"context"
	"path/filepath"
	"testing"

	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/store"
)

// createAutoCompactDisk 建一块已就绪、未本地挂载的差异盘。
func createAutoCompactDisk(t *testing.T, st *store.Store, repoID, name string) *domain.Disk {
	t.Helper()
	ctx := context.Background()
	disk := &domain.Disk{
		RepoID:   repoID,
		Kind:     domain.DiskKindDiff,
		VHDXPath: filepath.Join(t.TempDir(), name),
		State:    domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}
	return disk
}

// TestAutoCompactQueuesIdleDisksOnlyOnce 覆盖自动回收的默认开启、冷却去重与开关关闭。
func TestAutoCompactQueuesIdleDisksOnlyOnce(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	disk1 := createAutoCompactDisk(t, st, repoID, "disk1.vhdx")
	svc := &DiskService{Deps: Deps{
		Store: st,
		Log:   testLogger(),
		Disk:  fakeDiskBackend{exists: true},
		Jobs:  job.NewWorker(st, lock.NewKeyed(), job.Options{Logger: testLogger()}),
	}}

	// 1) 默认未配置 => 开启，第一轮应入队一块盘的压缩任务。
	if err := svc.AutoCompact(ctx); err != nil {
		t.Fatalf("自动回收失败：%v", err)
	}
	first, err := st.GetLatestJobByRef(ctx, disk1.ID)
	if err != nil || first == nil {
		t.Fatalf("期望为默认开启的库入队压缩任务，job=%v err=%v", first, err)
	}
	if first.Type != domain.JobCompact {
		t.Fatalf("任务类型 = %s，期望 %s", first.Type, domain.JobCompact)
	}

	// 2) 任务结束后 24 小时内不得重复入队（避免每小时扫描都压一遍）。
	if err := st.FinishJob(ctx, first.ID, domain.JobStateSucceeded, ""); err != nil {
		t.Fatalf("结束任务失败：%v", err)
	}
	if err := svc.AutoCompact(ctx); err != nil {
		t.Fatalf("第二轮自动回收失败：%v", err)
	}
	again, err := st.GetLatestJobByRef(ctx, disk1.ID)
	if err != nil {
		t.Fatalf("回查任务失败：%v", err)
	}
	if again == nil || again.ID != first.ID {
		t.Fatalf("冷却期内不应重复入队，got=%v", again)
	}

	// 3) 显式关闭后，新的空闲盘不再入队。
	off := false
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	repo.Meta.AutoCompact = &off
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("关闭自动回收失败：%v", err)
	}
	disk2 := createAutoCompactDisk(t, st, repoID, "disk2.vhdx")
	if err := svc.AutoCompact(ctx); err != nil {
		t.Fatalf("关闭后自动回收失败：%v", err)
	}
	if j, _ := st.GetLatestJobByRef(ctx, disk2.ID); j != nil {
		t.Fatalf("关闭自动回收后不应入队，got=%v", j)
	}

	// 4) 重新开启，但只要库下还有未结束租约（active/expired），就不能动盘。
	on := true
	repo.Meta.AutoCompact = &on
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("重新开启自动回收失败：%v", err)
	}
	allocID := "alloc-" + disk2.ID
	if err := st.CreateAllocation(ctx, &domain.Allocation{
		ID:     allocID,
		RepoID: repoID,
		DiskID: disk2.ID,
		UserID: userID,
		State:  domain.AllocationStateMounted,
	}); err != nil {
		t.Fatalf("建分配记录失败：%v", err)
	}
	lease := &domain.Lease{
		AllocationID: allocID,
		UserID:       userID,
		ClientID:     "client-1",
		State:        domain.LeaseStateActive,
	}
	if err := st.UpsertLease(ctx, lease); err != nil {
		t.Fatalf("建租约失败：%v", err)
	}
	if err := svc.AutoCompact(ctx); err != nil {
		t.Fatalf("租约存在时自动回收失败：%v", err)
	}
	if j, _ := st.GetLatestJobByRef(ctx, disk2.ID); j != nil {
		t.Fatalf("有活跃租约时不应入队，got=%v", j)
	}

	// 5) 客户端显式卸载（released）后，下一轮可以回收。
	if err := st.UpdateLeaseState(ctx, lease.ID, domain.LeaseStateReleased); err != nil {
		t.Fatalf("释放租约失败：%v", err)
	}
	if err := svc.AutoCompact(ctx); err != nil {
		t.Fatalf("释放后自动回收失败：%v", err)
	}
	if j, _ := st.GetLatestJobByRef(ctx, disk2.ID); j == nil || j.Type != domain.JobCompact {
		t.Fatalf("释放租约后应入队压缩任务，got=%v", j)
	}
}
