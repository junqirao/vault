package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
)

const (
	// clientDiffMaxFiles 限制客户端清单规模（清单走 JSON，100k 已足够大）。
	clientDiffMaxFiles = 100000
	// clientDiffMaxPathLen 限制单个相对路径长度，避免异常清单撑爆临时文件。
	clientDiffMaxPathLen = 4096
)

// ClientDiffFile 是客户端本地清单中的一项（路径用 "/" 分隔）。
type ClientDiffFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

// ClientDiffNeededFile 是客户端需要上传的一条文件。
type ClientDiffNeededFile struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"` // add | modify
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

// ClientDiffPlanResult 是"客户端目录 → 差异盘"第一阶段的计划结果。
//
// 服务器已经据此建好差异盘并执行了删除；客户端只需按 Needed 上传内容。
type ClientDiffPlanResult struct {
	RepoID     string `json:"repo_id"`
	DiffDiskID string `json:"diff_disk_id"`
	Mode       string `json:"mode"`
	Added      int    `json:"added"`
	Modified   int    `json:"modified"`
	Deleted    int    `json:"deleted"`
	// Needed 是待上传的新增/修改文件（按路径排序）。
	Needed []ClientDiffNeededFile `json:"needed"`
	// Deletes 是已经在服务器端删除的条目（供结果展示）。
	Deletes []DiffSyncEntry `json:"deletes"`
	// SourceFiles / SourceBytes 来自客户端清单。
	SourceFiles int      `json:"source_files"`
	SourceBytes int64    `json:"source_bytes"`
	Warnings    []string `json:"warnings"`
	Truncated   bool     `json:"truncated"`
}

// clientDiffPlanPayload 是 JobPlanClientDiff 的 payload。清单/计划都放临时文件，
// 避免把几十万条 JSON 塞进 jobs 表。
type clientDiffPlanPayload struct {
	RepoID       string `json:"repo_id"`
	DiskID       string `json:"disk_id"`
	ManifestPath string `json:"manifest_path"`
	PlanPath     string `json:"plan_path"`
	ResultPath   string `json:"result_path"`
	Mode         string `json:"mode"`
	DeleteExtra  bool   `json:"delete_extra"`
}

// StartClientDiffPlan 接收客户端清单，创建差异盘并计算"需要上传哪些文件"的计划（异步）。
//
// 关键语义：**diff 各自算各自的**——客户端扫描本地目录得到清单（路径/大小/哈希），
// 服务端扫描母盘/差异盘得到自己的清单，两边只交换清单（元数据），不传文件内容。
// 服务端完成删除与比对后返回 Needed，客户端只上传这些文件的内容。
func (s *DiskService) StartClientDiffPlan(ctx context.Context, repoID string, files []ClientDiffFile,
	mode string, deleteExtra bool, operatorID string) (*domain.Job, error) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if !repo.IsShared() {
		return nil, apperr.New("repo.not_shared", http.StatusConflict).WithArg("mode", string(repo.Mode))
	}
	if repo.State != domain.RepoStateActive {
		return nil, apperr.New("repo.state_invalid", http.StatusConflict).WithArg("state", string(repo.State))
	}
	if err := s.ensureNoActiveDiffOperation(ctx, repo.ID); err != nil {
		return nil, err
	}
	if repo.ParentDiskID == nil {
		return nil, apperr.New("repo.disk_missing", http.StatusInternalServerError)
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return nil, err
	}
	if !isDiskBuilt(parent.State) {
		return nil, apperr.DiskNotReady().WithArg("disk_id", parent.ID).WithArg("state", string(parent.State))
	}
	// 与从服务端目录创建同理：维护态专属；操作期间整个存储库锁定。
	if repo.Condition() != domain.ParentMaintenance {
		return nil, apperr.RepoParentConditionViolation(
			string(repo.Condition()), string(domain.ParentMaintenance))
	}
	if s.Repos != nil {
		s.Repos.UnmountParentVolume(ctx, repo.ID)
	}
	if len(files) == 0 {
		return nil, apperr.InvalidParam("files")
	}
	if len(files) > clientDiffMaxFiles {
		return nil, apperr.New("diffsync.too_many_files", http.StatusRequestEntityTooLarge).
			WithArg("limit", clientDiffMaxFiles)
	}

	disk, err := s.createSyncDiffDisk(ctx, repo, parent)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(os.TempDir(), diffSyncBaseDir, uuid.NewString())
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		_ = s.Store.DeleteDisk(ctx, disk.ID)
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "mkdir").WithCause(err)
	}
	cleanupOldDiffSyncRuns(24 * time.Hour)
	manifestPath := filepath.Join(runDir, "manifest.json")
	if err := writeClientDiffManifest(manifestPath, files); err != nil {
		_ = s.Store.DeleteDisk(ctx, disk.ID)
		return nil, err
	}
	payload := clientDiffPlanPayload{
		RepoID:       repo.ID,
		DiskID:       disk.ID,
		ManifestPath: manifestPath,
		PlanPath:     filepath.Join(runDir, "plan.json"),
		ResultPath:   filepath.Join(runDir, "result.json"),
		Mode:         string(normalizeDiffSyncMode(mode)),
		DeleteExtra:  deleteExtra,
	}
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobPlanClientDiff, disk.ID,
		"plan_client_diff:"+disk.ID, lock.RepoKey(repo.ID), payload)
	if err != nil {
		_ = s.Store.DeleteDisk(ctx, disk.ID)
		return nil, err
	}
	s.audit(ctx, operatorID, "repo.diff_from_client.plan", "repo:"+repo.ID,
		fmt.Sprintf("disk=%s files=%d mode=%s job=%s", disk.ID, len(files), payload.Mode, j.ID),
		domain.AuditResultOK)
	return j, nil
}

