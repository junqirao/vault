package app

import (
	"context"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// diffOperationJobTypes 是"会长时间占用整块盘"的差异盘工具任务。
//
// 它们都必须在维护态执行，并且同一存储库同时只能有一个：
//   - create_diff_from_dir：服务端目录增量建盘 + 写入；
//   - plan_client_diff：客户端目录的清单比对 + 删除；
//   - merge_diff：把选中的差异盘合并进母盘。
//
// 客户端上传（CompleteClientDiff）不是独立 job，但它持有同一把 RepoKey，
// 由请求内的锁与这里的 active job 闸门共同覆盖。
var diffOperationJobTypes = map[domain.JobType]bool{
	domain.JobCreateDiffFromDir: true,
	domain.JobPlanClientDiff:    true,
	domain.JobMergeDiff:         true,
}

// activeDiffOperation 返回该存储库上仍在排队/执行的差异盘工具任务；没有则返回 nil。
//
// 用途：维护态的"退出维护 / 编辑母盘文件 / 清理差异盘 / 扩容 / 再发起一个操作"都必须
// 先过这道闸门。只用 RepoKey 串行化任务本身不够——那些请求不会排队等锁，而是会立刻改变
// 母盘用途或删除正在使用的盘。
func (d Deps) activeDiffOperation(ctx context.Context, repoID string) (*domain.Job, error) {
	if d.Store == nil || repoID == "" {
		return nil, nil
	}
	refs := map[string]bool{repoID: true}
	if disks, err := d.Store.ListDisksByRepo(ctx, repoID); err == nil {
		for i := range disks {
			refs[disks[i].ID] = true
		}
	}
	for _, state := range []domain.JobState{domain.JobStatePending, domain.JobStateRunning} {
		jobs, err := d.Store.ListJobs(ctx, state, 500, 0)
		if err != nil {
			return nil, err
		}
		for i := range jobs {
			j := &jobs[i]
			if diffOperationJobTypes[j.Type] && refs[j.RefID] {
				return j, nil
			}
		}
	}
	return nil, nil
}

// ActiveDiffOperation 返回该存储库当前正在执行的差异盘工具任务（供界面恢复/锁定展示）。
func (s *RepoService) ActiveDiffOperation(ctx context.Context, repoID string) (*domain.Job, error) {
	return s.activeDiffOperation(ctx, repoID)
}

// CancelActiveDiffOperation 取消该存储库正在执行的差异盘工具任务。
//
// 与超级管理员的通用任务取消分开：库管理者也需要能取消自己发起的长时间建盘/合并，
// 否则锁在维护态时只能等它结束。
func (s *RepoService) CancelActiveDiffOperation(ctx context.Context, repoID string) error {
	job, err := s.activeDiffOperation(ctx, repoID)
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}
	return s.Jobs.Cancel(ctx, job.ID)
}

// ensureNoActiveDiffJob 只检查"排队/执行中的任务"。
//
// 清理差异盘要用这个版本：plan 成功、等待客户端上传时没有 active job，但用户正需要
// 用清理功能把这块 creating 的残盘删掉，不能把它也挡住。
func (d Deps) ensureNoActiveDiffJob(ctx context.Context, repoID string) error {
	job, err := d.activeDiffOperation(ctx, repoID)
	if err != nil {
		return err
	}
	if job != nil {
		return apperr.RepoOperationInProgress(string(job.Type), job.ID)
	}
	return nil
}

// ensureNoActiveDiffOperation 是给 API 入口用的统一闸门。
func (d Deps) ensureNoActiveDiffOperation(ctx context.Context, repoID string) error {
	if err := d.ensureNoActiveDiffJob(ctx, repoID); err != nil {
		return err
	}
	// 客户端目录是两阶段：plan 任务成功后差异盘停在 creating，等客户端上传内容。
	// 这时已经"没有 active job"，但操作仍在进行；维护态下任何差异盘 creating 都必须
	// 视为整库锁定（正常态的池位派生不在此列，因为差异盘工具只在维护态可用）。
	repo, rErr := d.Store.GetRepository(ctx, repoID)
	if rErr == nil && repo.Condition() == domain.ParentMaintenance {
		disks, lErr := d.Store.ListDisksByRepo(ctx, repoID)
		if lErr != nil {
			return lErr
		}
		for i := range disks {
			if disks[i].Kind == domain.DiskKindDiff && disks[i].State == domain.DiskStateCreating {
				return apperr.RepoOperationInProgress("diff_upload", disks[i].ID)
			}
		}
	}
	return nil
}
