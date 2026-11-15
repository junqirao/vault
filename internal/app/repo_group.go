package app

import (
	"context"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// RepoGroupService 管理服务端分组。Steam 组绑定内容账号、下载平台与自动更新配置。
type RepoGroupService struct {
	Deps
	Accounts *SteamAccountService
}

// CreateRepoGroupInput 是创建分组的入参。
type CreateRepoGroupInput struct {
	Name           string
	Kind           domain.RepoGroupKind
	Description    string
	SteamAccountID *string
	TargetPlatform string
	AutoUpdate     *bool
	UpdateSchedule string
	UpdateWindow   string
	MaxConcurrent  int
	StorageID      string
	AccessMode     domain.RepoGroupAccessMode
	MemberIDs      []string
}

// UpdateRepoGroupInput 是更新分组的入参；指针为 nil 表示不修改。
type UpdateRepoGroupInput struct {
	ID             string
	Name           *string
	Kind           *domain.RepoGroupKind
	Description    *string
	SteamAccountID *string
	TargetPlatform *string
	AutoUpdate     *bool
	UpdateSchedule *string
	UpdateWindow   *string
	MaxConcurrent  *int
	StorageID      *string
	AccessMode     *domain.RepoGroupAccessMode
	MemberIDs      *[]string
}

// Create 创建分组。
func (s *RepoGroupService) Create(ctx context.Context, in CreateRepoGroupInput, operatorID string) (*domain.RepoGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, apperr.InvalidParam("name")
	}
	kind := in.Kind
	if kind == "" {
		kind = domain.RepoGroupGeneral
	}
	if !kind.Valid() {
		return nil, apperr.InvalidParam("kind")
	}
	platform := steamPlatformOrDefault(in.TargetPlatform)
	if platform == "" {
		return nil, apperr.InvalidParam("target_platform")
	}
	// Steam 分组默认自动更新；显式传 false 时尊重调用方。
	autoUpdate := false
	if in.AutoUpdate != nil {
		autoUpdate = *in.AutoUpdate
	} else if kind == domain.RepoGroupSteam {
		autoUpdate = true
	}
	g := &domain.RepoGroup{
		Name:           name,
		Kind:           kind,
		Description:    strings.TrimSpace(in.Description),
		TargetPlatform: platform,
		AutoUpdate:     autoUpdate,
		UpdateSchedule: strings.TrimSpace(in.UpdateSchedule),
		UpdateWindow:   strings.TrimSpace(in.UpdateWindow),
		MaxConcurrent:  in.MaxConcurrent,
		StorageID:      strings.TrimSpace(in.StorageID),
		AccessMode:     in.AccessMode,
	}
	if g.AccessMode == "" {
		g.AccessMode = domain.RepoGroupAccessAll
	}
	if !g.AccessMode.Valid() {
		return nil, apperr.InvalidParam("access_mode")
	}
	if g.MaxConcurrent <= 0 {
		g.MaxConcurrent = 1
	}
	if kind == domain.RepoGroupSteam {
		if err := s.validateAccount(ctx, in.SteamAccountID); err != nil {
			return nil, err
		}
		if err := s.ValidatePlatform(platform); err != nil {
			return nil, err
		}
		g.SteamAccountID = in.SteamAccountID
	}
	if err := s.Store.CreateRepoGroup(ctx, g); err != nil {
		return nil, err
	}
	if g.AccessMode == domain.RepoGroupAccessSelected {
		if err := s.Store.ReplaceRepoGroupMembers(ctx, g.ID, in.MemberIDs); err != nil {
			return nil, err
		}
	}
	s.audit(ctx, operatorID, "group.create", "repo_group:"+g.ID,
		"name="+g.Name+" kind="+string(g.Kind), domain.AuditResultOK)
	return g, nil
}

// List 列出分组。
func (s *RepoGroupService) List(ctx context.Context, kind domain.RepoGroupKind) ([]domain.RepoGroup, error) {
	return s.Store.ListRepoGroups(ctx, kind)
}

// Get 按 ID 查询分组。
func (s *RepoGroupService) Get(ctx context.Context, id string) (*domain.RepoGroup, error) {
	return s.Store.GetRepoGroup(ctx, id)
}

// GetByName 按名称查询分组。
func (s *RepoGroupService) GetByName(ctx context.Context, name string) (*domain.RepoGroup, error) {
	return s.Store.GetRepoGroupByName(ctx, strings.TrimSpace(name))
}

