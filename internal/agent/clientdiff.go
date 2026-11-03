package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"vault/internal/apperr"
)

const (
	diffSyncStateRunning   = "running"
	diffSyncStateSucceeded = "succeeded"
	diffSyncStateFailed    = "failed"
	diffSyncStateCancelled = "cancelled"

	diffSyncPhaseScan   = "scan"
	diffSyncPhaseHash   = "hash"
	diffSyncPhasePlan   = "plan"
	diffSyncPhaseUpload = "upload"
	diffSyncPhaseDone   = "done"

	diffSyncTaskTTL = 30 * time.Minute
	// diffSyncPollInterval 服务端 plan 任务的轮询间隔（任务本身是分钟级）。
	diffSyncPollInterval = 800 * time.Millisecond
	// diffSyncUploadBuffer 单次读取本机文件的缓冲。
	diffSyncUploadBuffer = 4 << 20
)

// diffSyncRequest 是 POST /agent/diffs/from-dir 的请求体。
type diffSyncRequest struct {
	RepoID      string `json:"repo_id"`
	LocalDir    string `json:"local_dir"`
	Mode        string `json:"mode"`
	DeleteExtra bool   `json:"delete_extra"`
	ServerKey   string `json:"server_key,omitempty"`
	ServerURL   string `json:"server_url,omitempty"`
}

// diffSyncStatus 是客户端目录差异任务的进度/结果快照。
type diffSyncStatus struct {
	DiffSyncID  string `json:"diff_sync_id"`
	RepoID      string `json:"repo_id"`
	LocalDir    string `json:"local_dir,omitempty"`
	Mode        string `json:"mode,omitempty"`
	DeleteExtra bool   `json:"delete_extra,omitempty"`
	State       string `json:"state"`
	Phase       string `json:"phase"`
	StartedAt   int64  `json:"started_at,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
	// Done/Total 当前阶段的文件数；扫描阶段 Total 为 0。
	Done  int `json:"done"`
	Total int `json:"total"`
	// Bytes/TotalBytes 当前阶段已处理字节。
	Bytes      int64           `json:"bytes"`
	TotalBytes int64           `json:"total_bytes"`
	Current    string          `json:"current,omitempty"`
	DiffDiskID string          `json:"diff_disk_id,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	Result     *diffSyncResult `json:"result,omitempty"`
	ErrorCode  string          `json:"error_code,omitempty"`
	ErrorMsg   string          `json:"error_message,omitempty"`
}

type diffSyncTask struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	snap       diffSyncStatus
	finishedAt time.Time
	repoID     string
	jobID      string
}

func (t *diffSyncTask) update(fn func(*diffSyncStatus)) {
	t.mu.Lock()
	fn(&t.snap)
	t.mu.Unlock()
}

func (t *diffSyncTask) status() diffSyncStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := t.snap
	// 切片要复制：后台 goroutine 可能正在 append，直接返回会与 JSON 编码并发读写同一底层数组。
	snap.Warnings = append([]string{}, t.snap.Warnings...)
	return snap
}

func (t *diffSyncTask) setJob(repoID, jobID string) {
	t.mu.Lock()
	t.repoID, t.jobID = repoID, jobID
	t.mu.Unlock()
}

func (t *diffSyncTask) taskRefs() (repoID, jobID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.repoID, t.jobID
}

func (t *diffSyncTask) finish(state string) {
	t.mu.Lock()
	t.snap.State = state
	t.snap.Phase = diffSyncPhaseDone
	t.snap.Current = ""
	t.snap.FinishedAt = time.Now().UnixMilli()
	t.finishedAt = time.Now()
	t.mu.Unlock()
}

type diffSyncManager struct {
	mu    sync.Mutex
	tasks map[string]*diffSyncTask
}

func newDiffSyncManager() *diffSyncManager {
	return &diffSyncManager{tasks: make(map[string]*diffSyncTask)}
}

func (m *diffSyncManager) create(ctx context.Context) *diffSyncTask {
	t := &diffSyncTask{id: uuid.NewString(), ctx: ctx}
	t.snap = diffSyncStatus{DiffSyncID: t.id, State: diffSyncStateRunning, Phase: diffSyncPhaseScan}
	m.mu.Lock()
	m.gcLocked()
	m.tasks[t.id] = t
	m.mu.Unlock()
	return t
}

