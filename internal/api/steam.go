package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// steamLongOperationContext 让登录/许可同步脱离 HTTP 请求生命周期。
//
// 浏览器/代理可能先超时断开，但 SteamCMD 已经启动、租约也已经占用；
// 如果继续使用 req.Context()，服务端会以 "context canceled" 中止并留下半个状态。
// 这里给操作一个独立、有上限的上下文，客户端断开后仍能完成并落库。
func steamLongOperationContext(req *http.Request) (context.Context, context.CancelFunc) {
	// 登录测试成功后还会同步许可；两步合计可能接近 5 分钟，这里留足余量。
	return context.WithTimeout(context.WithoutCancel(req.Context()), 10*time.Minute)
}

// ---------- Steam 账号（仅超级管理员；仅用于存储库下载/更新） ----------

type createSteamAccountRequest struct {
	Username       string `json:"username"`
	DisplayName    string `json:"display_name"`
	Credential     string `json:"credential"`
	CredentialKind string `json:"credential_kind"`
	GuardType      string `json:"guard_type"`
	GuardSecret    string `json:"guard_secret"`
	MaxConcurrent  int    `json:"max_concurrent"`
}

func (r *Router) handleListSteamAccounts(w http.ResponseWriter, req *http.Request) {
	accounts, err := r.deps.App.SteamAccounts().List(req.Context(), queryInt(req, "limit", 200), queryInt(req, "offset", 0))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items := make([]steamAccountDTO, 0, len(accounts))
	for i := range accounts {
		dto := toSteamAccountDTO(&accounts[i])
		if n, err := r.deps.App.SteamAccounts().ActiveLeaseCount(req.Context(), accounts[i].ID); err == nil {
			dto.ActiveLeases = n
		}
		items = append(items, dto)
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (r *Router) handleCreateSteamAccount(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in createSteamAccountRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	account, err := r.deps.App.SteamAccounts().Create(req.Context(), app.CreateSteamAccountInput{
		Username:       in.Username,
		DisplayName:    in.DisplayName,
		Credential:     in.Credential,
		CredentialKind: domain.SteamCredentialKind(in.CredentialKind),
		GuardType:      domain.SteamGuardType(in.GuardType),
		GuardSecret:    in.GuardSecret,
		MaxConcurrent:  in.MaxConcurrent,
	}, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, toSteamAccountDTO(account))
}

func (r *Router) handleGetSteamAccount(w http.ResponseWriter, req *http.Request) {
	account, err := r.deps.App.SteamAccounts().Get(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	dto := toSteamAccountDTO(account)
	if n, err := r.deps.App.SteamAccounts().ActiveLeaseCount(req.Context(), account.ID); err == nil {
		dto.ActiveLeases = n
	}
	r.writeJSON(w, http.StatusOK, dto)
}

type updateSteamAccountRequest struct {
	DisplayName    *string `json:"display_name"`
	Credential     *string `json:"credential"`
	CredentialKind *string `json:"credential_kind"`
	GuardType      *string `json:"guard_type"`
	GuardSecret    *string `json:"guard_secret"`
	MaxConcurrent  *int    `json:"max_concurrent"`
	Enabled        *bool   `json:"enabled"`
}

func (r *Router) handleUpdateSteamAccount(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in updateSteamAccountRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	update := app.UpdateSteamAccountInput{
		ID:            chi.URLParam(req, "id"),
		DisplayName:   in.DisplayName,
		Credential:    in.Credential,
		MaxConcurrent: in.MaxConcurrent,
		Enabled:       in.Enabled,
	}
	if in.CredentialKind != nil {
		v := domain.SteamCredentialKind(*in.CredentialKind)
		update.CredentialKind = &v
	}
	if in.GuardType != nil {
		v := domain.SteamGuardType(*in.GuardType)
		update.GuardType = &v
	}
	update.GuardSecret = in.GuardSecret
	account, err := r.deps.App.SteamAccounts().Update(req.Context(), update, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toSteamAccountDTO(account))
}

func (r *Router) handleDeleteSteamAccount(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.SteamAccounts().Disable(req.Context(), chi.URLParam(req, "id"), p.UserID); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

type steamGuardRequest struct {
	GuardCode string `json:"guard_code"`
}

func (r *Router) handleTestSteamLogin(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in steamGuardRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	ctx, cancel := steamLongOperationContext(req)
	defer cancel()
	account, err := r.deps.App.SteamAccounts().TestLogin(ctx, chi.URLParam(req, "id"), in.GuardCode, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toSteamAccountDTO(account))
}

// handleReleaseSteamLeases 强制释放账号的全部活跃租约（管理员排障）。
func (r *Router) handleReleaseSteamLeases(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	n, err := r.deps.App.SteamAccounts().ForceReleaseLeases(req.Context(), chi.URLParam(req, "id"), p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"released": n})
}

func (r *Router) handleSyncSteamLicenses(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in steamGuardRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	ctx, cancel := steamLongOperationContext(req)
	defer cancel()
	apps, err := r.deps.App.SteamAccounts().SyncLicenses(ctx, chi.URLParam(req, "id"), in.GuardCode, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toSteamAppDTOs(apps)})
}

type importSteamClientGamesRequest struct {
	Games []struct {
		AppID     int64  `json:"app_id"`
		Name      string `json:"name"`
		Installed bool   `json:"installed"`
	} `json:"games"`
}

// handleImportSteamClientGames 接收客户端本机 Steam 库列表。
//
// 列表由客户端扫描本地 Steam 得到，服务端只保存 AppID/名称，不访问 Steam 商店，
// 也不接触客户端的 Steam 登录凭据。
func (r *Router) handleImportSteamClientGames(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in importSteamClientGamesRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if len(in.Games) == 0 {
		r.writeError(w, req, apperr.InvalidParam("games"))
		return
	}
	if len(in.Games) > 5000 {
		r.writeError(w, req, apperr.InvalidParam("games").WithArg("max", 5000))
		return
	}
	games := make([]app.ImportClientGame, 0, len(in.Games))
	for _, item := range in.Games {
		games = append(games, app.ImportClientGame{
			AppID: item.AppID, Name: item.Name, Installed: item.Installed,
		})
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 2*time.Minute)
	defer cancel()
	count, err := r.deps.App.SteamAccounts().ImportClientGames(ctx, chi.URLParam(req, "id"), games, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"imported": count})
}

// handleListSteamOwnedGames 返回账号已同步的全部游戏（带名称/封面/平台）。
//
// 这是「批量添加游戏」的主数据源：用户直接看到账号拥有的游戏列表，
// 不需要知道 AppID，也不需要先按名称搜索。
func (r *Router) handleListSteamOwnedGames(w http.ResponseWriter, req *http.Request) {
	refresh := strings.EqualFold(queryString(req, "refresh"), "true") ||
		queryString(req, "refresh") == "1"
	ctx, cancel := steamLongOperationContext(req)
	defer cancel()
	items, err := r.deps.App.SteamAccounts().OwnedGames(ctx, chi.URLParam(req, "id"),
		queryString(req, "lang"), queryString(req, "cc"), refresh)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (r *Router) handleListSteamAccountApps(w http.ResponseWriter, req *http.Request) {
	apps, err := r.deps.App.SteamAccounts().ListApps(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toSteamAppDTOs(apps)})
}

func (r *Router) handleListAvailableSteamAccounts(w http.ResponseWriter, req *http.Request) {
	appID := int64(queryInt(req, "app_id", 0))
	platform := queryString(req, "platform")
	if platform != "" && steamPlatformOrDefaultAPI(platform) == "" {
		r.writeError(w, req, apperr.InvalidParam("platform"))
		return
	}
	items, err := r.deps.App.SteamAccounts().Available(req.Context(), appID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toSteamAvailableDTOs(items)})
}

