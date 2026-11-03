package app

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
)

// mergeDiffPayload 是维护态「合并差异盘更新母盘」任务的入参。
type mergeDiffPayload struct {
	RepoID     string `json:"repo_id"`
	DiffDiskID string `json:"diff_disk_id"`
	OperatorID string `json:"operator_id,omitempty"`
}

// MergeDiff 在维护态下把选中的差异盘合并进母盘（异步任务）。
//
// 语义（与前端确认框一致）：
//   - 只允许维护态：合并会改写母盘内容，正常态下它还是所有差异盘的基线；
//   - 选中的差异盘必须基于**当前母盘版本**，否则它的内容不是当前基线的增量；
//   - 同一母盘下的**其它差异盘会被删除**——母盘内容变化后它们全部失效；
//   - 合并成功后自增 parent_version 并回到 idle，库重新可用。
func (s *RepoService) MergeDiff(ctx context.Context, repoID, diffDiskID, operatorID string) (*domain.Job, error) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if !repo.IsShared() {
		return nil, apperr.New("repo.not_shared", 409).WithArg("mode", string(repo.Mode))
	}
	if repo.Condition() != domain.ParentMaintenance {
		return nil, apperr.RepoParentConditionViolation(
			string(repo.Condition()), string(domain.ParentMaintenance))
	}
	if err := s.ensureNoActiveDiffOperation(ctx, repo.ID); err != nil {
		return nil, err
	}
	if repo.ParentDiskID == nil {
		return nil, apperr.New("repo.disk_missing", 500)
	}

	diffID := strings.TrimSpace(diffDiskID)
	if diffID == "" {
		return nil, apperr.InvalidParam("diff_disk_id")
	}
	disk, err := s.Store.GetDisk(ctx, diffID)
	if err != nil {
		return nil, err
	}
	if disk.Kind != domain.DiskKindDiff || disk.RepoID != repo.ID ||
		disk.ParentID == nil || *disk.ParentID != *repo.ParentDiskID {
		return nil, apperr.InvalidParam("diff_disk_id")
	}
	if !isDiskBuilt(disk.State) {
		return nil, apperr.DiskNotReady().WithArg("disk_id", disk.ID).WithArg("state", string(disk.State))
	}
	if disk.ParentVersion != repo.ParentVersion {
		return nil, apperr.DiskParentVersionMismatch(disk.ParentVersion, repo.ParentVersion)
	}
	if disk.Mounted {
		return nil, apperr.DiskBusy()
	}
	// Windows 没有 Hyper-V 模块时 Merge-VHD 不可用：提交前就说清楚，别让任务白重试三次。
	if bc, ok := s.Disk.(platform.BackendCapabilities); ok {
		if available, known := bc.Capabilities(ctx)["hyperv"]; known && !available {
			return nil, apperr.New("platform.merge_unavailable", http.StatusNotImplemented).
				WithArg("reason", "hyperv_unavailable")
		}
	}

	payload := mergeDiffPayload{
		RepoID:     repo.ID,
		DiffDiskID: disk.ID,
		OperatorID: operatorID,
	}
	// 幂等键带随机串：任务失败后"再点一次"必须创建新任务，而不是命中旧的失败记录。
	// 同一库的合并与派生/清理共用 RepoKey，串行化对母盘的独占操作。
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobMergeDiff, repo.ID,
		"merge_diff:"+repo.ID+":"+disk.ID+":"+uuid.NewString(), lock.RepoKey(repo.ID), payload)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "repo.parent.merge_diff", "repo:"+repo.ID,
		"diff_disk="+disk.ID+" job="+j.ID, domain.AuditResultOK)
	return j, nil
}