// GetClientDiffPlanResult 读取第一阶段的计划结果。
func (s *DiskService) GetClientDiffPlanResult(ctx context.Context, repoID, jobID string) (*ClientDiffPlanResult, error) {
	jobID = strings.TrimSpace(jobID)
	if _, err := uuid.Parse(jobID); err != nil {
		return nil, apperr.InvalidParam("job_id")
	}
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j.Type != domain.JobPlanClientDiff {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	var p clientDiffPlanPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	if p.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	if j.State != domain.JobStateSucceeded {
		if _, statErr := os.Stat(p.PlanPath); statErr != nil {
			return nil, apperr.New("diffsync.plan_not_ready", http.StatusConflict).
				WithArg("state", string(j.State))
		}
	}
	raw, err := os.ReadFile(p.PlanPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	var out ClientDiffPlanResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if out.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	return &out, nil
}

// runClientDiffPlan 是 JobPlanClientDiff 的实现：建差异盘 → 比对 → 删除多余项 → 写计划。
func (s *DiskService) runClientDiffPlan(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p clientDiffPlanPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if s.Disk == nil || s.Vol == nil {
		return errNotImplemented
	}
	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if repo.ParentDiskID == nil {
		return apperr.New("repo.disk_missing", http.StatusInternalServerError)
	}
	if repo.State != domain.RepoStateActive {
		return apperr.New("repo.state_invalid", http.StatusConflict).WithArg("state", string(repo.State))
	}
	if repo.Condition() != domain.ParentMaintenance {
		return apperr.RepoParentConditionViolation(
			string(repo.Condition()), string(domain.ParentMaintenance))
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return err
	}
	if disk.ParentVersion != repo.ParentVersion {
		return apperr.DiskParentVersionMismatch(disk.ParentVersion, repo.ParentVersion)
	}
	rep.Progress(5)

	files, err := readClientDiffManifest(p.ManifestPath)
	if err != nil {
		return err
	}
	srcMap := make(map[string]diffSyncFile, len(files))
	var sourceBytes int64
	for _, f := range files {
		path := filepath.ToSlash(f.Path)
		if path == "" || f.Size < 0 || strings.ContainsRune(path, 0) ||
			len(path) > clientDiffMaxPathLen || strings.HasPrefix(path, "/") {
			return apperr.InvalidParam("files")
		}
		modTime := time.Time{}
		if f.Mtime > 0 {
			modTime = time.UnixMilli(f.Mtime)
		}
		srcMap[path] = diffSyncFile{size: f.Size, modTime: modTime, hash: f.Hash}
		sourceBytes += f.Size
	}

	if !s.Disk.Exists(disk.VHDXPath) {
		if repo.Condition() == domain.ParentMaintenance {
			if s.Repos != nil {
				s.Repos.UnmountParentVolume(ctx, repo.ID)
			}
			if err := s.Vol.UnmountVolume(ctx, parent.VHDXPath, ""); err != nil {
				s.Log.Warn("客户端差异盘：卸载母盘失败（继续）", "repo_id", repo.ID, "error", err)
			}
		}
		if s.IscsiSvc != nil {
			s.IscsiSvc.TeardownDiskTargets(ctx, parent.ID)
		}
		if s.Iscsi != nil {
			if err := s.Iscsi.RemoveVirtualDisk(ctx, parent.VHDXPath); err != nil {
				s.Log.Warn("客户端差异盘：移除母盘 iSCSI 登记失败（继续）", "repo_id", repo.ID, "error", err)
			}
		}
		if err := s.createDiffWithRetry(ctx, disk.VHDXPath, parent.VHDXPath); err != nil {
			return err
		}
	}
	disk.ParentVersion = repo.ParentVersion
	rep.Progress(15)

	root, err := s.Vol.MountVolume(ctx, disk.VHDXPath, disk.FileSystemOrDefault().String())
	if err != nil {
		return err
	}
	unmounted := false
	defer func() {
		if unmounted {
			return
		}
		_ = s.Vol.UnmountVolume(context.WithoutCancel(ctx), disk.VHDXPath, root)
	}()

	mode := normalizeDiffSyncMode(p.Mode)
	dstFiles, _, warnings, err := scanDiffTree(ctx, root, mode, nil)
	if err != nil {
		return err
	}
	rep.Progress(55)
	plan, planWarnings := buildDiffSyncPlan(srcMap, dstFiles, mode, p.DeleteExtra)
	warnings = append(warnings, planWarnings...)

	// 只执行删除：新增/修改的内容还没有上传，等服务端收完文件再写。
	if len(plan.del) > 0 {
		if _, err := applyDiffSyncPlan(ctx, "", root, &diffSyncPlan{del: plan.del}, nil); err != nil {
			return err
		}
	}
	rep.Progress(85)

	if err := s.Vol.UnmountVolume(ctx, disk.VHDXPath, root); err != nil {
		return err
	}
	unmounted = true
	rep.Progress(95)

	needed := make([]ClientDiffNeededFile, 0, len(plan.add)+len(plan.modify))
	appendNeeded := func(kind string, entries []DiffSyncEntry) {
		for _, e := range entries {
			meta := srcMap[e.Path]
			mtime := int64(0)
			if !meta.modTime.IsZero() {
				mtime = meta.modTime.UnixMilli()
			}
			needed = append(needed, ClientDiffNeededFile{
				Path: e.Path, Kind: kind, Size: meta.size, Mtime: mtime, Hash: meta.hash,
			})
		}
	}
	appendNeeded("add", plan.add)
	appendNeeded("modify", plan.modify)
	sort.Slice(needed, func(i, j int) bool { return needed[i].Path < needed[j].Path })
	result := &ClientDiffPlanResult{
		RepoID:      repo.ID,
		DiffDiskID:  disk.ID,
		Mode:        string(mode),
		Added:       plan.added,
		Modified:    plan.modified,
		Deleted:     plan.deleted,
		Needed:      needed,
		Deletes:     plan.del,
		SourceFiles: len(srcMap),
		SourceBytes: sourceBytes,
		Warnings:    dedupeStrings(warnings),
	}
	if len(result.Deletes) > diffSyncMaxEntries {
		result.Deletes = result.Deletes[:diffSyncMaxEntries]
		result.Truncated = true
	}
	if err := writeJSONAtomic(p.PlanPath, result); err != nil {
		return err
	}
	rep.Progress(100)
	s.Log.Info("客户端目录差异计划完成", "repo_id", repo.ID, "disk_id", disk.ID,
		"added", result.Added, "modified", result.Modified, "deleted", result.Deleted)
	return nil
}

// CompleteClientDiff 接收客户端上传的"新增/修改文件内容"，写进差异盘并收尾。
//
// body 是 multipart/form-data：每个文件前先有一个 `path` 表单字段，随后是一个
// `file` 文件部件（见 agent 侧 UploadClientDiff）。
func (s *DiskService) CompleteClientDiff(ctx context.Context, repoID, jobID string, body io.Reader, boundary string) (*DiffSyncResult, error) {
	jobID = strings.TrimSpace(jobID)
	if _, err := uuid.Parse(jobID); err != nil {
		return nil, apperr.InvalidParam("job_id")
	}
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j.Type != domain.JobPlanClientDiff {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	var p clientDiffPlanPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	if p.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	if s.Disk == nil || s.Vol == nil {
		return nil, errNotImplemented
	}
	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		return nil, err
	}
	if disk.Kind != domain.DiskKindDiff || disk.RepoID != repoID {
		return nil, apperr.InvalidParam("diff_disk_id")
	}
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	// 计划生成后母盘可能已做过维护：再写进去的改动是基于旧基线的，不能标为 ready。
	if disk.ParentVersion != repo.ParentVersion {
		return nil, apperr.DiskParentVersionMismatch(disk.ParentVersion, repo.ParentVersion)
	}
	// 已完成过（客户端超时重试）：直接返回已写好的结果。
	if disk.State == domain.DiskStateReady || disk.State == domain.DiskStatePublished {
		if result, rErr := readDiffSyncResult(p.ResultPath); rErr == nil {
			return result, nil
		}
	}
	if disk.State != domain.DiskStateCreating {
		return nil, apperr.DiskNotReady().WithArg("disk_id", disk.ID).WithArg("state", string(disk.State))
	}
	raw, err := os.ReadFile(p.PlanPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	var plan ClientDiffPlanResult
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	needed := make(map[string]ClientDiffNeededFile, len(plan.Needed))
	for _, f := range plan.Needed {
		needed[f.Path] = f
	}

	// 与计划/合并/清理串行：上传期间不能有人把同一块盘删掉或合并走。
	if s.Locks != nil {
		unlock, lockErr := s.Locks.LockContext(ctx, lock.RepoKey(repoID))
		if lockErr != nil {
			return nil, lockErr
		}
		defer unlock()
	}

	root, err := s.Vol.MountVolume(ctx, disk.VHDXPath, disk.FileSystemOrDefault().String())
	if err != nil {
		return nil, err
	}
	unmounted := false
	defer func() {
		if unmounted {
			return
		}
		_ = s.Vol.UnmountVolume(context.WithoutCancel(ctx), disk.VHDXPath, root)
	}()

	mr := multipart.NewReader(body, boundary)
	currentPath := ""
	for {
		part, pErr := mr.NextPart()
		if pErr == io.EOF {
			break
		}
		if pErr != nil {
			return nil, apperr.InvalidParam("body").WithCause(pErr)
		}
		switch part.FormName() {
		case "path":
			rawPath, _ := io.ReadAll(io.LimitReader(part, clientDiffMaxPathLen+1))
			currentPath = filepath.ToSlash(string(rawPath))
		case "file":
			if currentPath == "" {
				_ = part.Close()
				return nil, apperr.InvalidParam("body")
			}
			expect, ok := needed[currentPath]
			if !ok {
				_ = part.Close()
				return nil, apperr.InvalidParam("path").WithArg("path", currentPath)
			}
			if err := writeClientDiffFile(root, expect, part); err != nil {
				_ = part.Close()
				return nil, err
			}
			delete(needed, currentPath)
			currentPath = ""
		}
		_ = part.Close()
	}
	if len(needed) > 0 {
		return nil, apperr.New("diffsync.missing_files", http.StatusConflict).
			WithArg("count", len(needed))
	}

	finalFiles, finalBytes, finalWarnings, err := scanDiffTree(ctx, root, DiffSyncModeSize, nil)
	if err != nil {
		return nil, err
	}
	if err := s.Vol.UnmountVolume(ctx, disk.VHDXPath, root); err != nil {
		return nil, err
	}
	unmounted = true

	if size, pErr := s.Disk.PhysicalSize(disk.VHDXPath); pErr == nil {
		disk.PhysicalBytes = size
	}
	disk.State = domain.DiskStateReady
	disk.Mounted = false
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return nil, err
	}
	if repo.Meta.Pool {
		s.prePublishTarget(ctx, disk)
	}

	entries := make([]DiffSyncEntry, 0, len(plan.Needed)+len(plan.Deletes))
	for _, f := range plan.Needed {
		entries = append(entries, DiffSyncEntry{Path: f.Path, Kind: f.Kind, Size: f.Size})
	}
	entries = append(entries, plan.Deletes...)
	truncated := plan.Truncated
	if len(entries) > diffSyncMaxEntries {
		entries = entries[:diffSyncMaxEntries]
		truncated = true
	}
	result := &DiffSyncResult{
		RepoID:      repoID,
		DiffDiskID:  disk.ID,
		SourceDir:   "", // 客户端本地目录不在服务端暴露
		Mode:        plan.Mode,
		Added:       plan.Added,
		Modified:    plan.Modified,
		Deleted:     plan.Deleted,
		SourceFiles: plan.SourceFiles,
		SourceBytes: plan.SourceBytes,
		DiffFiles:   len(finalFiles),
		DiffBytes:   finalBytes,
		Entries:     entries,
		Warnings:    dedupeStrings(append(plan.Warnings, finalWarnings...)),
		Truncated:   truncated,
	}
	if err := writeJSONAtomic(p.ResultPath, result); err != nil {
		return nil, err
	}
	s.audit(ctx, "", "repo.diff_from_client.done", "repo:"+repoID,
		fmt.Sprintf("disk=%s added=%d modified=%d deleted=%d", disk.ID, result.Added, result.Modified, result.Deleted),
		domain.AuditResultOK)
	return result, nil
}

// GetClientDiffResult 读取客户端目录差异盘的最终结果（上传已完成但响应丢失时用于自愈）。
func (s *DiskService) GetClientDiffResult(ctx context.Context, repoID, jobID string) (*DiffSyncResult, error) {
	jobID = strings.TrimSpace(jobID)
	if _, err := uuid.Parse(jobID); err != nil {
		return nil, apperr.InvalidParam("job_id")
	}
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j.Type != domain.JobPlanClientDiff {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	var p clientDiffPlanPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	if p.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	result, rErr := readDiffSyncResult(p.ResultPath)
	if rErr != nil {
		return nil, apperr.New("diffsync.result_not_ready", http.StatusConflict)
	}
	return result, nil
}

// AbortClientDiff 删除一条未完成的客户端差异盘（客户端取消时调用，尽力而为）。
func (s *DiskService) AbortClientDiff(ctx context.Context, repoID, jobID string) error {
	jobID = strings.TrimSpace(jobID)
	if _, err := uuid.Parse(jobID); err != nil {
		return apperr.InvalidParam("job_id")
	}
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j.Type != domain.JobPlanClientDiff {
		return apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	var p clientDiffPlanPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if p.RepoID != repoID {
		return apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	// 上传可能已经在服务端成功、只是响应丢在网络上：此时不能把已完成的盘删掉。
	disk, dErr := s.Store.GetDisk(ctx, p.DiskID)
	if dErr != nil {
		if isNotFound(dErr) {
			return nil
		}
		return dErr
	}
	if disk.State == domain.DiskStateReady || disk.State == domain.DiskStatePublished {
		return nil
	}
	if s.Locks != nil {
		unlock, lockErr := s.Locks.LockContext(ctx, lock.RepoKey(repoID))
		if lockErr != nil {
			return lockErr
		}
		defer unlock()
	}
	return s.deleteDiskArtifacts(ctx, p.DiskID, "")
}

func writeClientDiffManifest(path string, files []ClientDiffFile) error {
	return writeJSONAtomic(path, map[string]any{"files": files})
}

func readClientDiffManifest(path string) ([]ClientDiffFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	var m struct {
		Files []ClientDiffFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, apperr.InvalidParam("manifest").WithCause(err)
	}
	return m.Files, nil
}

func readDiffSyncResult(path string) (*DiffSyncResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out DiffSyncResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// writeClientDiffFile 把 multipart 文件部件原子写入差异盘，并校验大小/保留 mtime。
func writeClientDiffFile(root string, f ClientDiffNeededFile, r io.Reader) error {
	abs, err := diffSyncAbs(root, f.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "mkdir").WithCause(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".vault-diffsync-*")
	if err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// 只允许读到声明大小 + 1 字节：多出来的部分说明客户端清单与实际内容不一致，
	// 不能无限往盘里写。
	limit := f.Size + 1
	if limit < 1 {
		limit = 1
	}
	n, err := io.Copy(tmp, io.LimitReader(r, limit))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "write").WithArg("path", f.Path).WithCause(err)
	}
	if f.Size >= 0 && n != f.Size {
		return apperr.New("diffsync.size_mismatch", http.StatusConflict).
			WithArg("path", f.Path).WithArg("expected", f.Size).WithArg("actual", n)
	}
	_ = os.Chmod(abs, 0o666)
	_ = os.Remove(abs)
	if err := os.Rename(tmpName, abs); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "rename").WithArg("path", f.Path).WithCause(err)
	}
	if f.Mtime > 0 {
		t := time.UnixMilli(f.Mtime)
		_ = os.Chtimes(abs, t, t)
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	_ = os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	return nil
}