// Update 更新分组。
func (s *RepoGroupService) Update(ctx context.Context, in UpdateRepoGroupInput, operatorID string) (*domain.RepoGroup, error) {
	g, err := s.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	oldName := g.Name
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return nil, apperr.InvalidParam("name")
		}
		g.Name = strings.TrimSpace(*in.Name)
	}
	if in.Kind != nil {
		if !in.Kind.Valid() {
			return nil, apperr.InvalidParam("kind")
		}
		g.Kind = *in.Kind
	}
	if in.Description != nil {
		g.Description = strings.TrimSpace(*in.Description)
	}
	if in.TargetPlatform != nil {
		p := steamPlatformOrDefault(*in.TargetPlatform)
		if p == "" {
			return nil, apperr.InvalidParam("target_platform")
		}
		g.TargetPlatform = p
	}
	if in.AutoUpdate != nil {
		g.AutoUpdate = *in.AutoUpdate
	}
	if in.UpdateSchedule != nil {
		g.UpdateSchedule = strings.TrimSpace(*in.UpdateSchedule)
	}
	if in.UpdateWindow != nil {
		g.UpdateWindow = strings.TrimSpace(*in.UpdateWindow)
	}
	if in.MaxConcurrent != nil {
		if *in.MaxConcurrent <= 0 {
			return nil, apperr.InvalidParam("max_concurrent")
		}
		g.MaxConcurrent = *in.MaxConcurrent
	}
	if in.StorageID != nil {
		g.StorageID = strings.TrimSpace(*in.StorageID)
	}
	if in.AccessMode != nil {
		if !in.AccessMode.Valid() {
			return nil, apperr.InvalidParam("access_mode")
		}
		g.AccessMode = *in.AccessMode
	}
	if in.SteamAccountID != nil {
		g.SteamAccountID = in.SteamAccountID
	}
	if g.Kind == domain.RepoGroupSteam {
		if err := s.validateAccount(ctx, g.SteamAccountID); err != nil {
			return nil, err
		}
		if err := s.ValidatePlatform(g.TargetPlatform); err != nil {
			return nil, err
		}
	} else {
		g.SteamAccountID = nil
		g.AutoUpdate = false
	}
	if err := s.Store.UpdateRepoGroup(ctx, g); err != nil {
		return nil, err
	}
	if in.MemberIDs != nil {
		if err := s.Store.ReplaceRepoGroupMembers(ctx, g.ID, *in.MemberIDs); err != nil {
			return nil, err
		}
	} else if g.AccessMode == domain.RepoGroupAccessAll {
		// 改成"全部可访问"时清空旧成员，避免以后切回 selected 时残留旧名单。
		if err := s.Store.ReplaceRepoGroupMembers(ctx, g.ID, nil); err != nil {
			return nil, err
		}
	}
	if oldName != g.Name {
		// 存储库的组名仍写在 meta.group 里（客户端分组展示 / 本机挂载配置按组名索引），
		// 组改名时必须同步，否则库会"留在旧组"。
		if err := s.Store.RenameRepoGroupLabel(ctx, oldName, g.Name); err != nil {
			s.Log.Warn("分组改名后同步存储库组名失败", "group_id", g.ID, "old", oldName, "new", g.Name, "error", err)
		}
	}
	s.audit(ctx, operatorID, "group.update", "repo_group:"+g.ID,
		"name="+g.Name+" kind="+string(g.Kind), domain.AuditResultOK)
	return g, nil
}

// Delete 删除分组。组内仍有游戏时拒绝，避免把游戏记录级联删掉。
func (s *RepoGroupService) Delete(ctx context.Context, id, operatorID string) error {
	games, err := s.Store.ListGamesByGroup(ctx, id)
	if err != nil {
		return err
	}
	if len(games) > 0 {
		return apperr.New("group.has_games", 409).WithArg("count", len(games))
	}
	if err := s.Store.DeleteRepoGroup(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, operatorID, "group.delete", "repo_group:"+id, "", domain.AuditResultOK)
	return nil
}