// runMergeDiff 是 JobMergeDiff 的实现。
//
// 顺序刻意是"先清场、再合并、最后收尾"：
//  1. 卸掉母盘的服务端本地挂载、撤销全部租约、拆除所有差异盘的 iSCSI 目标；
//  2. 删除**未选中**的差异盘（母盘一变，它们全部失效）；
//  3. 把选中的差异盘离线合并进母盘（平台侧会删除子盘）；
//  4. 删除选中盘的记录/分配，自增 parent_version、重算指纹并回到 idle。
//
// 幂等性：每次尝试都按当前仍存在的差异盘重新处理；若子盘文件已经消失（上次已合并成功、
// 但后续收尾失败），则跳过平台合并，只补做 DB 收尾。
func (s *DiskService) runMergeDiff(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p mergeDiffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	// 已经结束维护（上一次尝试成功、只是任务状态没落库）：合并已完成，直接成功。
	if repo.Condition() != domain.ParentMaintenance {
		rep.Progress(100)
		return nil
	}
	if repo.ParentDiskID == nil {
		return apperr.New("repo.disk_missing", 500)
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return err
	}
	if s.Disk == nil || s.Repos == nil {
		return errNotImplemented
	}
	rep.Progress(5)

	// ① 母盘本地挂载会占住 VHDX，合并/删除差异盘都会撞共享冲突。
	s.Repos.UnmountParentVolume(ctx, repo.ID)
	// 进程内挂载表可能因重启丢失（Windows 的 attach 句柄会随进程消失，Linux 的挂载不会）：
	// 再按引用做一次幂等卸载，覆盖"重启后残留本地挂载"的情况。
	if s.Vol != nil {
		if err := s.Vol.UnmountVolume(ctx, parent.VHDXPath, ""); err != nil {
			s.Log.Warn("合并前卸载母盘本地挂载失败（继续尝试）", "repo_id", repo.ID, "error", err)
		}
	}

	// ② 撤销在线租约：客户端收到断开指令后会主动卸载，平台侧目标紧接着拆除。
	if s.Leases != nil {
		leases, lErr := s.Store.ListActiveLeasesByRepo(ctx, repo.ID)
		if lErr != nil {
			return lErr
		}
		for i := range leases {
			if err := s.Leases.RevokeWithReason(ctx, leases[i].ID, "system", RevokeReasonRepoMaintenance); err != nil {
				s.Log.Warn("合并差异盘：撤销租约失败（继续拆除目标）",
					"repo_id", repo.ID, "lease_id", leases[i].ID, "error", err)
			}
		}
	}
	rep.Progress(12)

	// ③ 拆除全部差异盘的 iSCSI 目标。先拆目标再删文件，否则 VHDX 被占用。
	disks, err := s.Store.ListDiffDisksByParent(ctx, parent.ID)
	if err != nil {
		return err
	}
	if s.IscsiSvc != nil {
		for i := range disks {
			s.IscsiSvc.TeardownDiskTargets(ctx, disks[i].ID)
		}
		// 母盘也可能残留临时共享目标（历史数据/上一次失败的任务）：一并拆掉。
		s.IscsiSvc.TeardownDiskTargets(ctx, parent.ID)
	}
	if s.Iscsi != nil {
		// 解除虚拟盘登记：Merge-VHD / lvconvert --merge 都要求两张盘离线。
		if err := s.Iscsi.RemoveVirtualDisk(ctx, parent.VHDXPath); err != nil {
			s.Log.Warn("合并前移除母盘 iSCSI 登记失败", "disk_id", parent.ID, "error", err)
		}
	}
	rep.Progress(20)

	var selected *domain.Disk
	for i := range disks {
		d := &disks[i]
		if d.ID == p.DiffDiskID {
			selected = d
			continue
		}
		// 未选中的差异盘：母盘合并后它们全部基于旧基线，必须删除。
		if err := s.deleteDiskArtifacts(ctx, d.ID, ""); err != nil {
			return err
		}
	}
	rep.Progress(45)

	// ④ 合并选中的差异盘。记录可能已被并发的清理任务删掉（幂等空跑）。
	if selected != nil && s.Disk != nil {
		if selected.Kind != domain.DiskKindDiff || selected.ParentID == nil || *selected.ParentID != parent.ID {
			return apperr.InvalidParam("diff_disk_id")
		}
		if !isDiskBuilt(selected.State) {
			return apperr.DiskNotReady().WithArg("disk_id", selected.ID).WithArg("state", string(selected.State))
		}
		merged := false
		if s.Iscsi != nil {
			if err := s.Iscsi.RemoveVirtualDisk(ctx, selected.VHDXPath); err != nil {
				s.Log.Warn("合并前移除差异盘 iSCSI 登记失败", "disk_id", selected.ID, "error", err)
			}
		}
		if s.Disk.Exists(selected.VHDXPath) {
			if err := s.Disk.MergeDiff(ctx, selected.VHDXPath, parent.VHDXPath); err != nil {
				return err
			}
			merged = true
			rep.Progress(75)
		}
		// 平台合并成功时子盘通常已被删除；这里再兜一次（Windows 脚本会尽量删，
		// Linux 的 lvconvert --merge 会移除快照）。
		if s.Disk.Exists(selected.VHDXPath) {
			if err := s.removeDiskFile(ctx, selected.VHDXPath); err != nil {
				return err
			}
		}
		userID := s.allocatedUserID(ctx, selected.ID)
		if s.Iscsi != nil {
			if err := s.Iscsi.RemoveVirtualDisk(ctx, selected.VHDXPath); err != nil {
				s.Log.Warn("移除已合并差异盘的 iSCSI 登记失败", "disk_id", selected.ID, "error", err)
			}
		}
		if alloc, aErr := s.Store.GetAllocationByDisk(ctx, selected.ID); aErr == nil && alloc != nil {
			if err := s.removeAllocation(ctx, alloc.ID); err != nil {
				return err
			}
		} else if aErr != nil && !isNotFound(aErr) {
			return aErr
		}
		if err := s.Store.DeleteDisk(ctx, selected.ID); err != nil && !isNotFound(err) {
			return err
		}
		s.resyncUsage(ctx, repo.ID, userID)
		s.Log.Info("已把差异盘合并进母盘", "repo_id", repo.ID,
			"diff_disk_id", selected.ID, "merged", merged)
	}
	rep.Progress(85)

	// ⑤ 合并改写了母盘内容：重算物理占用，自增版本并回到 idle（复用退出维护的统一收尾）。
	if size, sErr := s.Disk.PhysicalSize(parent.VHDXPath); sErr == nil {
		parent.PhysicalBytes = size
		if err := s.Store.UpdateDisk(ctx, parent); err != nil {
			return err
		}
	}
	if err := s.Repos.endParentShare(ctx, repo, true, ParentActionFinishMaint, p.OperatorID); err != nil {
		return err
	}
	rep.Progress(100)
	s.audit(ctx, p.OperatorID, "repo.parent.merge_diff.done", "repo:"+repo.ID,
		"diff_disk="+p.DiffDiskID, domain.AuditResultOK)
	return nil
}