func steamPlatformOrDefaultAPI(v string) string {
	switch v {
	case "", "windows", "linux":
		return v
	default:
		return ""
	}
}

// ---------- SteamCMD 安装状态（管理端） ----------

func (r *Router) handleSteamRuntimeStatus(w http.ResponseWriter, req *http.Request) {
	r.writeJSON(w, http.StatusOK, r.deps.App.SteamAccounts().RuntimeStatus())
}

// handleSearchSteamStore 代理 Steam 商店搜索，供「批量添加游戏」勾选列表使用。
func (r *Router) handleSearchSteamStore(w http.ResponseWriter, req *http.Request) {
	items, err := r.deps.App.SteamAccounts().SearchSteamStore(
		req.Context(), queryString(req, "q"), queryString(req, "lang"), queryString(req, "cc"), queryString(req, "account_id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCheckSteamNetwork 从服务端进程探测 Steam CDN 连通性。
//
// 失败也返回 200 + 结构化结果，便于界面同时展示状态码、延迟、代理和错误原因。
func (r *Router) handleCheckSteamNetwork(w http.ResponseWriter, req *http.Request) {
	r.writeJSON(w, http.StatusOK, r.deps.App.SteamAccounts().CheckSteamNetwork(req.Context()))
}

// handleInstallSteamCmd 启动后台安装 SteamCMD，并立即返回当前进度。
//
// 下载 + 首次自更新可能持续几十秒到几分钟；进度通过 GET /v1/steam/runtime 轮询。
func (r *Router) handleInstallSteamCmd(w http.ResponseWriter, req *http.Request) {
	status, err := r.deps.App.SteamAccounts().StartInstallSteamCmd()
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, status)
}

// ---------- 用户游戏目录（共享库可自由挂载） ----------

// handleGameCatalog 返回普通用户可见的 Steam 游戏目录。
//
// 库是共享的：任何登录用户都可以在这里看到游戏并分配自己的差异盘/挂载；
// Steam 账号只用于服务端更新，绝不下发给客户端。
func (r *Router) handleGameCatalog(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items, err := r.deps.App.SteamGames().Catalog(req.Context(), p.UserID, p.IsSuperAdmin())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toGameCatalogDTOs(items)})
}

