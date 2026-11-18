package api

import (
	"path/filepath"

	"vault/internal/app"
	"vault/internal/domain"
)

// steamAccountDTO 是账号模块的对外视图。
//
// ⚠️ 绝不包含 credential_cipher / auth_token_cipher / guard_secret_cipher；
// 只暴露"是否已设置"和状态。
type steamAccountDTO struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	DisplayName    string `json:"display_name"`
	Status         string `json:"status"`
	CredentialKind string `json:"credential_kind"`
	HasCredential  bool   `json:"has_credential"`
	GuardType      string `json:"guard_type"`
	HasGuardSecret bool   `json:"has_guard_secret"`
	SessionState   string `json:"session_state"`
	// CmdRoot 是该账号独立的 SteamCMD 安装目录（登录态 config.vdf 就在这里）。
	CmdRoot       string `json:"cmd_root"`
	MaxConcurrent int    `json:"max_concurrent"`
	ActiveLeases  int    `json:"active_leases"`
	LastLoginAt   int64  `json:"last_login_at"`
	LastCheckedAt int64  `json:"last_checked_at"`
	LastGuardAt   int64  `json:"last_guard_at"`
	LastError     string `json:"last_error"`
	LastOutput    string `json:"last_output"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func toSteamAccountDTO(a *domain.SteamAccount) steamAccountDTO {
	dto := steamAccountDTO{
		ID:             a.ID,
		Username:       a.Username,
		DisplayName:    a.DisplayName,
		Status:         string(a.Status),
		CredentialKind: string(a.CredentialKind),
		HasCredential:  a.HasCredential(),
		GuardType:      string(a.GuardType),
		HasGuardSecret: a.HasGuardSecret(),
		SessionState:   string(a.SessionState),
		MaxConcurrent:  a.MaxConcurrent,
		LastLoginAt:    a.LastLoginAt,
		LastCheckedAt:  a.LastCheckedAt,
		LastGuardAt:    a.LastGuardAt,
		LastError:      a.LastError,
		LastOutput:     a.LastOutput,
		CreatedAt:      a.CreatedAt,
		UpdatedAt:      a.UpdatedAt,
	}
	if a.SessionHome != "" {
		dto.CmdRoot = filepath.Join(a.SessionHome, "steamcmd")
	}
	return dto
}

type steamAppDTO struct {
	AppID      int64  `json:"app_id"`
	Source     string `json:"source"`
	VerifiedAt int64  `json:"verified_at"`
}

func toSteamAppDTOs(apps []domain.SteamAccountApp) []steamAppDTO {
	out := make([]steamAppDTO, 0, len(apps))
	for i := range apps {
		out = append(out, steamAppDTO{AppID: apps[i].AppID, Source: apps[i].Source, VerifiedAt: apps[i].VerifiedAt})
	}
	return out
}

type steamAvailableDTO struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	Status        string `json:"status"`
	OwnsApp       bool   `json:"owns_app"`
	Busy          bool   `json:"busy"`
	Available     bool   `json:"available"`
	MaxConcurrent int    `json:"max_concurrent"`
	Reason        string `json:"reason"`
}

func toSteamAvailableDTOs(items []app.SteamAccountAvailable) []steamAvailableDTO {
	out := make([]steamAvailableDTO, 0, len(items))
	for i := range items {
		a := items[i].Account
		out = append(out, steamAvailableDTO{
			ID: a.ID, Username: a.Username, DisplayName: a.DisplayName, Status: string(a.Status),
			OwnsApp: items[i].OwnsApp, Busy: items[i].Busy, Available: items[i].Available,
			MaxConcurrent: a.MaxConcurrent, Reason: items[i].Reason,
		})
	}
	return out
}

// repoGroupDTO 是服务端分组视图。Steam 组会带上账号/平台/自动更新配置。
type repoGroupDTO struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Description    string   `json:"description"`
	SteamAccountID string   `json:"steam_account_id,omitempty"`
	TargetPlatform string   `json:"target_platform"`
	AutoUpdate     bool     `json:"auto_update"`
	UpdateSchedule string   `json:"update_schedule"`
	UpdateWindow   string   `json:"update_window"`
	MaxConcurrent  int      `json:"max_concurrent"`
	StorageID      string   `json:"storage_id,omitempty"`
	AccessMode     string   `json:"access_mode"`
	MemberIDs      []string `json:"member_ids,omitempty"`
	GameCount      int      `json:"game_count"`
	CreatedAt      int64    `json:"created_at"`
	UpdatedAt      int64    `json:"updated_at"`
}

func toRepoGroupDTO(g *domain.RepoGroup) repoGroupDTO {
	dto := repoGroupDTO{
		ID: g.ID, Name: g.Name, Kind: string(g.Kind), Description: g.Description,
		TargetPlatform: g.TargetPlatform, AutoUpdate: g.AutoUpdate,
		UpdateSchedule: g.UpdateSchedule, UpdateWindow: g.UpdateWindow,
		MaxConcurrent: g.MaxConcurrent, StorageID: g.StorageID,
		AccessMode: string(g.AccessMode),
		CreatedAt:  g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
	if g.SteamAccountID != nil {
		dto.SteamAccountID = *g.SteamAccountID
	}
	if dto.AccessMode == "" {
		dto.AccessMode = string(domain.RepoGroupAccessAll)
	}
	return dto
}

// gameDTO 是游戏库视图。
type gameDTO struct {
	ID               string `json:"id"`
	GroupID          string `json:"group_id"`
	RepoID           string `json:"repo_id,omitempty"`
	AppID            int64  `json:"app_id"`
	AppName          string `json:"app_name"`
	InstallDir       string `json:"install_dir"`
	BuildID          string `json:"build_id"`
	AvailableBuildID string `json:"available_build_id"`
	TargetPlatform   string `json:"target_platform"`
	SteamAccountID   string `json:"steam_account_id,omitempty"`
	AutoUpdate       *bool  `json:"auto_update,omitempty"`
	Status           string `json:"status"`
	// 实时更新进度（仅 updating 时有值；paused/ready/error 时为空）。
	ProgressPercent float64 `json:"progress_percent"`
	ProgressPhase   string  `json:"progress_phase"`
	// ProgressStage 是 SteamCMD 原始状态（downloading / validating / reconfiguring 等）。
	ProgressStage string `json:"progress_stage"`
	// 字节进度与速度（速度由服务端按字节增量计算，SteamCMD 不直接输出）。
	ProgressBytesDone  int64  `json:"progress_bytes_done"`
	ProgressBytesTotal int64  `json:"progress_bytes_total"`
	ProgressSpeed      int64  `json:"progress_speed"`
	CanStop            bool   `json:"can_stop"`
	LastCheckedAt      int64  `json:"last_checked_at"`
	LastUpdatedAt      int64  `json:"last_updated_at"`
	LastError          string `json:"last_error"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

