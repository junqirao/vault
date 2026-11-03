package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
	// diffSyncMaxEntries 限制结果里返回的差异条目数（清单可能很大，响应不能无限膨胀）。
	diffSyncMaxEntries = 5000
	// diffSyncBaseDir 是差异同步结果文件的根目录（系统临时目录下）。
	diffSyncBaseDir = "vault-diffsync"
	// diffSyncNeedBytes 创建差异盘时要求的最小空闲空间：差异盘初始只占元数据，
	// 不能按母盘标称容量（可能 1TB）去要求空闲空间。
	diffSyncNeedBytes int64 = 1 << 30
	// diffSyncMtimeTolerance 是"自动"模式下判断修改的时间容差。
	// NTFS 精确到 100ns，FAT 只有 2 秒；1 秒足够覆盖跨文件系统搬运的误差。
	diffSyncMtimeTolerance = time.Second
)

// DiffSyncMode 是"从目录创建差异盘"的比较模式。
//
//   - auto：大小 + 修改时间（默认，快；内容变了但大小与时间都没变的情况会漏掉）
//   - hash：大小 + SHA-256（准确，但要完整读两边的文件）
//   - size：只比大小（最快，适合"目录只是增量文件"的场景）
type DiffSyncMode string

const (
	DiffSyncModeAuto DiffSyncMode = "auto"
	DiffSyncModeHash DiffSyncMode = "hash"
	DiffSyncModeSize DiffSyncMode = "size"
)

