package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
)

// TestRepairTargetJobCompletesWithSharedLocks 锁定 repair_target 的**锁归属**。
//
// 现场反馈："repair_target 这个任务要非常久，然后一直不成功"。根因是自锁：
// worker 按 LockKey=iscsi:<target> 先持锁，runRepairTarget 又对**同一个** Keyed 实例
// Acquire 同一把非可重入锁 —— 任务永远停在 running。修复后锁只由处理函数自己持有
// （enqueueTargetRepair 的 LockKey 留空），本测试用生产同款的"共享 locks 实例 + 真实
// worker"跑一遍，确保任务能跑到成功终态。
func TestRepairTargetJobCompletesWithSharedLocks(t *testing.T) {
	ctx := context.Background()
	backend := newFakeIscsiBackend()
	st := openAppTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 生产装配里 worker 与 IscsiService 共用同一个 Keyed（见 cmd/vault-server/main.go）。
	locks := lock.NewKeyed()
	worker := job.NewWorker(st, locks, job.Options{Logger: logger, PollInterval: 20 * time.Millisecond})
	svc := &IscsiService{Deps: Deps{
		Store: st,
		Log:   logger,
		Disk:  fakeDiskBackend{exists: true},
		Iscsi: backend,
		Locks: locks,
		Jobs:  worker,
	}}
	worker.Register(job.HandlerFunc{T: domain.JobRepairTarget, F: svc.runRepairTarget})

	target, _ := seedPublishFixture(t, svc, "vault-repair-lock")
	if pushed, _, err := svc.pushTarget(ctx, target, true); err != nil || !pushed {
		t.Fatalf("首次下发失败：pushed=%v err=%v", pushed, err)
	}
	// 平台侧目标被外部删除 → 对账只入队 repair_target。
	if err := backend.RemoveTarget(ctx, svc.targetIQN(target)); err != nil {
		t.Fatalf("删除平台目标失败：%v", err)
	}
	if err := svc.ReconcileTargets(ctx); err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	enqueued, err := st.GetLatestJobByRef(ctx, target.ID)
	if err != nil || enqueued == nil || enqueued.Type != domain.JobRepairTarget {
		t.Fatalf("对账应入队 repair_target：job=%v err=%v", enqueued, err)
	}
	// 任务锁由处理函数自己获取；worker 的 LockKey 必须留空（否则自锁）。
	if enqueued.LockKey != "" {
		t.Fatalf("repair_target 的 LockKey 必须留空（处理函数内部已加目标锁），实际 %q", enqueued.LockKey)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go worker.Start(runCtx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		latest, getErr := st.GetLatestJobByRef(ctx, target.ID)
		if getErr == nil && latest != nil && latest.State.IsTerminal() {
			if latest.State != domain.JobStateSucceeded {
				t.Fatalf("repair_target 任务未成功：state=%s last_error=%s", latest.State, latest.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("repair_target 任务在共享锁下卡死（worker 与处理函数重复获取同一把目标锁）")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := backend.pushes(); got != 2 {
		t.Fatalf("修复任务应重新下发目标，实际下发次数=%d", got)
	}
}

// TestEnqueueTargetRepairSkipsDoomedAndInFlight 锁定"修复任务只在真正可修时入队"：
//
//   - 背盘还在建 / 已建失败 → 修复必然失败，不入队（否则任务中心会被永远失败的 repair_target 刷屏）；
//   - 背盘正常但已有一条排队/执行中的修复 → 不再叠加（5 分钟分桶拦不住跨桶的"仍在执行"）。
func TestEnqueueTargetRepairSkipsDoomedAndInFlight(t *testing.T) {
	ctx := context.Background()
	backend := newFakeIscsiBackend()
	svc := newPushTestService(t, backend)
	target, _ := seedPublishFixture(t, svc, "vault-repair-skip")

	countRepairJobs := func() int {
		t.Helper()
		jobs, err := svc.Store.ListJobs(ctx, "", 100, 0)
		if err != nil {
			t.Fatalf("列任务失败：%v", err)
		}
		n := 0
		for i := range jobs {
			if jobs[i].Type == domain.JobRepairTarget && jobs[i].RefID == target.ID {
				n++
			}
		}
		return n
	}
	setDiskState := func(state domain.DiskState) {
		t.Helper()
		disk, err := svc.Store.GetDisk(ctx, *target.DiskID)
		if err != nil {
			t.Fatalf("读磁盘失败：%v", err)
		}
		disk.State = state
		if err := svc.Store.UpdateDisk(ctx, disk); err != nil {
			t.Fatalf("更新磁盘状态失败：%v", err)
		}
	}

	// 建盘中 / 建盘失败：都不该入队（等下一轮对账 / 人工处理）。
	for _, state := range []domain.DiskState{domain.DiskStateCreating, domain.DiskStateError} {
		setDiskState(state)
		svc.enqueueTargetRepair(ctx, target, "missing")
		if got := countRepairJobs(); got != 0 {
			t.Fatalf("磁盘状态为 %q 时不应入队修复任务，实际 %d 条", state, got)
		}
	}

	// 磁盘正常：入队一条；再模拟一轮对账，不应重复叠加。
	setDiskState(domain.DiskStateReady)
	svc.enqueueTargetRepair(ctx, target, "missing")
	if got := countRepairJobs(); got != 1 {
		t.Fatalf("磁盘正常时应入队 1 条修复任务，实际 %d 条", got)
	}
	svc.enqueueTargetRepair(ctx, target, "missing")
	if got := countRepairJobs(); got != 1 {
		t.Fatalf("已有排队中的修复任务时不应重复入队，实际 %d 条", got)
	}
}
