package app

import (
	"context"
	"path/filepath"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
)

// TestMergeDiffOnlyMaintenanceAndSelectedDiff 锁定"合并差异盘更新母盘"的入口条件：
// 仅维护态、差异盘必须属于该库/该母盘、且必须基于当前母盘版本。
func TestMergeDiffOnlyMaintenanceAndSelectedDiff(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	parent := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "parents", "base.vhdx"),
		SizeBytes: 1 << 30,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘失败：%v", err)
	}
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	repo.ParentDiskID = &parent.ID
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}

	diff := &domain.Disk{
		RepoID:        repoID,
		Kind:          domain.DiskKindDiff,
		VHDXPath:      filepath.Join("C:", "vault", "disks", "diffs", "d1.vhdx"),
		ParentID:      &parent.ID,
		ParentVersion: repo.ParentVersion,
		SizeBytes:     parent.SizeBytes,
		State:         domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, diff); err != nil {
		t.Fatal(err)
	}

	worker := job.NewWorker(st, lock.NewKeyed(), job.Options{Logger: testLogger()})
	svc := &RepoService{Deps: Deps{Store: st, Log: testLogger(), Jobs: worker}}

	// 正常态：合并会改写母盘基线，必须拒绝。
	if _, err := svc.MergeDiff(ctx, repoID, diff.ID, "op"); err == nil {
		t.Fatal("正常态不应允许合并差异盘")
	} else if got := apperr.CodeOf(err); got != "repo.parent_condition_violation" {
		t.Fatalf("错误码 = %q，期望 repo.parent_condition_violation（err=%v）", got, err)
	}

	if err := st.SetParentCondition(ctx, repoID, domain.ParentMaintenance, false); err != nil {
		t.Fatal(err)
	}
	// 版本不一致的差异盘：内容不是当前基线的增量，必须拒绝。
	stale := &domain.Disk{
		RepoID:        repoID,
		Kind:          domain.DiskKindDiff,
		VHDXPath:      filepath.Join("C:", "vault", "disks", "diffs", "d2.vhdx"),
		ParentID:      &parent.ID,
		ParentVersion: repo.ParentVersion + 1,
		SizeBytes:     parent.SizeBytes,
		State:         domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MergeDiff(ctx, repoID, stale.ID, "op"); err == nil {
		t.Fatal("版本不一致的差异盘应被拒绝")
	} else if got := apperr.CodeOf(err); got != "disk.parent_version_mismatch" {
		t.Fatalf("错误码 = %q，期望 disk.parent_version_mismatch（err=%v）", got, err)
	}

	// 维护态：允许合并，并返回异步任务；ref_id 是存储库 ID（作业中心据此跳详情）。
	j, err := svc.MergeDiff(ctx, repoID, diff.ID, "op")
	if err != nil {
		t.Fatalf("维护态应允许合并：%v", err)
	}
	if j.Type != domain.JobMergeDiff || j.RefID != repoID || j.State != domain.JobStatePending {
		t.Fatalf("任务 = %s/%s/%s，期望 merge_diff/%s/pending", j.Type, j.RefID, j.State, repoID)
	}

	// 已有任务在执行时，再点一次合并必须被整库锁拒绝。
	if _, err := svc.MergeDiff(ctx, repoID, diff.ID, "op"); err == nil {
		t.Fatal("已有差异盘任务时不应允许再次合并")
	} else if got := apperr.CodeOf(err); got != "repo.operation_in_progress" {
		t.Fatalf("错误码 = %q，期望 repo.operation_in_progress（err=%v）", got, err)
	}
}
