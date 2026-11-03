package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"vault/internal/domain"
)

// diffFromDirRequest 是"从服务端目录创建差异盘"的请求体。
type diffFromDirRequest struct {
	// SourceDir 服务端本地源目录（受 storage.source_roots 白名单约束）。
	SourceDir string `json:"source_dir"`
	// Mode 比较模式：auto（默认，大小 + 修改时间）| hash（大小 + SHA-256）| size（只比大小）。
	Mode string `json:"mode"`
	// DeleteExtra 是否把源目录中不存在的文件从差异盘里删掉（镜像语义，默认 true）。
	DeleteExtra *bool `json:"delete_extra"`
}

// handleStartDiffFromDir 从服务端目录创建一块差异盘（异步任务，202）。
//
// 先比较源目录与母盘现有内容，生成新增/修改/删除清单，再在差异盘里只执行这些改动。
// 母盘本身不变；创建出的差异盘可在维护态合并回母盘，也可分配给用户。
func (r *Router) handleStartDiffFromDir(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in diffFromDirRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	deleteExtra := true
	if in.DeleteExtra != nil {
		deleteExtra = *in.DeleteExtra
	}
	j, err := r.deps.App.Disks().StartDiffFromDir(req.Context(), id, in.SourceDir, in.Mode, deleteExtra, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

// handleDiffFromDirResult 读取"从目录创建差异盘"的结果清单。
func (r *Router) handleDiffFromDirResult(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.Disks().GetDiffSyncResult(req.Context(), id, queryString(req, "job"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, res)
}

// mergeDiffRequest 是维护态"合并差异盘更新母盘"的请求体。
type mergeDiffRequest struct {
	DiffDiskID string `json:"diff_disk_id"`
}

// handleMergeDiff 把选中的差异盘合并进母盘（异步任务，202）。
//
// ⚠️ 破坏性：同一母盘下未选中的差异盘会被删除；成功后母盘版本 +1 并退出维护。
func (r *Router) handleMergeDiff(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in mergeDiffRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	j, err := r.deps.App.Repos().MergeDiff(req.Context(), id, in.DiffDiskID, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}
