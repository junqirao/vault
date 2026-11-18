package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// SteamGameService 负责"一个 AppID ↔ 一个共享存储库"的游戏记录与内容更新。
//
// 更新流程完全复用现有能力：
//
//	进入维护态 → SteamCMD 更新到暂存目录 → StartDiffFromDir → MergeDiff
//
// 它不直接操作 VHDX/iSCSI，也不持有 RepoKey；子任务由现有 job worker 执行。
type SteamGameService struct {
	Deps
	Repos    *RepoService
	Disks    *DiskService
	Groups   *RepoGroupService
	Accounts *SteamAccountService

	sem     chan struct{}
	semOnce sync.Once

	progressMu  sync.RWMutex
	progress    map[string]SteamGameProgress
	cancels     map[string]context.CancelFunc
	cancelModes map[string]string
	outputs     map[string][]string
}

// SteamGameProgress 是一次更新任务的实时进度（仅内存，不落库）。
type SteamGameProgress struct {
	Phase      string  `json:"phase"`
	Stage      string  `json:"stage"`
	Percent    float64 `json:"percent"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	// SpeedBytesPerSec 由 SteamCMD 输出的字节增量按时间换算，SteamCMD 本身不打印速度。
	SpeedBytesPerSec int64 `json:"speed_bytes_per_sec"`
	CanStop          bool  `json:"can_stop"`
	UpdatedAt        int64 `json:"updated_at"`

	// speedAt / speedBytes 只用于采样，不对外暴露。
	speedAt    time.Time
	speedBytes int64
}

// Steam 更新控制模式。
const (
	SteamUpdateControlPause = "pause"
	SteamUpdateControlStop  = "stop"
)

// NewSteamGameService 构造游戏服务并初始化全局更新并发闸门。
func NewSteamGameService(d Deps, repos *RepoService, disks *DiskService, groups *RepoGroupService, accounts *SteamAccountService) *SteamGameService {
	n := 2
	if raw := d.raw(); raw.Steam.MaxConcurrentUpdates > 0 {
		n = raw.Steam.MaxConcurrentUpdates
	}
	return &SteamGameService{
		Deps: d, Repos: repos, Disks: disks, Groups: groups, Accounts: accounts,
		sem:         make(chan struct{}, n),
		progress:    make(map[string]SteamGameProgress),
		cancels:     make(map[string]context.CancelFunc),
		cancelModes: make(map[string]string),
		outputs:     make(map[string][]string),
	}
}

func (s *SteamGameService) setProgress(gameID, phase string, percent float64) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	s.progressMu.Lock()
	s.progress[gameID] = SteamGameProgress{
		Phase: phase, Percent: percent, UpdatedAt: time.Now().UnixMilli(),
	}
	s.progressMu.Unlock()
}

// setDownloadProgress 合并 SteamCMD 的单行进度：百分比 + 字节 + 速度。
func (s *SteamGameService) setDownloadProgress(gameID string, sample steamDownloadSample) {
	now := time.Now()
	if sample.Percent < 0 {
		sample.Percent = 0
	}
	if sample.Percent > 100 {
		sample.Percent = 100
	}

	s.progressMu.Lock()
	p := s.progress[gameID]
	if p.Phase == "" {
		p.Phase = "downloading"
	}
	if sample.Stage != "" {
		p.Stage = sample.Stage
	}
	p.Percent = sample.Percent
	p.BytesDone = sample.BytesDone
	p.BytesTotal = sample.BytesTotal
	if sample.BytesTotal == 0 {
		// reconfiguring / validating 等 0/0 状态没有下载速度可言。
		p.SpeedBytesPerSec = 0
		p.speedAt = time.Time{}
		p.speedBytes = 0
	}
	if sample.BytesDone > 0 {
		switch {
		case p.speedAt.IsZero():
			p.speedAt = now
			p.speedBytes = sample.BytesDone
		case now.Sub(p.speedAt) >= time.Second:
			elapsed := now.Sub(p.speedAt).Seconds()
			delta := sample.BytesDone - p.speedBytes
			if elapsed > 0 && delta >= 0 {
				instant := float64(delta) / elapsed
				if p.SpeedBytesPerSec > 0 {
					p.SpeedBytesPerSec = int64(0.7*float64(p.SpeedBytesPerSec) + 0.3*instant)
				} else {
					p.SpeedBytesPerSec = int64(instant)
				}
			} else {
				p.SpeedBytesPerSec = 0
			}
			p.speedAt = now
			p.speedBytes = sample.BytesDone
		}
	}
	p.UpdatedAt = now.UnixMilli()
	s.progress[gameID] = p
	s.progressMu.Unlock()
}

// resetOutput 在每次更新开始时清空上一轮的 SteamCMD 输出。
func (s *SteamGameService) resetOutput(gameID string) {
	s.progressMu.Lock()
	s.outputs[gameID] = nil
	s.progressMu.Unlock()
}

const (
	steamGameOutputMaxLines = 400
	steamGameOutputMaxLine  = 2000
)

// appendOutput 保留最近一段 SteamCMD 原始输出，供管理端排查/直接查看。
func (s *SteamGameService) appendOutput(gameID, line string) {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return
	}
	if len(line) > steamGameOutputMaxLine {
		line = line[:steamGameOutputMaxLine]
	}
	s.progressMu.Lock()
	buf := append(s.outputs[gameID], line)
	if len(buf) > steamGameOutputMaxLines {
		buf = buf[len(buf)-steamGameOutputMaxLines:]
	}
	s.outputs[gameID] = buf
	s.progressMu.Unlock()
}

// Output 返回该游戏最近一次/当前更新的 SteamCMD 输出（只读副本）。
func (s *SteamGameService) Output(gameID string) []string {
	s.progressMu.RLock()
	buf := s.outputs[gameID]
	out := append([]string(nil), buf...)
	s.progressMu.RUnlock()
	return out
}

// Progress 返回指定游戏的实时更新进度；没有任务时返回零值。
func (s *SteamGameService) Progress(gameID string) SteamGameProgress {
	s.progressMu.RLock()
	p := s.progress[gameID]
	_, running := s.cancels[gameID]
	s.progressMu.RUnlock()
	if p.Phase == "" {
		return SteamGameProgress{}
	}
	p.CanStop = running && (p.Phase == "checking" || p.Phase == "downloading")
	// 10 秒没有新字节就把速度归零，避免界面一直显示上一次的瞬时值。
	if p.SpeedBytesPerSec > 0 && time.Since(time.UnixMilli(p.UpdatedAt)) > 10*time.Second {
		p.SpeedBytesPerSec = 0
	}
	return p
}

func (s *SteamGameService) clearProgress(gameID string) {
	s.progressMu.Lock()
	delete(s.progress, gameID)
	s.progressMu.Unlock()
}

func (s *SteamGameService) registerCancel(gameID string, cancel context.CancelFunc) {
	s.progressMu.Lock()
	s.cancels[gameID] = cancel
	delete(s.cancelModes, gameID)
	s.progressMu.Unlock()
}

func (s *SteamGameService) unregisterCancel(gameID string) {
	s.progressMu.Lock()
	delete(s.cancels, gameID)
	delete(s.cancelModes, gameID)
	s.progressMu.Unlock()
}

func (s *SteamGameService) setCancelMode(gameID, mode string) {
	s.progressMu.Lock()
	s.cancelModes[gameID] = mode
	s.progressMu.Unlock()
}

func (s *SteamGameService) takeCancelMode(gameID string) string {
	s.progressMu.Lock()
	mode := s.cancelModes[gameID]
	delete(s.cancelModes, gameID)
	s.progressMu.Unlock()
	return mode
}

// CreateGameInput 是创建游戏记录的入参。
type CreateGameInput struct {
	GroupID string
	AppID   int64
	AppName string
	RepoID  string
	// RepoName 为空时按 App 名称/AppID 自动生成。
	RepoName   string
	InstallDir string
	// CreateRepo 为 true 且 RepoID 为空时，自动创建一个 shared 存储库。
	CreateRepo bool
	SizeBytes  int64
	StorageID  string
	ShareCount int
	// TargetPlatform / SteamAccountID / AutoUpdate 为空表示继承分组。
	TargetPlatform string
	SteamAccountID *string
	AutoUpdate     *bool
	// SkipLicenseCheck 用于客户端本机 Steam 库批量添加：客户端列表可能比服务端
	// 许可缓存更新（或涉及家庭共享），此时不在建库前硬拒绝，交给 SteamCMD 下载时校验。
	SkipLicenseCheck bool
}

// UpdateGameInput 是更新游戏记录的入参；指针为 nil 表示不修改。
type UpdateGameInput struct {
	ID             string
	AppName        *string
	InstallDir     *string
	TargetPlatform *string
	SteamAccountID *string
	AutoUpdate     *bool
}

// CreateGame 创建游戏。默认一个 AppID 只对应一个共享库。
func (s *SteamGameService) CreateGame(ctx context.Context, in CreateGameInput, operatorID string) (*domain.Game, error) {
	if in.AppID <= 0 {
		return nil, apperr.InvalidParam("app_id")
	}
	group, err := s.Groups.Get(ctx, in.GroupID)
	if err != nil {
		return nil, err
	}
	if !group.IsSteam() {
		return nil, apperr.New("group.not_steam", 409).WithArg("group_id", group.ID)
	}
	platform := s.Groups.EffectivePlatform(group, nil)
	if in.TargetPlatform != "" {
		p := steamPlatformOrDefault(in.TargetPlatform)
		if p == "" {
			return nil, apperr.InvalidParam("target_platform")
		}
		platform = p
	}
	if err := s.Groups.ValidatePlatform(platform); err != nil {
		return nil, err
	}

	// 账号可继承分组，也可在游戏级覆盖。先做一次轻量许可校验：
	// 只有许可清单已同步且明确不包含该 AppID 时才拒绝；空清单视为"尚未同步"。
	accountID := s.Groups.EffectiveAccountID(group, &domain.Game{
		SteamAccountID: in.SteamAccountID,
	})
	if accountID != "" && !in.SkipLicenseCheck {
		apps, aErr := s.Store.ListSteamAccountApps(ctx, accountID)
		if aErr == nil && len(apps) > 0 {
			owned := false
			for _, app := range apps {
				if app.AppID == in.AppID {
					owned = true
					break
				}
			}
			if !owned {
				return nil, apperr.New("steam.account_no_license", 409).
					WithArg("account_id", accountID).WithArg("app_id", in.AppID)
			}
		}
	}

	repoID := strings.TrimSpace(in.RepoID)
	if repoID == "" {
		if !in.CreateRepo {
			return nil, apperr.InvalidParam("repo_id")
		}
		sizeBytes := in.SizeBytes
		if sizeBytes <= 0 {
			sizeGB := s.raw().Steam.DefaultRepoSizeGB
			if sizeGB <= 0 {
				sizeGB = 60
			}
			sizeBytes = int64(sizeGB) << 30
		}
		name := strings.TrimSpace(in.RepoName)
		if name == "" {
			name = defaultGameRepoName(in.AppName, in.AppID)
		}
		storageID := strings.TrimSpace(in.StorageID)
		if storageID == "" {
			storageID = strings.TrimSpace(group.StorageID)
		}
		repo, _, err := s.Repos.Create(ctx, CreateRepoInput{
			Name:         name,
			Mode:         domain.RepoModeShared,
			OwnerID:      operatorID,
			SizeBytes:    sizeBytes,
			MaxDiffDisks: in.ShareCount,
			Preallocate:  false,
			Group:        group.Name,
			StorageID:    storageID,
			ClientOS:     platformToClientOS(platform),
		})
		if err != nil {
			return nil, err
		}
		repoID = repo.ID
	} else {
		repo, err := s.Repos.Get(ctx, repoID)
		if err != nil {
			return nil, err
		}
		if !repo.IsShared() {
			return nil, apperr.New("repo.not_shared", 409).WithArg("mode", string(repo.Mode))
		}
	}

	if _, err := s.Store.GetGameByRepoID(ctx, repoID); err == nil {
		return nil, apperr.New("game.repo_taken", 409).WithArg("repo_id", repoID)
	} else if !isNotFound(err) {
		return nil, err
	}
	// 一个 AppID 只对应一个游戏库：避免管理员重复添加导致同一游戏多份母盘。
	if all, err := s.Store.ListAllGames(ctx); err == nil {
		for i := range all {
			if all[i].AppID == in.AppID {
				return nil, apperr.New("game.app_taken", 409).
					WithArg("app_id", in.AppID).WithArg("game_id", all[i].ID)
			}
		}
	} else {
		return nil, err
	}

	game := &domain.Game{
		GroupID:        group.ID,
		RepoID:         &repoID,
		AppID:          in.AppID,
		AppName:        strings.TrimSpace(in.AppName),
		InstallDir:     strings.TrimSpace(in.InstallDir),
		TargetPlatform: strings.TrimSpace(in.TargetPlatform),
		SteamAccountID: in.SteamAccountID,
		AutoUpdate:     in.AutoUpdate,
		Status:         domain.GameStatusReady,
	}
	if game.AppName == "" {
		game.AppName = fmt.Sprintf("App %d", game.AppID)
	}
	if err := s.Store.CreateGame(ctx, game); err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "game.create", "game:"+game.ID,
		"app_id="+strconv.FormatInt(game.AppID, 10)+" repo_id="+repoID, domain.AuditResultOK)
	s.emit("game", map[string]any{"action": "created", "game_id": game.ID, "repo_id": repoID})
	return game, nil
}

// ListGames 列出分组内游戏。
func (s *SteamGameService) ListGames(ctx context.Context, groupID string) ([]domain.Game, error) {
	return s.Store.ListGamesByGroup(ctx, groupID)
}

// GetGame 按 ID 查询游戏。
func (s *SteamGameService) GetGame(ctx context.Context, id string) (*domain.Game, error) {
	return s.Store.GetGame(ctx, id)
}

// UpdateGame 更新游戏可变字段。
func (s *SteamGameService) UpdateGame(ctx context.Context, in UpdateGameInput, operatorID string) (*domain.Game, error) {
	g, err := s.GetGame(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if in.AppName != nil {
		g.AppName = strings.TrimSpace(*in.AppName)
	}
	if in.InstallDir != nil {
		g.InstallDir = strings.TrimSpace(*in.InstallDir)
	}
	if in.TargetPlatform != nil {
		if *in.TargetPlatform != "" {
			p := steamPlatformOrDefault(*in.TargetPlatform)
			if p == "" {
				return nil, apperr.InvalidParam("target_platform")
			}
			if err := s.Groups.ValidatePlatform(p); err != nil {
				return nil, err
			}
			g.TargetPlatform = p
		} else {
			g.TargetPlatform = ""
		}
	}
	if in.SteamAccountID != nil {
		if *in.SteamAccountID != "" {
			if _, err := s.Store.GetSteamAccount(ctx, *in.SteamAccountID); err != nil {
				return nil, err
			}
			v := *in.SteamAccountID
			g.SteamAccountID = &v
		} else {
			g.SteamAccountID = nil
		}
	}
	if in.AutoUpdate != nil {
		v := *in.AutoUpdate
		g.AutoUpdate = &v
	}
	if err := s.Store.UpdateGame(ctx, g); err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "game.update", "game:"+g.ID, "app_id="+strconv.FormatInt(g.AppID, 10), domain.AuditResultOK)
	return g, nil
}

// DeleteGame 删除游戏记录，不删除存储库本身。
func (s *SteamGameService) DeleteGame(ctx context.Context, id, operatorID string) error {
	if err := s.Store.DeleteGame(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, operatorID, "game.delete", "game:"+id, "", domain.AuditResultOK)
	return nil
}

// GameCatalogItem 是面向普通用户的游戏目录项：组 + 游戏 + 可挂载的共享库 + 实时更新进度。
type GameCatalogItem struct {
	Group    *domain.RepoGroup
	Game     *domain.Game
	Repo     *domain.Repository
	Progress SteamGameProgress
}

// Catalog 返回当前用户可见的 Steam 游戏库。
//
// 这是普通用户视角：只暴露游戏与共享库，不暴露 Steam 账号、凭据或服务端暂存路径。
// 分组 access_mode=selected 时只返回该用户是成员的分组；超级管理员看全部。
func (s *SteamGameService) Catalog(ctx context.Context, userID string, isSuperAdmin bool) ([]GameCatalogItem, error) {
	groups, err := s.Groups.List(ctx, domain.RepoGroupSteam)
	if err != nil {
		return nil, err
	}
	out := make([]GameCatalogItem, 0)
	for i := range groups {
		g := &groups[i]
		allowed, err := s.Groups.CanAccess(ctx, g, userID, isSuperAdmin)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		games, err := s.Store.ListGamesByGroup(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for j := range games {
			game := &games[j]
			if game.RepoID == nil || strings.TrimSpace(*game.RepoID) == "" {
				continue
			}
			repo, err := s.Repos.Get(ctx, *game.RepoID)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, err
			}
			if !repo.IsShared() {
				continue
			}
			out = append(out, GameCatalogItem{
				Group: g, Game: game, Repo: repo, Progress: s.Progress(game.ID),
			})
		}
	}
	return out, nil
}

// IsGameRepo 判断某个存储库是否是当前用户可访问的 Steam 共享游戏库。
//
// 用于给普通用户开放"共享游戏库可自由挂载"的最小权限：只有 games 表引用、
// 且分组 access_mode 允许该用户的库才放行；普通存储库的权限模型不变。
func (s *SteamGameService) IsGameRepo(ctx context.Context, repoID, userID string, isSuperAdmin bool) (bool, error) {
	if strings.TrimSpace(repoID) == "" {
		return false, nil
	}
	game, err := s.Store.GetGameByRepoID(ctx, repoID)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if game.RepoID == nil {
		return false, nil
	}
	group, err := s.Groups.Get(ctx, game.GroupID)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	allowed, err := s.Groups.CanAccess(ctx, group, userID, isSuperAdmin)
	if err != nil || !allowed {
		return false, err
	}
	repo, err := s.Repos.Get(ctx, *game.RepoID)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return repo.IsShared(), nil
}

// CheckGame 检查一个游戏是否有新版本，并记录 available_build_id。
func (s *SteamGameService) CheckGame(ctx context.Context, gameID string) (*domain.Game, error) {
	game, group, repo, account, err := s.loadGameContext(ctx, gameID)
	if err != nil {
		return nil, err
	}
	_ = repo
	if err := steamAccountUsableForUpdate(account); err != nil {
		return nil, err
	}
	password, guard, err := s.Accounts.credentials(account, "")
	if err != nil {
		return nil, err
	}
	platform := s.Groups.EffectivePlatform(group, game)
	if err := s.Groups.ValidatePlatform(platform); err != nil {
		return nil, err
	}
	lease, err := s.Accounts.acquire(ctx, account, "game_check", gameID, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	defer s.Accounts.release(ctx, lease)

	buildID, err := s.steamCheckAppInfo(ctx, s.Accounts.sessionHomeFor(account.ID),
		account.Username, password, guard, platform, game.AppID)
	now := time.Now().UnixMilli()
	game.LastCheckedAt = now
	if err != nil {
		game.LastError = apperr.CodeOf(err)
		s.Accounts.HandleAuthError(ctx, account, err)
		_ = s.Store.UpdateGame(ctx, game)
		return game, err
	}
	if buildID != "" {
		game.AvailableBuildID = buildID
	}
	game.LastError = ""
	if err := s.Store.UpdateGame(ctx, game); err != nil {
		return nil, err
	}
	return game, nil
}

// CheckGroup 检查组内所有游戏。
func (s *SteamGameService) CheckGroup(ctx context.Context, groupID string) ([]domain.Game, error) {
	games, err := s.ListGames(ctx, groupID)
	if err != nil {
		return nil, err
	}
	for i := range games {
		updated, checkErr := s.CheckGame(ctx, games[i].ID)
		if checkErr != nil {
			games[i].LastError = apperr.CodeOf(checkErr)
			continue
		}
		games[i] = *updated
	}
	return games, nil
}

// TriggerUpdate 异步触发一次游戏内容更新。
//
// 账号模块只在这里被用于"存储库下载"；没有任何玩家登录/通用命令入口。
func (s *SteamGameService) TriggerUpdate(ctx context.Context, gameID, operatorID string) (*domain.Game, error) {
	game, group, repo, account, err := s.loadGameContext(ctx, gameID)
	if err != nil {
		return nil, err
	}
	if !group.IsSteam() {
		return nil, apperr.New("group.not_steam", 409)
	}
	if repo.Condition() != domain.ParentIdle && repo.Condition() != domain.ParentMaintenance {
		return nil, apperr.New("steam.repo_state_invalid", 409).WithArg("condition", string(repo.Condition()))
	}
	if game.Status == domain.GameStatusUpdating {
		return nil, apperr.New("steam.game_updating", 409).WithArg("game_id", game.ID)
	}
	active, err := s.Store.CountActiveLeasesByRepo(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, apperr.New("steam.repo_busy", 409).WithArg("leases", active)
	}
	if err := steamAccountUsableForUpdate(account); err != nil {
		return nil, err
	}

	game.Status = domain.GameStatusUpdating
	game.LastError = ""
	if err := s.Store.UpdateGame(ctx, game); err != nil {
		return nil, err
	}
	s.emit("game", map[string]any{"action": "updating", "game_id": game.ID})

	updateCtx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	s.registerCancel(game.ID, cancel)
	s.resetOutput(game.ID)
	s.setProgress(game.ID, "checking", 0)
	go s.runUpdate(updateCtx, cancel, game.ID, operatorID)
	return game, nil
}

func (s *SteamGameService) runUpdate(ctx context.Context, cancel context.CancelFunc, gameID, operatorID string) {
	var staging string
	s.semOnce.Do(func() {
		if s.sem == nil {
			n := 2
			if raw := s.raw(); raw.Steam.MaxConcurrentUpdates > 0 {
				n = raw.Steam.MaxConcurrentUpdates
			}
			s.sem = make(chan struct{}, n)
		}
	})
	s.sem <- struct{}{}
	defer func() {
		<-s.sem
		if r := recover(); r != nil {
			s.Log.Error("Steam 游戏更新 panic", "game_id", gameID, "panic", r)
			s.markGameError(context.Background(), gameID, "steam.update_panic")
		}
	}()
	defer cancel()
	// 注意顺序：cleanup 必须在 cancel 之后注册（先执行），这样它能读到用户取消的 ctx.Err()。
	defer func() { s.cleanupUpdate(ctx, gameID, staging) }()

	game, group, repo, account, err := s.loadGameContext(ctx, gameID)
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	platform := s.Groups.EffectivePlatform(group, game)
	if err := s.Groups.ValidatePlatform(platform); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	password, guard, err := s.Accounts.credentials(account, "")
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	lease, err := s.Accounts.acquire(ctx, account, "game_update", gameID, 12*time.Hour)
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	defer s.Accounts.release(ctx, lease)

	// 1) 检查版本：已是最新则只更新检查时间。
	buildID, err := s.steamCheckAppInfo(ctx, s.Accounts.sessionHomeFor(account.ID),
		account.Username, password, guard, platform, game.AppID)
	if err != nil {
		s.Accounts.HandleAuthError(ctx, account, err)
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	now := time.Now().UnixMilli()
	if buildID != "" {
		game.AvailableBuildID = buildID
	}
	game.LastCheckedAt = now
	if buildID != "" && game.BuildID != "" && buildID == game.BuildID {
		game.Status = domain.GameStatusReady
		game.LastError = ""
		_ = s.Store.UpdateGame(ctx, game)
		s.emit("game", map[string]any{"action": "up_to_date", "game_id": game.ID})
		return
	}

	// 2) 等存储库建好（新游戏库可能还在 creating）。
	if err := s.waitRepoActive(ctx, repo.ID, 30*time.Minute); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}

	// 3) 进入维护态。已处于维护态则复用（例如上一次失败后人工排查）。
	maintenanceEntered := false
	if repo.Condition() != domain.ParentMaintenance {
		if _, err := s.Repos.TransitionParent(ctx, repo.ID, ParentActionMaintenance, operatorID); err != nil {
			s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
			return
		}
		maintenanceEntered = true
	}
	// 3.5) 更新母盘前先清掉上一次失败留下的差异盘/任务。
	// 否则 StartDiffFromDir 会一直返回 repo.operation_in_progress（创建中的差异盘
	// 即使没有 active job 也会被当成进行中的操作）。
	if err := s.prepareSteamDiff(ctx, repo.ID, operatorID); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	merged := false
	defer func() {
		if maintenanceEntered && !merged {
			// 合并前失败：母盘未变，退出维护但**不自增版本**。
			if err := s.Repos.AbortMaintenance(context.WithoutCancel(ctx), repo.ID, operatorID); err != nil {
				s.Log.Warn("Steam 更新失败后退出维护态失败", "repo_id", repo.ID, "error", err)
			}
		}
	}()

	// 4) SteamCMD 更新到暂存目录。
	staging, err = s.stagingDirFor(ctx, game)
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		s.markUpdateFailure(ctx, gameID, "steam.staging_dir_failed")
		return
	}
	s.setProgress(game.ID, "downloading", 0)
	out, updateErr := s.steamUpdateApp(ctx, s.Accounts.sessionHomeFor(account.ID),
		account.Username, password, guard, platform, game.AppID, staging, false,
		func(line string) {
			s.appendOutput(game.ID, line)
			if sample, ok := parseSteamDownloadDetail(line); ok {
				s.setDownloadProgress(game.ID, sample)
			}
		})
	completed := steamUpdateCompleted(out, game.AppID)
	if updateErr != nil && !completed {
		s.Accounts.HandleAuthError(ctx, account, updateErr)
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(updateErr))
		return
	}
	// 进程退出码为 0 不等于"安装完成"：必须看到 SteamCMD 自己打印的
	// Success! App '<id>' fully installed，并确认 staging 不再变化。
	if !completed {
		s.Log.Warn("SteamCMD 未报告安装完成",
			"game_id", game.ID, "app_id", game.AppID, "output", truncateSteamOutput(out))
		s.markUpdateFailure(ctx, gameID, "steam.update_incomplete")
		return
	}
	if updateErr != nil {
		s.Log.Warn("SteamCMD 退出码异常，但输出已报告安装完成",
			"game_id", game.ID, "app_id", game.AppID, "error", updateErr)
	}
	s.setProgress(game.ID, "preparing", 0)
	if err := s.waitStagingStable(ctx, staging, 2*time.Second, 5*time.Minute); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	if size, err := dirSize(staging); err == nil && size == 0 {
		// "Success! already up to date" 但暂存目录是空的：说明 SteamCMD 实际更新的是
		// 别的安装目录（典型是 +force_install_dir 顺序错误），不能拿空目录建差异盘。
		s.markUpdateFailure(ctx, gameID, "steam.update_incomplete")
		return
	}
	// 4.5) 自动扩容：游戏变大后，母盘容量可能不够。必须在创建差异盘之前扩容，
	// 因为差异盘容量会继承母盘；扩容要求先清掉旧差异盘（它们本来也会在 MergeDiff 时被删除）。
	if err := s.ensureParentCapacity(ctx, repo.ID, staging, operatorID); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	newBuild := steamACFBuildID(staging, game.AppID)
	if newBuild == "" {
		newBuild = buildID
	}
	if newBuild == "" {
		// SteamCMD 没写出可解析的 buildid（旧版本/异常输出）：记一个哨兵值，
		// 避免调度器每次都把同一个游戏判成"从未更新"而反复下载。
		newBuild = "unknown"
	}

	// 5) 生成临时差异盘并合并回母盘。完全复用现有任务链。
	s.setProgress(game.ID, "diffing", 0)
	diffJob, err := s.Disks.StartSteamUpdate(ctx, repo.ID, staging, "auto", true, operatorID)
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	if err := s.waitJob(ctx, diffJob.ID, 6*time.Hour); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	result, err := s.Disks.GetDiffSyncResult(ctx, repo.ID, diffJob.ID)
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	if result.DiffDiskID == "" {
		s.markUpdateFailure(ctx, gameID, "steam.diff_missing")
		return
	}
	s.setProgress(game.ID, "merging", 0)
	mergeJob, err := s.Repos.MergeDiff(ctx, repo.ID, result.DiffDiskID, operatorID)
	if err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}
	merged = true
	if err := s.waitJob(ctx, mergeJob.ID, 6*time.Hour); err != nil {
		s.markUpdateFailure(ctx, gameID, apperr.CodeOf(err))
		return
	}

	// 6) 收尾：记录版本/时间，并把成功更新过的 AppID 标成已验证许可。
	game.BuildID = newBuild
	game.LastUpdatedAt = time.Now().UnixMilli()
	game.LastCheckedAt = game.LastUpdatedAt
	game.Status = domain.GameStatusReady
	game.LastError = ""
	if err := s.Store.UpdateGame(ctx, game); err != nil {
		s.Log.Warn("更新游戏记录失败", "game_id", game.ID, "error", err)
	}
	_ = s.Store.UpsertSteamAccountApp(ctx, account.ID, game.AppID, "update_ok")
	s.audit(ctx, operatorID, "game.update.done", "game:"+game.ID,
		"app_id="+strconv.FormatInt(game.AppID, 10)+" build="+newBuild, domain.AuditResultOK)
	s.emit("game", map[string]any{"action": "updated", "game_id": game.ID, "build_id": newBuild})
}

func (s *SteamGameService) loadGameContext(ctx context.Context, gameID string) (*domain.Game, *domain.RepoGroup, *domain.Repository, *domain.SteamAccount, error) {
	game, err := s.Store.GetGame(ctx, gameID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	group, err := s.Groups.Get(ctx, game.GroupID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if game.RepoID == nil || strings.TrimSpace(*game.RepoID) == "" {
		return nil, nil, nil, nil, apperr.New("game.repo_missing", 409).WithArg("game_id", game.ID)
	}
	repo, err := s.Repos.Get(ctx, *game.RepoID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	accountID := s.Groups.EffectiveAccountID(group, game)
	if accountID == "" {
		return nil, nil, nil, nil, apperr.New("steam.account_missing", 409).WithArg("group_id", group.ID)
	}
	account, err := s.Store.GetSteamAccount(ctx, accountID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return game, group, repo, account, nil
}

// prepareSteamDiff 清理上一次失败留下的差异盘工具任务，保证本次更新可以创建新差异盘。
//
// 允许自动取消的只有 create_diff_from_dir / plan_client_diff；merge_diff 正在改写母盘，
// 不能强行取消，直接返回操作进行中让管理员处理。
func (s *SteamGameService) prepareSteamDiff(ctx context.Context, repoID, operatorID string) error {
	if job, err := s.Repos.ActiveDiffOperation(ctx, repoID); err != nil {
		return err
	} else if job != nil {
		switch job.Type {
		case domain.JobCreateDiffFromDir, domain.JobSteamUpdate, domain.JobPlanClientDiff:
			if err := s.Repos.CancelActiveDiffOperation(ctx, repoID); err != nil {
				return err
			}
			deadline := time.Now().Add(2 * time.Minute)
			for {
				current, err := s.Repos.ActiveDiffOperation(ctx, repoID)
				if err != nil {
					return err
				}
				if current == nil {
					break
				}
				if time.Now().After(deadline) {
					return apperr.New("repo.operation_in_progress", 409).
						WithArg("job_type", string(current.Type)).WithArg("job_id", current.ID)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
				}
			}
		default:
			return apperr.New("repo.operation_in_progress", 409).
				WithArg("job_type", string(job.Type)).WithArg("job_id", job.ID)
		}
	}

	disks, err := s.Store.ListDisksByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	hasDiff := false
	for i := range disks {
		if disks[i].Kind == domain.DiskKindDiff {
			hasDiff = true
			break
		}
	}
	if hasDiff {
		job, err := s.Repos.CleanupDiffs(ctx, repoID, operatorID)
		if err != nil {
			return err
		}
		if err := s.waitJob(ctx, job.ID, 30*time.Minute); err != nil {
			return err
		}
	}
	// 旧版本扩展分区后可能留下了过期母盘指纹；现在没有子差异盘，且马上会用
	// Steam 内容更新母盘，先在离线状态重算一次，避免 reconcile 继续停用 target。
	if repo, rErr := s.Repos.Get(ctx, repoID); rErr == nil {
		if fpErr := s.Repos.refreshParentFingerprint(ctx, repo); fpErr != nil && s.Log != nil {
			s.Log.Warn("更新前刷新母盘指纹失败", "repo_id", repoID, "error", fpErr)
		}
	}
	return nil
}

func (s *SteamGameService) ensureParentCapacity(ctx context.Context, repoID, staging, operatorID string) error {
	raw := s.raw().Steam
	repo, err := s.Repos.Get(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.ParentDiskID == nil {
		return apperr.New("repo.disk_missing", 500)
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return err
	}
	required, err := dirSize(staging)
	if err != nil {
		return err
	}
	if required <= 0 {
		return nil
	}
	reservePercent := raw.ExpandReservePercent
	if reservePercent <= 0 {
		reservePercent = 15
	}
	target := required + required*int64(reservePercent)/100
	if min := required + (512 << 20); target < min {
		target = min
	}
	if target <= parent.SizeBytes {
		// 更新流程中母盘必须保持离线：不在这里挂载/修改母盘分区。
		// 分区扩展只在管理员显式扩容 VHDX 时执行。
		return nil
	}
	if !raw.AutoExpandEnabled() {
		return apperr.New("steam.repo_capacity_exceeded", 409).
			WithArg("current_bytes", parent.SizeBytes).WithArg("required_bytes", target)
	}
	if repo.Condition() != domain.ParentMaintenance {
		return apperr.RepoParentConditionViolation(string(repo.Condition()), string(domain.ParentMaintenance))
	}
	active, err := s.Store.CountActiveLeasesByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if active > 0 {
		return apperr.RepoHasActiveLease(active)
	}
	if n, err := s.Store.CountDiffDisks(ctx, parent.ID); err != nil {
		return err
	} else if n > 0 {
		// 扩容母盘要求没有子差异盘；这些差异盘在之后的 MergeDiff 里本来也会被删除。
		job, err := s.Repos.CleanupDiffs(ctx, repoID, operatorID)
		if err != nil {
			return err
		}
		if err := s.waitJob(ctx, job.ID, 30*time.Minute); err != nil {
			return err
		}
	}
	// 母盘可能被服务端本地挂载；扩容前先卸载。
	s.Repos.UnmountParentVolume(ctx, repoID)
	if s.Log != nil {
		s.Log.Info("游戏更新需要扩容母盘",
			"repo_id", repoID, "current_bytes", parent.SizeBytes, "required_bytes", target)
	}
	_, err = s.Repos.ResizeParentDisk(ctx, repoID, target, operatorID)
	return err
}

func (s *SteamGameService) waitStagingStable(ctx context.Context, dir string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var prev int64 = -1
	for {
		size, err := dirSize(dir)
		if err != nil {
			return err
		}
		if prev >= 0 && size == prev {
			return nil
		}
		prev = size
		if time.Now().After(deadline) {
			return apperr.New("steam.update_incomplete", 504).WithArg("reason", "staging_changing")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (s *SteamGameService) waitJob(ctx context.Context, jobID string, timeout time.Duration) error {
	if strings.TrimSpace(jobID) == "" {
		return apperr.New("steam.job_missing", 500)
	}
	deadline := time.Now().Add(timeout)
	for {
		j, err := s.Store.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		if j.State.IsTerminal() {
			if j.State == domain.JobStateSucceeded {
				return nil
			}
			return apperr.New("steam.job_failed", 500).
				WithArg("job_id", jobID).WithArg("state", string(j.State)).
				WithCause(errors.New(j.LastError))
		}
		if time.Now().After(deadline) {
			return apperr.New("steam.job_timeout", 504).WithArg("job_id", jobID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *SteamGameService) waitRepoActive(ctx context.Context, repoID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		repo, err := s.Repos.Get(ctx, repoID)
		if err != nil {
			return err
		}
		switch repo.State {
		case domain.RepoStateActive:
			return nil
		case domain.RepoStateError, domain.RepoStateDeleting:
			return apperr.New("steam.repo_state_invalid", 409).WithArg("state", string(repo.State))
		}
		if time.Now().After(deadline) {
			return apperr.New("steam.repo_not_ready", 504).WithArg("repo_id", repoID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *SteamGameService) stagingDirFor(ctx context.Context, game *domain.Game) (string, error) {
	roots, err := s.sourceRoots(ctx)
	if err != nil {
		return "", err
	}
	if root := strings.TrimSpace(s.raw().Steam.StagingDir); root != "" {
		for _, allowed := range roots {
			if domain.UnderRoot(allowed, root) {
				return filepath.Join(root, strconv.FormatInt(game.AppID, 10)), nil
			}
		}
		if s.Log != nil {
			s.Log.Warn("steam.staging_dir 不在 source_roots/存储根白名单内，回退到第一个允许的根",
				"configured", root)
		}
	}
	if len(roots) == 0 {
		return "", apperr.New("steam.staging_root_missing", 500)
	}
	return filepath.Join(roots[0], ".vault-steam", "staging", strconv.FormatInt(game.AppID, 10)), nil
}

// StopUpdate 暂停或停止一次正在下载的游戏更新。
//
// SteamCMD 没有原生暂停；这里的“暂停/停止”都是取消当前进程：
//   - pause：保留暂存目录，点继续时重新执行 app_update，SteamCMD 会尽量复用已下载内容；
//   - stop：取消后删除暂存目录，适合彻底放弃这次更新。
//
// 只允许在 checking/downloading 阶段操作；Diff/Merge 阶段取消会破坏任务链，直接拒绝。
func (s *SteamGameService) StopUpdate(ctx context.Context, gameID, mode, operatorID string) (*domain.Game, error) {
	if mode != SteamUpdateControlPause && mode != SteamUpdateControlStop {
		return nil, apperr.InvalidParam("action")
	}
	game, err := s.Store.GetGame(ctx, gameID)
	if err != nil {
		return nil, err
	}
	if game.Status != domain.GameStatusUpdating {
		return nil, apperr.New("steam.update_not_running", 409).WithArg("status", game.Status)
	}
	s.progressMu.Lock()
	cancel := s.cancels[gameID]
	progress := s.progress[gameID]
	if cancel == nil {
		s.progressMu.Unlock()
		return nil, apperr.New("steam.update_not_running", 409)
	}
	if progress.Phase != "checking" && progress.Phase != "downloading" {
		s.progressMu.Unlock()
		return nil, apperr.New("steam.update_not_stoppable", 409).WithArg("phase", progress.Phase)
	}
	s.cancelModes[gameID] = mode
	progress.Phase = "stopping"
	progress.CanStop = false
	progress.UpdatedAt = time.Now().UnixMilli()
	s.progress[gameID] = progress
	s.progressMu.Unlock()

	cancel()
	s.audit(ctx, operatorID, "game.update."+mode, "game:"+game.ID, "", domain.AuditResultOK)
	return game, nil
}

func (s *SteamGameService) cleanupUpdate(ctx context.Context, gameID, staging string) {
	mode := s.takeCancelMode(gameID)
	s.unregisterCancel(gameID)
	if errors.Is(ctx.Err(), context.Canceled) && mode == SteamUpdateControlStop && staging != "" {
		if err := os.RemoveAll(staging); err != nil {
			s.Log.Warn("停止更新后清理暂存目录失败", "game_id", gameID, "staging", staging, "error", err)
		}
	}
	s.clearProgress(gameID)
}

func (s *SteamGameService) markUpdateFailure(ctx context.Context, gameID, code string) {
	if errors.Is(ctx.Err(), context.Canceled) {
		s.markGamePaused(ctx, gameID)
		return
	}
	s.markGameError(ctx, gameID, code)
}

func (s *SteamGameService) markGamePaused(ctx context.Context, gameID string) {
	writeCtx := context.WithoutCancel(ctx)
	game, err := s.Store.GetGame(writeCtx, gameID)
	if err != nil {
		return
	}
	game.Status = domain.GameStatusPaused
	game.LastError = ""
	game.LastCheckedAt = time.Now().UnixMilli()
	_ = s.Store.UpdateGame(writeCtx, game)
	s.emit("game", map[string]any{"action": "paused", "game_id": gameID})
}

func (s *SteamGameService) markGameError(ctx context.Context, gameID, code string) {
	writeCtx := context.WithoutCancel(ctx)
	game, err := s.Store.GetGame(writeCtx, gameID)
	if err != nil {
		return
	}
	game.Status = domain.GameStatusError
	game.LastError = code
	_ = s.Store.UpdateGame(writeCtx, game)
	s.emit("game", map[string]any{"action": "error", "game_id": gameID, "code": code})
}

func defaultGameRepoName(appName string, appID int64) string {
	name := strings.TrimSpace(appName)
	if name == "" {
		return fmt.Sprintf("Steam App %d", appID)
	}
	return fmt.Sprintf("%s (%d)", name, appID)
}

func platformToClientOS(platform string) string {
	if strings.EqualFold(platform, "linux") {
		return "linux"
	}
	return "windows"
}

// updateWindowOK 判断当前时间是否落在 "HH:MM-HH:MM" 时间窗内；空窗口表示不限制。
func updateWindowOK(window string, now time.Time) bool {
	window = strings.TrimSpace(window)
	if window == "" {
		return true
	}
	parts := strings.SplitN(window, "-", 2)
	if len(parts) != 2 {
		return true
	}
	start, err1 := time.Parse("15:04", strings.TrimSpace(parts[0]))
	end, err2 := time.Parse("15:04", strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	s := start.Hour()*60 + start.Minute()
	e := end.Hour()*60 + end.Minute()
	if s <= e {
		return cur >= s && cur <= e
	}
	return cur >= s || cur <= e
}
