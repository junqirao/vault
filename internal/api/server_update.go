package api

import (
	"net/http"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

const serverUpdateStatusPath = "/v1/system/server-update"

// handleServerUpdateStatus 返回服务端自更新状态（仅超级管理员）。
func (r *Router) handleServerUpdateStatus(w http.ResponseWriter, req *http.Request) {
	if r.deps.Update == nil {
		r.writeError(w, req, errUpdateUnsupported())
		return
	}
	r.writeJSON(w, http.StatusOK, r.deps.Update.Status())
}

// handleServerUpdateCheck 查询 GitHub Release。
func (r *Router) handleServerUpdateCheck(w http.ResponseWriter, req *http.Request) {
	if r.deps.Update == nil {
		r.writeError(w, req, errUpdateUnsupported())
		return
	}
	status, _ := r.deps.Update.Check(req.Context())
	r.writeJSON(w, http.StatusOK, status)
	if p, err := r.principal(req); err == nil {
		version := ""
		if status.Latest != nil {
			version = status.Latest.Version
		}
		result := domain.AuditResultOK
		if status.Error != nil {
			result = domain.AuditResultError
		}
		r.deps.App.Audit(req.Context(), p.UserID, "system.server_update.check", version, "", result)
	}
}

// handleServerUpdateDownload 启动下载并解压（202 立即返回，进度轮询状态接口）。
func (r *Router) handleServerUpdateDownload(w http.ResponseWriter, req *http.Request) {
	if r.deps.Update == nil {
		r.writeError(w, req, errUpdateUnsupported())
		return
	}
	status, err := r.deps.Update.StartDownload(req.Context())
	if err != nil {
		r.writeError(w, req, updateStartError(err))
		return
	}
	r.writeJSON(w, http.StatusAccepted, status)
	if p, perr := r.principal(req); perr == nil && status.Latest != nil {
		r.deps.App.Audit(req.Context(), p.UserID, "system.server_update.download", status.Latest.Version, "", domain.AuditResultOK)
	}
}

// handleServerUpdateCancel 取消下载（保留 .part 供下次续传）。
func (r *Router) handleServerUpdateCancel(w http.ResponseWriter, req *http.Request) {
	if r.deps.Update == nil {
		r.writeError(w, req, errUpdateUnsupported())
		return
	}
	status, _ := r.deps.Update.CancelDownload(req.Context())
	r.writeJSON(w, http.StatusOK, status)
}

// handleServerUpdateApply 启动替换器，返回 202 后让旧服务端优雅停机。
func (r *Router) handleServerUpdateApply(w http.ResponseWriter, req *http.Request) {
	if r.deps.Update == nil || r.deps.RequestShutdown == nil {
		r.writeError(w, req, errUpdateUnsupported())
		return
	}
	status, err := r.deps.Update.PrepareApply(req.Context())
	if err != nil {
		r.writeError(w, req, updateStartError(err))
		return
	}
	r.writeJSON(w, http.StatusAccepted, status)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if p, perr := r.principal(req); perr == nil {
		version := ""
		if status.Latest != nil {
			version = status.Latest.Version
		}
		r.deps.App.Audit(req.Context(), p.UserID, "system.server_update.apply", version, "", domain.AuditResultOK)
	}
	if r.deps.RequestShutdown != nil {
		// 给响应和审计一点落盘时间，再触发优雅停机；helper 已经在等本进程退出。
		go func() {
			time.Sleep(300 * time.Millisecond)
			r.deps.RequestShutdown()
		}()
	}
}

func errUpdateUnsupported() error {
	return apperr.New("server_update.unsupported", http.StatusNotImplemented)
}

func updateStartError(err error) error {
	if err == nil {
		return nil
	}
	return apperr.New(err.Error(), http.StatusConflict)
}
