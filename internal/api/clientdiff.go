package api

import (
	"mime"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// clientDiffPlanRequest 是客户端目录创建差异盘的第一阶段请求。
type clientDiffPlanRequest struct {
	Files       []app.ClientDiffFile `json:"files"`
	Mode        string               `json:"mode"`
	DeleteExtra *bool                `json:"delete_extra"`
}

// handleActiveDiffOperation 返回当前正在执行的差异盘工具任务（界面刷新/重进页面后恢复进度）。
func (r *Router) handleActiveDiffOperation(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	job, err := r.deps.App.Repos().ActiveDiffOperation(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if job == nil {
		r.writeJSON(w, http.StatusOK, map[string]any{"job": nil})
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"job": toJobDTO(job)})
}

// handleCancelActiveDiffOperation 取消当前存储库的差异盘工具任务（库管理者即可）。
func (r *Router) handleCancelActiveDiffOperation(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Repos().CancelActiveDiffOperation(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleClientDiffPlan 接收客户端清单，异步建盘并计算需要上传的文件。
func (r *Router) handleClientDiffPlan(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in clientDiffPlanRequest
	// 清单可能很大（20 万文件的元数据），与上传核对同一档上限。
	if err := r.decodeJSONLimit(req, &in, maxVerifyDiffBodyBytes); err != nil {
		r.writeError(w, req, err)
		return
	}
	deleteExtra := true
	if in.DeleteExtra != nil {
		deleteExtra = *in.DeleteExtra
	}
	j, err := r.deps.App.Disks().StartClientDiffPlan(req.Context(), id, in.Files, in.Mode, deleteExtra, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

// handleClientDiffPlanResult 读取第一阶段的计划结果（Needed 文件列表）。
func (r *Router) handleClientDiffPlanResult(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.Disks().GetClientDiffPlanResult(req.Context(), id, queryString(req, "job"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, res)
}

// handleClientDiffUpload 接收客户端上传的新增/修改文件内容（multipart）。
//
// 每个文件前有一个 path 字段，随后是一个 file 部件；服务端只接受计划里的路径。
func (r *Router) handleClientDiffUpload(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") || params["boundary"] == "" {
		r.writeError(w, req, apperr.InvalidParam("Content-Type"))
		return
	}
	res, err := r.deps.App.Disks().CompleteClientDiff(req.Context(), id, queryString(req, "job"), req.Body, params["boundary"])
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, res)
}

// handleClientDiffResult 读取最终结果（上传已完成但响应丢失时用于客户端自愈）。
func (r *Router) handleClientDiffResult(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.Disks().GetClientDiffResult(req.Context(), id, queryString(req, "job"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, res)
}

// handleClientDiffAbort 客户端取消时删除未完成的差异盘。
func (r *Router) handleClientDiffAbort(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Disks().AbortClientDiff(req.Context(), id, queryString(req, "job")); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}