// DiffSyncEntry 是一条将被执行到差异盘上的改动。
type DiffSyncEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"` // add | modify | delete
	Size   int64  `json:"size,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// DiffSyncResult 是"从目录创建差异盘"的完整结果。
type DiffSyncResult struct {
	RepoID     string `json:"repo_id"`
	DiffDiskID string `json:"diff_disk_id"`
	SourceDir  string `json:"source_dir"`
	Mode       string `json:"mode"`
	// Added / Modified / Deleted 是实际执行的条数（与 Entries 长度一致，Entries 可能被截断）。
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Deleted  int `json:"deleted"`
	// SourceFiles / SourceBytes 是源目录的统计；DiffFiles / DiffBytes 是应用后盘内的统计。
	SourceFiles int   `json:"source_files"`
	SourceBytes int64 `json:"source_bytes"`
	DiffFiles   int   `json:"diff_files"`
	DiffBytes   int64 `json:"diff_bytes"`
	// Entries 是改动清单（最多 diffSyncMaxEntries 条，超出时 Truncated=true）。
	Entries   []DiffSyncEntry `json:"entries"`
	Warnings  []string        `json:"warnings"`
	Truncated bool            `json:"truncated"`
}

// diffSyncPayload 是 JobCreateDiffFromDir 的 payload。
type diffSyncPayload struct {
	RepoID      string `json:"repo_id"`
	DiskID      string `json:"disk_id"`
	SourceDir   string `json:"source_dir"`
	Mode        string `json:"mode"`
	DeleteExtra bool   `json:"delete_extra"`
	ResultPath  string `json:"result_path"`
}

// diffSyncFile 是一次扫描得到的文件元数据。
type diffSyncFile struct {
	size    int64
	modTime time.Time
	hash    string
}

// StartDiffFromDir 从服务端目录创建一块差异盘（异步任务）。
//
// 为什么不是普通"派生差异盘"：用户手里已经有一棵目录树，其中绝大多数文件与母盘相同。
// 直接整棵拷进去既慢又占空间；这里先比较出新增/修改/删除清单，再只把这些改动写进
// 差异盘——差异盘天然只保存相对母盘的增量，最终内容与"全量拷入"完全一致。
//
// 正常态（idle）按普通派生规则校验 max_diff_disks；维护态是受控的管理员操作：
// 通常紧接着就合并回母盘，因此不受该上限约束（临时共享态仍拒绝）。
func (s *DiskService) StartDiffFromDir(ctx context.Context, repoID, sourceDir, mode string, deleteExtra bool, operatorID string) (*domain.Job, error) {
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

	src, err := s.validateSourceDir(ctx, sourceDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(src)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, apperr.InvalidParam("source_dir")
	}

	// 差异盘工具是**维护态专属操作**：期间整个存储库已禁止分配与挂载，工具内的建盘/合并
	// 会长时间占用母盘，必须锁在维护态里执行（临时共享/正常态一律拒绝）。
	if repo.Condition() != domain.ParentMaintenance {
		return nil, apperr.RepoParentConditionViolation(
			string(repo.Condition()), string(domain.ParentMaintenance))
	}
	// 创建前先卸掉母盘的服务端本地挂载：差异盘要独占打开母盘，本地挂载会撞共享冲突。
	if s.Repos != nil {
		s.Repos.UnmountParentVolume(ctx, repo.ID)
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
	// 顺手清掉一天前的旧结果，避免临时目录无限堆积（与上传核对同一策略）。
	cleanupOldDiffSyncRuns(24 * time.Hour)

	payload := diffSyncPayload{
		RepoID:      repo.ID,
		DiskID:      disk.ID,
		SourceDir:   src,
		Mode:        string(normalizeDiffSyncMode(mode)),
		DeleteExtra: deleteExtra,
		ResultPath:  filepath.Join(runDir, "result.json"),
	}
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobCreateDiffFromDir, disk.ID,
		"create_diff_from_dir:"+disk.ID, lock.RepoKey(repo.ID), payload)
	if err != nil {
		_ = s.Store.DeleteDisk(ctx, disk.ID)
		return nil, err
	}
	s.audit(ctx, operatorID, "repo.diff_from_dir.start", "repo:"+repo.ID,
		"disk="+disk.ID+" source="+src+" mode="+payload.Mode+" job="+j.ID, domain.AuditResultOK)
	return j, nil
}

// createSyncDiffDisk 为"从目录创建差异盘"登记一块待建差异盘（文件由任务异步创建）。
//
// 两条路径共用：服务端目录（StartDiffFromDir）与客户端目录（StartClientDiffPlan）。
func (s *DiskService) createSyncDiffDisk(ctx context.Context, repo *domain.Repository, parent *domain.Disk) (*domain.Disk, error) {
	if s.Disk == nil || s.Repos == nil {
		return nil, errNotImplemented
	}
	guard, err := s.Repos.pickGuardForPath(ctx, parent.VHDXPath, diffSyncNeedBytes)
	if err != nil {
		return nil, err
	}
	diskID := uuid.NewString()
	rel := filepath.Join(s.raw().Storage.DisksDir, "diffs", repo.ID, diskID+".vhdx")
	ref, err := s.Disk.DiskRef(guard.Root, rel)
	if err != nil {
		return nil, apperr.InvalidParam("path")
	}
	disk := &domain.Disk{
		ID:            diskID,
		RepoID:        repo.ID,
		Kind:          domain.DiskKindDiff,
		VHDXPath:      ref,
		ParentID:      &parent.ID,
		ParentVersion: repo.ParentVersion,
		SizeBytes:     parent.SizeBytes,
		VHDType:       domain.VHDTypeDifferencing,
		State:         domain.DiskStateCreating,
		FileSystem:    parent.FileSystemOrDefault(),
	}
	if err := s.Store.CreateDisk(ctx, disk); err != nil {
		return nil, err
	}
	return disk, nil
}

// GetDiffSyncResult 读取一次"从目录创建差异盘"的结果。
func (s *DiskService) GetDiffSyncResult(ctx context.Context, repoID, jobID string) (*DiffSyncResult, error) {
	jobID = strings.TrimSpace(jobID)
	if _, err := uuid.Parse(jobID); err != nil {
		return nil, apperr.InvalidParam("job_id")
	}
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j.Type != domain.JobCreateDiffFromDir {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	var p diffSyncPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	if p.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	if j.State != domain.JobStateSucceeded {
		if _, statErr := os.Stat(p.ResultPath); statErr != nil {
			return nil, apperr.New("diffsync.result_not_ready", http.StatusConflict).
				WithArg("state", string(j.State))
		}
	}
	raw, err := os.ReadFile(p.ResultPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
		}
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	var out DiffSyncResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if out.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	return &out, nil
}

// runCreateDiffFromDir 是 JobCreateDiffFromDir 的实现。
func (s *DiskService) runCreateDiffFromDir(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p diffSyncPayload
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
	// 排队期间必须仍在维护态：中途退出维护/进入临时共享会把母盘用途改掉，
	// 继续建盘会与其它维护动作互相踩踏。
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
	// 排队期间母盘可能已做过维护（parent_version +1）：这块盘的内容不再是当前基线的
	// 增量，继续写会把"基于旧母盘的改动"错误地标成新版本。
	if disk.ParentVersion != repo.ParentVersion {
		return apperr.DiskParentVersionMismatch(disk.ParentVersion, repo.ParentVersion)
	}

	// 已建好、文件存在且结果也已写出：任务重试时直接成功，不能重复派生/覆盖清单。
	// 若结果文件缺失（上次在写结果前失败），则继续走一遍：盘已经包含改动，重新比较
	// 只会得到空清单，至少能让结果 API 返回可用数据。
	if isDiskBuilt(disk.State) && s.Disk.Exists(disk.VHDXPath) {
		if _, statErr := os.Stat(p.ResultPath); statErr == nil {
			rep.Progress(100)
			return nil
		}
	}
	rep.Progress(5)
	if !s.Disk.Exists(disk.VHDXPath) {
		// 维护态下母盘可能正被「母盘文件」页本地挂载/被 iSCSI 目标打开：
		// 先卸挂载、拆目标，否则创建差异盘会撞共享冲突。
		if repo.Condition() == domain.ParentMaintenance {
			if s.Repos != nil {
				s.Repos.UnmountParentVolume(ctx, repo.ID)
			}
			if s.Vol != nil {
				if err := s.Vol.UnmountVolume(ctx, parent.VHDXPath, ""); err != nil {
					s.Log.Warn("从目录创建差异盘前卸载母盘失败（继续尝试）", "repo_id", repo.ID, "error", err)
				}
			}
		}
		if s.IscsiSvc != nil {
			s.IscsiSvc.TeardownDiskTargets(ctx, parent.ID)
		}
		if s.Iscsi != nil {
			if err := s.Iscsi.RemoveVirtualDisk(ctx, parent.VHDXPath); err != nil {
				s.Log.Warn("从目录创建差异盘前移除母盘 iSCSI 登记失败（继续尝试）",
					"repo_id", repo.ID, "error", err)
			}
		}
		if err := s.createDiffWithRetry(ctx, disk.VHDXPath, parent.VHDXPath); err != nil {
			return err
		}
	}
	disk.ParentVersion = repo.ParentVersion
	rep.Progress(15)

	fsName := disk.FileSystemOrDefault().String()
	root, err := s.Vol.MountVolume(ctx, disk.VHDXPath, fsName)
	if err != nil {
		return err
	}
	unmounted := false
	defer func() {
		if unmounted {
			return
		}
		// 任务失败/取消也要把盘卸掉，避免留下脏挂载挡住后续重试。
		_ = s.Vol.UnmountVolume(context.WithoutCancel(ctx), disk.VHDXPath, root)
	}()

	mode := normalizeDiffSyncMode(p.Mode)
	srcFiles, srcBytes, srcWarnings, err := scanDiffTree(ctx, p.SourceDir, mode, nil)
	if err != nil {
		return err
	}
	rep.Progress(35)
	dstFiles, _, dstWarnings, err := scanDiffTree(ctx, root, mode, nil)
	if err != nil {
		return err
	}
	rep.Progress(55)

	plan, planWarnings := buildDiffSyncPlan(srcFiles, dstFiles, mode, p.DeleteExtra)
	warnings := append(append(srcWarnings, dstWarnings...), planWarnings...)
	rep.Progress(60)

	lastProgress := -1
	_, err = applyDiffSyncPlan(ctx, p.SourceDir, root, plan, func(done, total int) {
		if total <= 0 {
			return
		}
		next := 60 + 30*done/total
		if next == lastProgress {
			return
		}
		lastProgress = next
		rep.Progress(next)
	})
	if err != nil {
		return err
	}
	rep.Progress(92)

	// 应用后再扫一遍：把"实际写进盘里的内容"作为结果统计，顺便发现静默失败。
	finalFiles, finalBytes, finalWarnings, err := scanDiffTree(ctx, root, DiffSyncModeSize, nil)
	if err != nil {
		return err
	}
	warnings = append(warnings, finalWarnings...)

	if err := s.Vol.UnmountVolume(ctx, disk.VHDXPath, root); err != nil {
		return err
	}
	unmounted = true
	rep.Progress(96)

	if size, pErr := s.Disk.PhysicalSize(disk.VHDXPath); pErr == nil {
		disk.PhysicalBytes = size
	}
	disk.State = domain.DiskStateReady
	disk.Mounted = false
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	// 池化库的新差异盘通常直接交给用户：先把 iSCSI 目标建好，分配后即可挂载。
	// 非池化库交给挂载路径现场下发（与 runCreateDiff 同一策略）。
	if repo.Meta.Pool {
		s.prePublishTarget(ctx, disk)
	}

	result := &DiffSyncResult{
		RepoID:      repo.ID,
		DiffDiskID:  disk.ID,
		SourceDir:   p.SourceDir,
		Mode:        string(mode),
		Added:       plan.added,
		Modified:    plan.modified,
		Deleted:     plan.deleted,
		SourceFiles: len(srcFiles),
		SourceBytes: srcBytes,
		DiffFiles:   len(finalFiles),
		DiffBytes:   finalBytes,
		Warnings:    dedupeStrings(warnings),
	}
	result.Entries, result.Truncated = plan.entries(diffSyncMaxEntries)
	if err := writeDiffSyncResult(p.ResultPath, result); err != nil {
		return err
	}
	rep.Progress(100)
	s.audit(ctx, "", "repo.diff_from_dir.done", "repo:"+repo.ID,
		fmt.Sprintf("disk=%s added=%d modified=%d deleted=%d", disk.ID, result.Added, result.Modified, result.Deleted),
		domain.AuditResultOK)
	s.Log.Info("已从目录创建差异盘", "repo_id", repo.ID, "disk_id", disk.ID,
		"source", p.SourceDir, "added", result.Added, "modified", result.Modified, "deleted", result.Deleted)
	return nil
}

// diffSyncPlan 是待执行的改动集合。
type diffSyncPlan struct {
	add      []DiffSyncEntry
	modify   []DiffSyncEntry
	del      []DiffSyncEntry
	added    int
	modified int
	deleted  int
}

func (p *diffSyncPlan) entries(limit int) ([]DiffSyncEntry, bool) {
	total := len(p.add) + len(p.modify) + len(p.del)
	out := make([]DiffSyncEntry, 0, minInt(total, limit))
	for _, group := range [][]DiffSyncEntry{p.add, p.modify, p.del} {
		for _, e := range group {
			if len(out) >= limit {
				return out, true
			}
			out = append(out, e)
		}
	}
	return out, false
}

// buildDiffSyncPlan 对比源目录与盘内内容，生成新增/修改/删除清单。
func buildDiffSyncPlan(src, dst map[string]diffSyncFile, mode DiffSyncMode, deleteExtra bool) (*diffSyncPlan, []string) {
	plan := &diffSyncPlan{}
	for path, s := range src {
		d, ok := dst[path]
		if !ok {
			plan.add = append(plan.add, DiffSyncEntry{Path: path, Kind: "add", Size: s.size, Reason: "missing"})
			continue
		}
		reason := ""
		switch {
		case s.size != d.size:
			reason = "size"
		case mode == DiffSyncModeHash && s.hash != "" && d.hash != "" && !strings.EqualFold(s.hash, d.hash):
			reason = "hash"
		case mode == DiffSyncModeAuto && !s.modTime.IsZero() && !d.modTime.IsZero() &&
			absDuration(s.modTime.Sub(d.modTime)) > diffSyncMtimeTolerance:
			reason = "mtime"
		}
		if reason != "" {
			plan.modify = append(plan.modify, DiffSyncEntry{Path: path, Kind: "modify", Size: s.size, Reason: reason})
		}
	}
	if deleteExtra {
		for path, d := range dst {
			if _, ok := src[path]; ok {
				continue
			}
			plan.del = append(plan.del, DiffSyncEntry{Path: path, Kind: "delete", Size: d.size, Reason: "extra"})
		}
	}
	sortEntries := func(list []DiffSyncEntry) {
		sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	}
	sortEntries(plan.add)
	sortEntries(plan.modify)
	sortEntries(plan.del)
	plan.added = len(plan.add)
	plan.modified = len(plan.modify)
	plan.deleted = len(plan.del)
	return plan, nil
}

// scanDiffTree 扫描一棵目录树，返回 相对路径（"/" 分隔）→ 元数据。
//
// onFile 用于细粒度进度（已处理文件数、当前路径）；可为 nil。
func scanDiffTree(ctx context.Context, root string, mode DiffSyncMode, onFile func(done int, current string)) (
	map[string]diffSyncFile, int64, []string, error) {
	files := make(map[string]diffSyncFile)
	warnings := make([]string, 0)
	var totalBytes int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			if p != root {
				warnings = append(warnings, "unreadable:"+diffSyncRel(root, p))
				return nil
			}
			return walkErr
		}
		rel := diffSyncRel(root, p)
		if rel == "." {
			return nil
		}
		if isVerifyRootMetadata(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, "symlink_skipped:"+rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			warnings = append(warnings, "stat_skipped:"+rel)
			return nil
		}
		meta := diffSyncFile{size: info.Size(), modTime: info.ModTime()}
		if mode == DiffSyncModeHash {
			hash, hErr := hashFileSHA256(p)
			if hErr != nil {
				warnings = append(warnings, "hash_failed:"+rel)
			} else {
				meta.hash = hash
			}
		}
		files[rel] = meta
		totalBytes += info.Size()
		if onFile != nil {
			onFile(len(files), rel)
		}
		return nil
	})
	return files, totalBytes, warnings, err
}

// applyDiffSyncPlan 把改动写进已挂载的差异盘根目录。
func applyDiffSyncPlan(ctx context.Context, sourceDir, root string, plan *diffSyncPlan, onProgress func(done, total int)) (int, error) {
	// 上次尝试如果在中途崩溃，目标里可能留下 .vault-diffsync-* 临时文件：先清掉，
	// 避免它们被当成盘内内容（尤其是 delete_extra=false 时）长期留给用户。
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".vault-diffsync-") {
			_ = os.Remove(p)
		}
		return nil
	})
	total := len(plan.add) + len(plan.modify) + len(plan.del)
	done := 0
	report := func() {
		done++
		if onProgress != nil {
			onProgress(done, total)
		}
	}

	// 先删除：path 从文件变成目录、或反过来时，先腾位置再写。
	for _, e := range plan.del {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		abs, err := diffSyncAbs(root, e.Path)
		if err != nil {
			return done, err
		}
		// Windows 上只读文件直接 os.Remove 会失败：先清掉只读位再删。
		_ = os.Chmod(abs, 0o666)
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return done, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("reason", "delete").WithArg("path", e.Path).WithCause(err)
		}
		pruneEmptyDiffDirs(root, filepath.Dir(abs))
		report()
	}

	for _, e := range append(append([]DiffSyncEntry{}, plan.add...), plan.modify...) {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		src, err := diffSyncAbs(sourceDir, e.Path)
		if err != nil {
			return done, err
		}
		dst, err := diffSyncAbs(root, e.Path)
		if err != nil {
			return done, err
		}
		if err := copyDiffFile(src, dst); err != nil {
			return done, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("reason", "copy").WithArg("path", e.Path).WithCause(err)
		}
		report()
	}
	return done, nil
}

// copyDiffFile 复制单个文件并保留修改时间（auto 模式下一轮比较依赖它）。
func copyDiffFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".vault-diffsync-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows 的 os.Rename 不允许覆盖已存在的文件；先清只读位再删目标。
	_ = os.Chmod(dst, 0o666)
	_ = os.Remove(dst)
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	// 时间戳放在改名之后：部分平台 rename 会重置 mtime。
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// pruneEmptyDiffDirs 删除空目录，直到 root 为止（避免把盘根删掉）。
func pruneEmptyDiffDirs(root, dir string) {
	root = filepath.Clean(root)
	for {
		dir = filepath.Clean(dir)
		if dir == root || !domain.UnderRoot(root, dir) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// diffSyncAbs 把相对路径安全地拼到根下（拒绝 `..`、绝对路径与 NUL）。
func diffSyncAbs(root, rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return "", apperr.InvalidParam("path")
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", apperr.InvalidParam("path")
		}
	}
	abs := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if !domain.UnderRoot(filepath.Clean(root), filepath.Clean(abs)) {
		return "", apperr.InvalidParam("path")
	}
	return abs, nil
}

func diffSyncRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

func normalizeDiffSyncMode(mode string) DiffSyncMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case string(DiffSyncModeHash):
		return DiffSyncModeHash
	case string(DiffSyncModeSize):
		return DiffSyncModeSize
	default:
		return DiffSyncModeAuto
	}
}

// hashFileSHA256 计算文件 SHA-256（不分块上报进度，调用方只在需要精确比较时用）。
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeDiffSyncResult(path string, result *DiffSyncResult) error {
	raw, err := json.MarshalIndent(result, "", "  ")
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
	// Windows 的 os.Rename 不覆盖已存在的目标：任务重试时结果文件可能还在。
	_ = os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	return nil
}

func cleanupOldDiffSyncRuns(maxAge time.Duration) {
	root := filepath.Join(os.TempDir(), diffSyncBaseDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