// EnsureForRepo 在创建/更新存储库时保证分组实体存在。
//
// 现有存储库的 `meta.group` 只是一个自由文本标签；为了让分组设置能保存 Steam 配置，
// 这里把首次出现的组名登记为 general 分组。若该名字已经是一个 Steam 组，则保持原配置。
func (s *RepoGroupService) EnsureForRepo(ctx context.Context, repo *domain.Repository, groupName string) error {
	groupName = strings.TrimSpace(groupName)
	if repo == nil || groupName == "" {
		return nil
	}
	if _, err := s.Store.GetRepoGroupByName(ctx, groupName); err == nil {
		return nil
	} else if !isNotFound(err) {
		return err
	}
	g := &domain.RepoGroup{
		Name:           groupName,
		Kind:           domain.RepoGroupGeneral,
		TargetPlatform: "windows",
		MaxConcurrent:  1,
	}
	return s.Store.CreateRepoGroup(ctx, g)
}

// ListMembers 列出分组成员。
func (s *RepoGroupService) ListMembers(ctx context.Context, groupID string) ([]domain.RepoGroupMember, error) {
	return s.Store.ListRepoGroupMembers(ctx, groupID)
}

// CanAccess 判断用户是否可以访问该分组下的游戏库。
//
// - all：任意登录用户可访问；
// - selected：仅分组成员可访问；超级管理员始终可访问。
func (s *RepoGroupService) CanAccess(ctx context.Context, group *domain.RepoGroup, userID string, isSuperAdmin bool) (bool, error) {
	if group == nil {
		return false, apperr.New("group.not_found", 404)
	}
	if isSuperAdmin || group.AccessMode == "" || group.AccessMode == domain.RepoGroupAccessAll {
		return true, nil
	}
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	return s.Store.IsRepoGroupMember(ctx, group.ID, userID)
}

func (s *RepoGroupService) validateAccount(ctx context.Context, accountID *string) error {
	if accountID == nil || strings.TrimSpace(*accountID) == "" {
		return apperr.InvalidParam("steam_account_id")
	}
	a, err := s.Store.GetSteamAccount(ctx, strings.TrimSpace(*accountID))
	if err != nil {
		return err
	}
	if a.Status == domain.SteamAccountStatusDisabled {
		return apperr.New("steam.account_disabled", 409).WithArg("account_id", a.ID)
	}
	return nil
}

// ValidatePlatform 校验服务端是否有能力承载目标平台的游戏库。
//
// Linux 服务端可以承载 Windows（SteamCMD 强制平台 + NTFS 差异盘）和 Linux；
// Windows 服务端不能挂载 ext4，因此不支持 Linux 目标平台。
func (s *RepoGroupService) ValidatePlatform(platform string) error {
	p := steamPlatformOrDefault(platform)
	if p == "" {
		return apperr.InvalidParam("target_platform")
	}
	if string(s.platformKind()) == "windows" && p == "linux" {
		return apperr.New("steam.platform_unsupported", 409).
			WithArg("server", "windows").WithArg("target", p)
	}
	return nil
}

// EffectiveAccountID 返回游戏实际使用的账号：游戏覆盖 > 分组绑定。
func (s *RepoGroupService) EffectiveAccountID(g *domain.RepoGroup, game *domain.Game) string {
	if game != nil && game.SteamAccountID != nil && strings.TrimSpace(*game.SteamAccountID) != "" {
		return strings.TrimSpace(*game.SteamAccountID)
	}
	if g != nil && g.SteamAccountID != nil {
		return strings.TrimSpace(*g.SteamAccountID)
	}
	return ""
}

// EffectivePlatform 返回游戏实际下载平台：游戏覆盖 > 分组配置 > windows。
func (s *RepoGroupService) EffectivePlatform(g *domain.RepoGroup, game *domain.Game) string {
	if game != nil && strings.TrimSpace(game.TargetPlatform) != "" {
		if p := steamPlatformOrDefault(game.TargetPlatform); p != "" {
			return p
		}
	}
	if g != nil && strings.TrimSpace(g.TargetPlatform) != "" {
		if p := steamPlatformOrDefault(g.TargetPlatform); p != "" {
			return p
		}
	}
	return "windows"
}

// EffectiveAutoUpdate 返回游戏是否参与自动更新：游戏覆盖 > 分组配置。
func (s *RepoGroupService) EffectiveAutoUpdate(g *domain.RepoGroup, game *domain.Game) bool {
	if game != nil && game.AutoUpdate != nil {
		return *game.AutoUpdate
	}
	return g != nil && g.AutoUpdate
}