// ---------- 分组 ----------

type createRepoGroupRequest struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Description    string   `json:"description"`
	SteamAccountID string   `json:"steam_account_id"`
	TargetPlatform string   `json:"target_platform"`
	AutoUpdate     *bool    `json:"auto_update"`
	UpdateSchedule string   `json:"update_schedule"`
	UpdateWindow   string   `json:"update_window"`
	MaxConcurrent  int      `json:"max_concurrent"`
	StorageID      string   `json:"storage_id"`
	AccessMode     string   `json:"access_mode"`
	MemberIDs      []string `json:"member_ids"`
}

func (r *Router) handleListRepoGroups(w http.ResponseWriter, req *http.Request) {
	kind := domain.RepoGroupKind(queryString(req, "kind"))
	if kind != "" && !kind.Valid() {
		r.writeError(w, req, apperr.InvalidParam("kind"))
		return
	}
	groups, err := r.deps.App.RepoGroups().List(req.Context(), kind)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items := make([]repoGroupDTO, 0, len(groups))
	for i := range groups {
		dto := toRepoGroupDTO(&groups[i])
		if groups[i].AccessMode == domain.RepoGroupAccessSelected {
			if members, mErr := r.deps.App.RepoGroups().ListMembers(req.Context(), groups[i].ID); mErr == nil {
				ids := make([]string, 0, len(members))
				for _, m := range members {
					ids = append(ids, m.UserID)
				}
				dto.MemberIDs = ids
			}
		}
		if games, gErr := r.deps.App.SteamGames().ListGames(req.Context(), groups[i].ID); gErr == nil {
			dto.GameCount = len(games)
		}
		items = append(items, dto)
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (r *Router) handleCreateRepoGroup(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in createRepoGroupRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	var accountID *string
	if in.SteamAccountID != "" {
		accountID = &in.SteamAccountID
	}
	group, err := r.deps.App.RepoGroups().Create(req.Context(), app.CreateRepoGroupInput{
		Name: in.Name, Kind: domain.RepoGroupKind(in.Kind), Description: in.Description,
		SteamAccountID: accountID, TargetPlatform: in.TargetPlatform, AutoUpdate: in.AutoUpdate,
		UpdateSchedule: in.UpdateSchedule, UpdateWindow: in.UpdateWindow,
		MaxConcurrent: in.MaxConcurrent, StorageID: in.StorageID,
		AccessMode: domain.RepoGroupAccessMode(in.AccessMode), MemberIDs: in.MemberIDs,
	}, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, toRepoGroupDTO(group))
}

func (r *Router) handleGetRepoGroup(w http.ResponseWriter, req *http.Request) {
	group, err := r.deps.App.RepoGroups().Get(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	dto := toRepoGroupDTO(group)
	if group.AccessMode == domain.RepoGroupAccessSelected {
		if members, mErr := r.deps.App.RepoGroups().ListMembers(req.Context(), group.ID); mErr == nil {
			ids := make([]string, 0, len(members))
			for _, m := range members {
				ids = append(ids, m.UserID)
			}
			dto.MemberIDs = ids
		}
	}
	if games, gErr := r.deps.App.SteamGames().ListGames(req.Context(), group.ID); gErr == nil {
		dto.GameCount = len(games)
	}
	r.writeJSON(w, http.StatusOK, dto)
}

type updateRepoGroupRequest struct {
	Name           *string   `json:"name"`
	Kind           *string   `json:"kind"`
	Description    *string   `json:"description"`
	SteamAccountID *string   `json:"steam_account_id"`
	TargetPlatform *string   `json:"target_platform"`
	AutoUpdate     *bool     `json:"auto_update"`
	UpdateSchedule *string   `json:"update_schedule"`
	UpdateWindow   *string   `json:"update_window"`
	MaxConcurrent  *int      `json:"max_concurrent"`
	StorageID      *string   `json:"storage_id"`
	AccessMode     *string   `json:"access_mode"`
	MemberIDs      *[]string `json:"member_ids"`
}

func (r *Router) handleUpdateRepoGroup(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in updateRepoGroupRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	update := app.UpdateRepoGroupInput{
		ID: chi.URLParam(req, "id"), Name: in.Name, Description: in.Description,
		SteamAccountID: in.SteamAccountID, TargetPlatform: in.TargetPlatform,
		AutoUpdate: in.AutoUpdate, UpdateSchedule: in.UpdateSchedule, UpdateWindow: in.UpdateWindow,
		MaxConcurrent: in.MaxConcurrent, StorageID: in.StorageID,
		MemberIDs: in.MemberIDs,
	}
	if in.Kind != nil {
		v := domain.RepoGroupKind(*in.Kind)
		update.Kind = &v
	}
	if in.AccessMode != nil {
		v := domain.RepoGroupAccessMode(*in.AccessMode)
		update.AccessMode = &v
	}
	group, err := r.deps.App.RepoGroups().Update(req.Context(), update, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toRepoGroupDTO(group))
}

func (r *Router) handleDeleteRepoGroup(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.RepoGroups().Delete(req.Context(), chi.URLParam(req, "id"), p.UserID); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// ---------- 游戏 ----------

type createGameRequest struct {
	AppID          int64   `json:"app_id"`
	AppName        string  `json:"app_name"`
	RepoID         string  `json:"repo_id"`
	RepoName       string  `json:"repo_name"`
	InstallDir     string  `json:"install_dir"`
	CreateRepo     bool    `json:"create_repo"`
	SizeBytes      int64   `json:"size_bytes"`
	StorageID      string  `json:"storage_id"`
	ShareCount     int     `json:"share_count"`
	TargetPlatform string  `json:"target_platform"`
	SteamAccountID *string `json:"steam_account_id"`
	AutoUpdate     *bool   `json:"auto_update"`
}

// ---------- 批量添加游戏 ----------

type batchCreateGamesRequest struct {
	SizeBytes int64  `json:"size_bytes"`
	StorageID string `json:"storage_id"`
	Games     []struct {
		AppID   int64  `json:"app_id"`
		AppName string `json:"app_name"`
	} `json:"games"`
}

type batchGameFailure struct {
	AppID   int64  `json:"app_id"`
	AppName string `json:"app_name"`
	Code    string `json:"code"`
}

// handleBatchCreateGames 批量添加游戏；每个游戏建立一个独立共享库。
func (r *Router) handleBatchCreateGames(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in batchCreateGamesRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if len(in.Games) == 0 {
		r.writeError(w, req, apperr.InvalidParam("games"))
		return
	}
	if len(in.Games) > 50 {
		r.writeError(w, req, apperr.InvalidParam("games").WithArg("max", 50))
		return
	}
	// 一次最多 50 个游戏，每个都会建库；批量创建可能超过浏览器默认 20 秒。
	// 与同步许可一样脱离请求上下文，避免客户端超时把创建过程拦腰截断。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 15*time.Minute)
	defer cancel()
	groupID := chi.URLParam(req, "id")
	created := make([]gameDTO, 0, len(in.Games))
	failed := make([]batchGameFailure, 0)
	for _, item := range in.Games {
		if item.AppID <= 0 {
			failed = append(failed, batchGameFailure{AppID: item.AppID, AppName: item.AppName, Code: "system.invalid_param"})
			continue
		}
		game, err := r.deps.App.SteamGames().CreateGame(ctx, app.CreateGameInput{
			GroupID: groupID, AppID: item.AppID, AppName: item.AppName,
			CreateRepo: true, SizeBytes: in.SizeBytes, StorageID: in.StorageID,
			// 批量列表来自客户端本机 Steam；服务端许可缓存可能滞后，下载时再校验。
			SkipLicenseCheck: true,
		}, p.UserID)
		if err != nil {
			failed = append(failed, batchGameFailure{
				AppID: item.AppID, AppName: item.AppName, Code: apperr.CodeOf(err),
			})
			continue
		}
		created = append(created, r.toSteamGameDTO(game))
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"created": created, "failed": failed})
}

// toSteamGameDTO 在普通 DTO 上附加内存中的实时更新进度。
func (r *Router) toSteamGameDTO(game *domain.Game) gameDTO {
	dto := toGameDTO(game)
	progress := r.deps.App.SteamGames().Progress(game.ID)
	dto.ProgressPercent = progress.Percent
	dto.ProgressPhase = progress.Phase
	dto.ProgressStage = progress.Stage
	dto.ProgressBytesDone = progress.BytesDone
	dto.ProgressBytesTotal = progress.BytesTotal
	dto.ProgressSpeed = progress.SpeedBytesPerSec
	dto.CanStop = progress.CanStop
	return dto
}

func (r *Router) toSteamGameDTOs(games []domain.Game) []gameDTO {
	out := make([]gameDTO, 0, len(games))
	for i := range games {
		out = append(out, r.toSteamGameDTO(&games[i]))
	}
	return out
}

func (r *Router) handleListGroupGames(w http.ResponseWriter, req *http.Request) {
	groupID := chi.URLParam(req, "id")
	if _, err := r.deps.App.RepoGroups().Get(req.Context(), groupID); err != nil {
		r.writeError(w, req, err)
		return
	}
	games, err := r.deps.App.SteamGames().ListGames(req.Context(), groupID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": r.toSteamGameDTOs(games)})
}

func (r *Router) handleCreateGame(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in createGameRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	game, err := r.deps.App.SteamGames().CreateGame(req.Context(), app.CreateGameInput{
		GroupID: chi.URLParam(req, "id"), AppID: in.AppID, AppName: in.AppName,
		RepoID: in.RepoID, RepoName: in.RepoName, InstallDir: in.InstallDir,
		CreateRepo: in.CreateRepo, SizeBytes: in.SizeBytes, StorageID: in.StorageID,
		ShareCount: in.ShareCount, TargetPlatform: in.TargetPlatform,
		SteamAccountID: in.SteamAccountID, AutoUpdate: in.AutoUpdate,
	}, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, r.toSteamGameDTO(game))
}

func (r *Router) handleGetGame(w http.ResponseWriter, req *http.Request) {
	game, err := r.deps.App.SteamGames().GetGame(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, r.toSteamGameDTO(game))
}

type updateGameRequest struct {
	AppName        *string `json:"app_name"`
	InstallDir     *string `json:"install_dir"`
	TargetPlatform *string `json:"target_platform"`
	SteamAccountID *string `json:"steam_account_id"`
	AutoUpdate     *bool   `json:"auto_update"`
}

func (r *Router) handleUpdateGame(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in updateGameRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	game, err := r.deps.App.SteamGames().UpdateGame(req.Context(), app.UpdateGameInput{
		ID: chi.URLParam(req, "id"), AppName: in.AppName, InstallDir: in.InstallDir,
		TargetPlatform: in.TargetPlatform, SteamAccountID: in.SteamAccountID, AutoUpdate: in.AutoUpdate,
	}, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, r.toSteamGameDTO(game))
}

func (r *Router) handleDeleteGame(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.SteamGames().DeleteGame(req.Context(), chi.URLParam(req, "id"), p.UserID); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

func (r *Router) handleCheckGroupUpdates(w http.ResponseWriter, req *http.Request) {
	// 组内每个游戏都要单独跑一次 SteamCMD；慢代理下可能超过浏览器默认超时。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 30*time.Minute)
	defer cancel()
	games, err := r.deps.App.SteamGames().CheckGroup(ctx, chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": r.toSteamGameDTOs(games)})
}

func (r *Router) handleTriggerGameUpdate(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	game, err := r.deps.App.SteamGames().TriggerUpdate(req.Context(), chi.URLParam(req, "id"), p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, r.toSteamGameDTO(game))
}

func (r *Router) handlePauseGameUpdate(w http.ResponseWriter, req *http.Request) {
	r.handleControlGameUpdate(w, req, app.SteamUpdateControlPause)
}

// handleGameOutput 返回该游戏最近一次/当前更新的 SteamCMD 原始输出。
//
// 仅超级管理员可访问（与其它 /v1/games 管理接口一致）；输出在内存中保留最近 400 行。
func (r *Router) handleGameOutput(w http.ResponseWriter, req *http.Request) {
	gameID := chi.URLParam(req, "id")
	if _, err := r.deps.App.SteamGames().GetGame(req.Context(), gameID); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{
		"items": r.deps.App.SteamGames().Output(gameID),
	})
}

func (r *Router) handleStopGameUpdate(w http.ResponseWriter, req *http.Request) {
	r.handleControlGameUpdate(w, req, app.SteamUpdateControlStop)
}

func (r *Router) handleControlGameUpdate(w http.ResponseWriter, req *http.Request, mode string) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	game, err := r.deps.App.SteamGames().StopUpdate(req.Context(), chi.URLParam(req, "id"), mode, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, r.toSteamGameDTO(game))
}