func toGameDTO(g *domain.Game) gameDTO {
	dto := gameDTO{
		ID: g.ID, GroupID: g.GroupID, AppID: g.AppID, AppName: g.AppName,
		InstallDir: g.InstallDir, BuildID: g.BuildID, AvailableBuildID: g.AvailableBuildID,
		TargetPlatform: g.TargetPlatform, AutoUpdate: g.AutoUpdate, Status: g.Status,
		LastCheckedAt: g.LastCheckedAt, LastUpdatedAt: g.LastUpdatedAt, LastError: g.LastError,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
	if g.RepoID != nil {
		dto.RepoID = *g.RepoID
	}
	if g.SteamAccountID != nil {
		dto.SteamAccountID = *g.SteamAccountID
	}
	return dto
}

func toGameDTOs(games []domain.Game) []gameDTO {
	out := make([]gameDTO, 0, len(games))
	for i := range games {
		out = append(out, toGameDTO(&games[i]))
	}
	return out
}

// catalogGameDTO 是普通用户看到的游戏目录项。
//
// 刻意不包含 install_dir / last_error 等内部字段，也不包含 Steam 账号信息。
type catalogGameDTO struct {
	ID               string `json:"id"`
	GroupID          string `json:"group_id"`
	RepoID           string `json:"repo_id"`
	AppID            int64  `json:"app_id"`
	AppName          string `json:"app_name"`
	BuildID          string `json:"build_id"`
	AvailableBuildID string `json:"available_build_id"`
	TargetPlatform   string `json:"target_platform"`
	Status           string `json:"status"`
	// 实时更新进度：普通用户也要能在存储库列表里看到同一个下载进度。
	ProgressPercent    float64 `json:"progress_percent"`
	ProgressPhase      string  `json:"progress_phase"`
	ProgressStage      string  `json:"progress_stage"`
	ProgressBytesDone  int64   `json:"progress_bytes_done"`
	ProgressBytesTotal int64   `json:"progress_bytes_total"`
	ProgressSpeed      int64   `json:"progress_speed"`
	LastUpdatedAt      int64   `json:"last_updated_at"`
	Repo               repoDTO `json:"repo"`
}

type catalogGroupDTO struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Games       []catalogGameDTO `json:"games"`
}

func toGameCatalogDTOs(items []app.GameCatalogItem) []catalogGroupDTO {
	order := make([]string, 0)
	byID := map[string]*catalogGroupDTO{}
	for i := range items {
		item := &items[i]
		group := byID[item.Group.ID]
		if group == nil {
			group = &catalogGroupDTO{
				ID: item.Group.ID, Name: item.Group.Name, Description: item.Group.Description,
				Games: make([]catalogGameDTO, 0),
			}
			byID[item.Group.ID] = group
			order = append(order, item.Group.ID)
		}
		game := item.Game
		repoDTO := toRepoDTO(item.Repo)
		// 用户目录只暴露挂载所需信息，不向普通用户下发管理备注/客户端配置。
		repoDTO.Note = ""
		repoDTO.ClientConfig = nil
		dto := catalogGameDTO{
			ID: game.ID, GroupID: game.GroupID, AppID: game.AppID, AppName: game.AppName,
			BuildID: game.BuildID, AvailableBuildID: game.AvailableBuildID,
			TargetPlatform: game.TargetPlatform, Status: game.Status,
			ProgressPercent: item.Progress.Percent, ProgressPhase: item.Progress.Phase,
			ProgressStage: item.Progress.Stage, ProgressBytesDone: item.Progress.BytesDone,
			ProgressBytesTotal: item.Progress.BytesTotal, ProgressSpeed: item.Progress.SpeedBytesPerSec,
			LastUpdatedAt: game.LastUpdatedAt, Repo: repoDTO,
		}
		if game.RepoID != nil {
			dto.RepoID = *game.RepoID
		}
		group.Games = append(group.Games, dto)
	}
	out := make([]catalogGroupDTO, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