// list 返回某个存储库的差异任务（开始时间倒序；repoID 为空返回全部）。
func (m *diffSyncManager) list(repoID string) []diffSyncStatus {
	if m == nil {
		return []diffSyncStatus{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	out := make([]diffSyncStatus, 0, len(m.tasks))
	for _, t := range m.tasks {
		t.mu.Lock()
		st := t.snap
		st.Warnings = append([]string{}, t.snap.Warnings...)
		// 列表只给摘要：完整 result 可能包含几千条清单，按需用 GET /agent/diffs/{id} 取。
		st.Result = nil
		t.mu.Unlock()
		if repoID != "" && st.RepoID != repoID {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

func (m *diffSyncManager) get(id string) *diffSyncTask {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// diffSyncMgr 惰性初始化（测试里手工构造的 Agent 可能没有它）。
func (a *Agent) diffSyncMgr() *diffSyncManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.diffsync == nil {
		a.diffsync = newDiffSyncManager()
	}
	return a.diffsync
}

func (m *diffSyncManager) gcLocked() {
	cutoff := time.Now().Add(-diffSyncTaskTTL)
	for id, t := range m.tasks {
		t.mu.Lock()
		finished := !t.finishedAt.IsZero() && t.finishedAt.Before(cutoff)
		t.mu.Unlock()
		if finished {
			delete(m.tasks, id)
		}
	}
}

// ---- 本地接口 ----

func (a *Agent) handleStartDiffSync(w http.ResponseWriter, r *http.Request) {
	var req diffSyncRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	req.RepoID = strings.TrimSpace(req.RepoID)
	if req.RepoID == "" {
		a.writeError(w, apperr.InvalidParam("repo_id"))
		return
	}
	dir, err := validateAbsDir(req.LocalDir, "local_dir")
	if err != nil {
		a.writeError(w, err)
		return
	}
	serverKey := a.store.ResolveKey(req.ServerKey, req.ServerURL)
	if serverKey == "" {
		a.writeError(w, errServerUnknown())
		return
	}
	client, err := a.serverClientFor(serverKey)
	if err != nil {
		a.writeError(w, err)
		return
	}
	ctx, cancel := context.WithCancel(a.bgContext())
	task := a.diffSyncMgr().create(ctx)
	task.cancel = cancel
	task.update(func(s *diffSyncStatus) {
		s.RepoID = req.RepoID
		s.LocalDir = dir
		s.Mode = normalizeDiffSyncMode(req.Mode)
		s.DeleteExtra = req.DeleteExtra
		s.StartedAt = time.Now().UnixMilli()
	})
	req.ServerKey = serverKey
	a.logger.Info("开始客户端目录差异盘", "diff_sync_id", task.id,
		"repo_id", req.RepoID, "local_dir", dir, "mode", req.Mode)
	safeGo(a.logger, "diff_sync_run", func() { a.runDiffSync(task, client, req, dir) })
	a.writeJSON(w, http.StatusAccepted, map[string]any{
		"diff_sync_id": task.id,
		"status":       task.status(),
	})
}

func (a *Agent) handleListDiffSync(w http.ResponseWriter, r *http.Request) {
	repoID := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	a.writeJSON(w, http.StatusOK, map[string]any{"items": a.diffSyncMgr().list(repoID)})
}

func (a *Agent) handleGetDiffSync(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	t := a.diffSyncMgr().get(id)
	if t == nil {
		a.writeError(w, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	a.writeJSON(w, http.StatusOK, t.status())
}

func (a *Agent) handleCancelDiffSync(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	t := a.diffSyncMgr().get(id)
	if t == nil {
		a.writeError(w, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	if t.cancel != nil {
		t.cancel()
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runDiffSync 执行：扫描 → 可选哈希 → 服务端清单比对 → 上传新增/修改文件。
func (a *Agent) runDiffSync(task *diffSyncTask, client *serverClient, req diffSyncRequest, dir string) {
	defer task.cancel()
	defer func() {
		// 用户取消/中途失败：服务端可能留下 creating 的差异盘，尽力删掉。
		state := task.status().State
		if state == diffSyncStateSucceeded {
			return
		}
		repoID, jobID := task.taskRefs()
		if repoID == "" || jobID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.AbortClientDiff(ctx, repoID, jobID); err != nil {
			a.logger.Warn("取消客户端差异盘失败（服务端可能残留 creating 盘）",
				"repo_id", repoID, "job_id", jobID, "error", err)
		}
	}()

	fail := func(err error) {
		state := diffSyncStateFailed
		if task.ctx.Err() != nil {
			state = diffSyncStateCancelled
		}
		task.update(func(s *diffSyncStatus) {
			s.State = state
			if state == diffSyncStateCancelled {
				// 用户主动取消不算错误，别在界面上弹一条 "context canceled"。
				s.ErrorCode = ""
				s.ErrorMsg = ""
			} else {
				s.ErrorCode = apperr.CodeOf(err)
				s.ErrorMsg = err.Error()
			}
		})
		task.finish(state)
		a.logger.Warn("客户端目录差异盘失败", "diff_sync_id", task.id, "error", err)
	}

	scan, err := scanLocalDirWith(dir, func(files int, bytes int64, rel string) {
		task.update(func(s *diffSyncStatus) {
			s.Phase = diffSyncPhaseScan
			s.Done = files
			s.Bytes = bytes
			s.Current = rel
		})
	})
	if err != nil {
		fail(err)
		return
	}
	entries := make([]scanEntry, 0, len(scan.Entries))
	var totalBytes int64
	for _, e := range scan.Entries {
		if isVerifyRootMetadata(e.Rel) {
			continue
		}
		entries = append(entries, e)
		totalBytes += e.Size
	}
	if len(entries) == 0 {
		fail(errScanEmpty())
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
	task.update(func(s *diffSyncStatus) {
		s.Total = len(entries)
		s.TotalBytes = totalBytes
		s.Phase = diffSyncPhaseScan
		s.Done = len(entries)
		s.Bytes = totalBytes
		s.Current = ""
		s.Warnings = append([]string{}, scan.Warnings...)
	})

	mode := normalizeDiffSyncMode(req.Mode)
	files := make([]clientDiffFile, 0, len(entries))
	for _, e := range entries {
		files = append(files, clientDiffFile{Path: e.Rel, Size: e.Size, Mtime: e.Mtime})
	}
	if mode == "hash" {
		var hashedBytes int64
		for i := range files {
			if task.ctx.Err() != nil {
				fail(task.ctx.Err())
				return
			}
			abs := filepath.Join(dir, filepath.FromSlash(files[i].Path))
			task.update(func(s *diffSyncStatus) {
				s.Phase = diffSyncPhaseHash
				s.Done = i
				s.Bytes = hashedBytes
				s.Current = files[i].Path
			})
			sum, hErr := hashLocalFileProgress(abs, func(n int64) {
				hashedBytes += n
				task.update(func(s *diffSyncStatus) {
					s.Phase = diffSyncPhaseHash
					s.Done = i
					s.Bytes = hashedBytes
					s.Current = files[i].Path
				})
			})
			if hErr != nil {
				fail(errDiffSyncFile("hash", files[i].Path, hErr))
				return
			}
			files[i].Hash = sum
		}
		task.update(func(s *diffSyncStatus) {
			s.Phase = diffSyncPhaseHash
			s.Done = len(files)
			s.Bytes = totalBytes
			s.Current = ""
		})
	}

	task.update(func(s *diffSyncStatus) { s.Phase = diffSyncPhasePlan; s.Current = "" })
	jobID, err := client.PlanClientDiff(task.ctx, req.RepoID, files, mode, req.DeleteExtra)
	if err != nil {
		fail(err)
		return
	}
	task.setJob(req.RepoID, jobID)

	for {
		job, jErr := client.Job(task.ctx, jobID)
		if jErr != nil {
			fail(jErr)
			return
		}
		if job.State == "succeeded" {
			break
		}
		if job.State == "failed" || job.State == "cancelled" {
			fail(fmt.Errorf("服务端差异计划任务失败：%s", strings.TrimSpace(job.LastError)))
			return
		}
		if task.ctx.Err() != nil {
			fail(task.ctx.Err())
			return
		}
		select {
		case <-task.ctx.Done():
			fail(task.ctx.Err())
			return
		case <-time.After(diffSyncPollInterval):
		}
	}

	plan, err := client.ClientDiffPlanResult(task.ctx, req.RepoID, jobID)
	if err != nil {
		fail(err)
		return
	}
	task.update(func(s *diffSyncStatus) {
		s.DiffDiskID = plan.DiffDiskID
		s.Phase = diffSyncPhaseUpload
		s.Done = 0
		s.Total = len(plan.Needed)
		s.Bytes = 0
		s.TotalBytes = 0
		s.Current = ""
		s.Warnings = append(s.Warnings, plan.Warnings...)
	})
	var uploadTotal int64
	for _, f := range plan.Needed {
		uploadTotal += f.Size
	}
	task.update(func(s *diffSyncStatus) { s.TotalBytes = uploadTotal })

	result, err := client.UploadClientDiff(task.ctx, req.RepoID, jobID, dir, plan.Needed,
		func(doneFiles, totalFiles int, bytes, total int64, current string) {
			task.update(func(s *diffSyncStatus) {
				s.Phase = diffSyncPhaseUpload
				s.Done = doneFiles
				s.Total = totalFiles
				s.Bytes = bytes
				s.TotalBytes = total
				s.Current = current
			})
		})
	if err != nil {
		// 响应可能在服务端已经写完之后丢失：先查一次最终结果，能读回就按成功处理。
		if recovered, rErr := client.ClientDiffResult(task.ctx, req.RepoID, jobID); rErr == nil {
			result = recovered
		} else {
			fail(err)
			return
		}
	}
	task.update(func(s *diffSyncStatus) {
		s.State = diffSyncStateSucceeded
		s.Result = result
		s.DiffDiskID = result.DiffDiskID
		s.Warnings = append(s.Warnings, result.Warnings...)
	})
	task.finish(diffSyncStateSucceeded)
	a.logger.Info("客户端目录差异盘完成", "diff_sync_id", task.id,
		"disk_id", result.DiffDiskID, "added", result.Added, "modified", result.Modified, "deleted", result.Deleted)
}

func errDiffSyncFile(stage, path string, err error) error {
	return apperr.New("agent.diff_file_failed", http.StatusInternalServerError).
		WithArg("stage", stage).WithArg("path", path).WithCause(err)
}

// ---- 服务端 API（客户端目录差异） ----

type clientDiffFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

type clientDiffNeededFile struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

type clientDiffPlanResult struct {
	RepoID      string                 `json:"repo_id"`
	DiffDiskID  string                 `json:"diff_disk_id"`
	Mode        string                 `json:"mode"`
	Added       int                    `json:"added"`
	Modified    int                    `json:"modified"`
	Deleted     int                    `json:"deleted"`
	Needed      []clientDiffNeededFile `json:"needed"`
	Deletes     []diffSyncEntry        `json:"deletes"`
	SourceFiles int                    `json:"source_files"`
	SourceBytes int64                  `json:"source_bytes"`
	Warnings    []string               `json:"warnings"`
	Truncated   bool                   `json:"truncated"`
}

type diffSyncEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type diffSyncResult struct {
	RepoID      string          `json:"repo_id"`
	DiffDiskID  string          `json:"diff_disk_id"`
	SourceDir   string          `json:"source_dir"`
	Mode        string          `json:"mode"`
	Added       int             `json:"added"`
	Modified    int             `json:"modified"`
	Deleted     int             `json:"deleted"`
	SourceFiles int             `json:"source_files"`
	SourceBytes int64           `json:"source_bytes"`
	DiffFiles   int             `json:"diff_files"`
	DiffBytes   int64           `json:"diff_bytes"`
	Entries     []diffSyncEntry `json:"entries"`
	Warnings    []string        `json:"warnings"`
	Truncated   bool            `json:"truncated"`
}

func (c *serverClient) PlanClientDiff(ctx context.Context, repoID string, files []clientDiffFile, mode string, deleteExtra bool) (string, error) {
	body := map[string]any{"files": files, "mode": mode, "delete_extra": deleteExtra}
	var out struct {
		ID string `json:"id"`
	}
	path := "/v1/repos/" + url.PathEscape(repoID) + "/diffs/from-dir/client-plan"
	// 清单可能几十 MB，且服务端要同步建盘/挂载后才入队：用长耗时客户端。
	if err := c.doLong(ctx, http.MethodPost, path, body, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", apperr.New("agent.diff_plan_invalid", http.StatusBadGateway)
	}
	return out.ID, nil
}

func (c *serverClient) ClientDiffPlanResult(ctx context.Context, repoID, jobID string) (*clientDiffPlanResult, error) {
	path := "/v1/repos/" + url.PathEscape(repoID) + "/diffs/from-dir/client-plan/result?job=" + url.QueryEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, errServerUnreachable(err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, errServerUnreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 计划结果可能包含几十万条 Needed，4MiB 的通用上限不够；这里单独放宽到 64MiB。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseServerError(resp.StatusCode, raw)
	}
	var out clientDiffPlanResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.New("agent.diff_plan_invalid", http.StatusBadGateway).WithCause(err)
	}
	return &out, nil
}

// UploadClientDiff 只上传计划里 Needed 的文件内容（multipart，流式）。
func (c *serverClient) UploadClientDiff(ctx context.Context, repoID, jobID, localDir string,
	files []clientDiffNeededFile, onProgress func(doneFiles, totalFiles int, bytes, total int64, current string)) (*diffSyncResult, error) {
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()
	var writeMu sync.Mutex
	var writeErr error
	setWriteErr := func(err error) {
		writeMu.Lock()
		if writeErr == nil {
			writeErr = err
		}
		writeMu.Unlock()
	}
	go func() {
		err := writeClientDiffMultipart(mw, localDir, files, onProgress)
		if err != nil {
			setWriteErr(err)
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	path := "/v1/repos/" + url.PathEscape(repoID) + "/diffs/from-dir/client-upload?job=" + url.QueryEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, pr)
	if err != nil {
		_ = pr.Close()
		return nil, errServerUnreachable(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		_ = pr.Close()
		writeMu.Lock()
		localErr := writeErr
		writeMu.Unlock()
		if localErr != nil {
			return nil, localErr
		}
		return nil, errServerUnreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 最终结果含改动清单（最多 5000 条），给足余量。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = pr.Close()
		return nil, parseServerError(resp.StatusCode, raw)
	}
	var out diffSyncResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.New("agent.diff_upload_invalid", http.StatusBadGateway).WithCause(err)
	}
	return &out, nil
}

// ClientDiffResult 读取最终结果（上传响应丢失时用来自愈，避免把已完成的盘判失败）。
func (c *serverClient) ClientDiffResult(ctx context.Context, repoID, jobID string) (*diffSyncResult, error) {
	var out diffSyncResult
	path := "/v1/repos/" + url.PathEscape(repoID) + "/diffs/from-dir/client-upload/result?job=" + url.QueryEscape(jobID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *serverClient) AbortClientDiff(ctx context.Context, repoID, jobID string) error {
	path := "/v1/repos/" + url.PathEscape(repoID) + "/diffs/from-dir/client-abort?job=" + url.QueryEscape(jobID)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// writeClientDiffMultipart 把本机文件按计划顺序写进 multipart 流。
func writeClientDiffMultipart(mw *multipart.Writer, localDir string,
	files []clientDiffNeededFile, onProgress func(doneFiles, totalFiles int, bytes, total int64, current string)) error {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	var doneBytes int64
	buf := make([]byte, diffSyncUploadBuffer)
	for i, f := range files {
		abs, err := localDiffPath(localDir, f.Path)
		if err != nil {
			_ = mw.Close()
			return err
		}
		info, err := os.Stat(abs)
		if err != nil {
			_ = mw.Close()
			return errDiffSyncFile("stat", f.Path, err)
		}
		if info.Size() != f.Size {
			_ = mw.Close()
			return apperr.New("agent.diff_file_changed", http.StatusConflict).
				WithArg("path", f.Path).WithArg("expected", f.Size).WithArg("actual", info.Size())
		}
		if err := mw.WriteField("path", f.Path); err != nil {
			return err
		}
		part, err := mw.CreateFormFile("file", filepath.Base(f.Path))
		if err != nil {
			return err
		}
		in, err := os.Open(abs)
		if err != nil {
			return errDiffSyncFile("open", f.Path, err)
		}
		for {
			n, readErr := in.Read(buf)
			if n > 0 {
				if _, wErr := part.Write(buf[:n]); wErr != nil {
					_ = in.Close()
					return wErr
				}
				doneBytes += int64(n)
				if onProgress != nil {
					onProgress(i, len(files), doneBytes, total, f.Path)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				_ = in.Close()
				return errDiffSyncFile("read", f.Path, readErr)
			}
		}
		_ = in.Close()
		if onProgress != nil {
			onProgress(i+1, len(files), doneBytes, total, "")
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	return nil
}

// localDiffPath 把清单相对路径安全地拼到本地源目录下。
func localDiffPath(root, rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") {
		return "", apperr.InvalidParam("path")
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", apperr.InvalidParam("path")
		}
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(r, "..") {
		return "", apperr.InvalidParam("path")
	}
	return abs, nil
}

func normalizeDiffSyncMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "hash":
		return "hash"
	case "size":
		return "size"
	default:
		return "auto"
	}
}
