package store

import (
	"context"
	"testing"

	"vault/internal/domain"
)

// TestDeleteDuplicatePendingJobs 锁定"对账收敛任务不能无限堆积"的清理规则：
// 同一类型、同一关联对象只保留最早的一条排队任务，重复的直接删除；其它类型、已结束的
// 任务以及仍在运行的任务都不能被误删（正在跑的任务可能真的在执行）。
func TestDeleteDuplicatePendingJobs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}

	create := func(jobType domain.JobType, ref, idem string, state domain.JobState, created int64) string {
		t.Helper()
		id, _, err := s.CreateJob(ctx, &domain.Job{
			Type:      jobType,
			RefID:     ref,
			IdemKey:   idem,
			State:     state,
			CreatedAt: created,
		})
		if err != nil {
			t.Fatalf("建任务失败: %v", err)
		}
		return id
	}

	keep := create(domain.JobRepairTarget, "target-a", "repair:a:1", domain.JobStatePending, 1)
	dup1 := create(domain.JobRepairTarget, "target-a", "repair:a:2", domain.JobStatePending, 2)
	dup2 := create(domain.JobRepairTarget, "target-a", "repair:a:3", domain.JobStatePending, 3)
	otherTarget := create(domain.JobRepairTarget, "target-b", "repair:b:1", domain.JobStatePending, 1)
	running := create(domain.JobRepairTarget, "target-a", "repair:a:run", domain.JobStateRunning, 4)
	finished := create(domain.JobRepairTarget, "target-a", "repair:a:done", domain.JobStateSucceeded, 1)
	otherType := create(domain.JobPublish, "target-a", "publish:a:1", domain.JobStatePending, 1)

	n, err := s.DeleteDuplicatePendingJobs(ctx, domain.JobRepairTarget)
	if err != nil {
		t.Fatalf("清理重复任务失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应删除 2 条重复排队任务，实际 %d", n)
	}

	for _, id := range []string{keep, otherTarget, running, finished, otherType} {
		if _, err := s.GetJob(ctx, id); err != nil {
			t.Fatalf("任务 %s 不应被删除: %v", id, err)
		}
	}
	for _, id := range []string{dup1, dup2} {
		if _, err := s.GetJob(ctx, id); err == nil {
			t.Fatalf("重复任务 %s 应已删除", id)
		}
	}
}
