package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// SteamAccountService 管理 Steam 内容账号。
//
// ⚠️ 安全边界：该服务只服务于"存储库内容下载/更新"。它不提供玩家登录、Steam 客户端自动登录、
// 任意 Steam 命令执行或凭据读取接口；凭据只在本服务内部解密并交给 SteamCMD。
type SteamAccountService struct {
	Deps
	runtimeMu      sync.Mutex
	runtimeInstall steamInstallState
}

// CreateSteamAccountInput 是创建账号的入参。
type CreateSteamAccountInput struct {
	Username       string
	DisplayName    string
	Credential     string
	CredentialKind domain.SteamCredentialKind
	GuardType      domain.SteamGuardType
	GuardSecret    string
	MaxConcurrent  int
}

// UpdateSteamAccountInput 是更新账号的入参；指针为 nil 表示不修改。
type UpdateSteamAccountInput struct {
	ID             string
	DisplayName    *string
	Credential     *string
	CredentialKind *domain.SteamCredentialKind
	GuardType      *domain.SteamGuardType
	GuardSecret    *string
	MaxConcurrent  *int
	Enabled        *bool
}

// SteamAccountAvailable 是"按 AppID/平台查询可用账号"的结果。
type SteamAccountAvailable struct {
	Account   *domain.SteamAccount
	OwnsApp   bool
	Busy      bool
	Available bool
	Reason    string
}

// Create 创建一个内容账号。凭据只以密文落库。
func (s *SteamAccountService) Create(ctx context.Context, in CreateSteamAccountInput, operatorID string) (*domain.SteamAccount, error) {
	username := strings.TrimSpace(in.Username)
	if username == "" {
		return nil, apperr.InvalidParam("username")
	}
	if strings.TrimSpace(in.Credential) == "" {
		return nil, apperr.InvalidParam("credential")
	}
	kind := in.CredentialKind
	if kind == "" {
		kind = domain.SteamCredentialPassword
	}
	if !kind.Valid() {
		return nil, apperr.InvalidParam("credential_kind")
	}
	guard := in.GuardType
	if guard == "" {
		guard = domain.SteamGuardNone
	}
	if !guard.Valid() {
		return nil, apperr.InvalidParam("guard_type")
	}
	if guard == domain.SteamGuardTOTP && strings.TrimSpace(in.GuardSecret) == "" {
		return nil, apperr.InvalidParam("guard_secret")
	}
	maxConcurrent := in.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	cred, err := s.encrypt(in.Credential)
	if err != nil {
		return nil, err
	}
	var guardCipher []byte
	if strings.TrimSpace(in.GuardSecret) != "" {
		guardCipher, err = s.encrypt(in.GuardSecret)
		if err != nil {
			return nil, err
		}
	}

	a := &domain.SteamAccount{
		Username:          username,
		DisplayName:       strings.TrimSpace(in.DisplayName),
		Status:            domain.SteamAccountStatusNeeds2FA,
		CredentialCipher:  cred,
		CredentialKind:    kind,
		SessionState:      domain.SteamSessionNone,
		GuardType:         guard,
		GuardSecretCipher: guardCipher,
		MaxConcurrent:     maxConcurrent,
	}
	if display := a.DisplayName; display == "" {
		a.DisplayName = username
	}
	a.SessionHome = s.sessionHomeFor("")
	if err := s.Store.CreateSteamAccount(ctx, a); err != nil {
		return nil, err
	}
	// 先生成 ID 再补会话目录：目录名用稳定 ID，后续重置账号也不会串会话。
	a.SessionHome = s.sessionHomeFor(a.ID)
	if err := s.Store.UpdateSteamAccount(ctx, a); err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "steam.account.create", "steam_account:"+a.ID,
		"username="+a.Username+" guard="+string(a.GuardType), domain.AuditResultOK)
	return a, nil
}

// Update 更新账号可变字段。修改凭据/Guard 后会回到 needs_2fa，并要求重新验证。
func (s *SteamAccountService) Update(ctx context.Context, in UpdateSteamAccountInput, operatorID string) (*domain.SteamAccount, error) {
	a, err := s.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	reauth := false
	if in.DisplayName != nil {
		a.DisplayName = strings.TrimSpace(*in.DisplayName)
	}
	if in.Credential != nil && strings.TrimSpace(*in.Credential) != "" {
		cred, err := s.encrypt(*in.Credential)
		if err != nil {
			return nil, err
		}
		a.CredentialCipher = cred
		reauth = true
	}
	if in.CredentialKind != nil {
		if !in.CredentialKind.Valid() {
			return nil, apperr.InvalidParam("credential_kind")
		}
		if *in.CredentialKind != a.CredentialKind {
			a.CredentialKind = *in.CredentialKind
			reauth = true
		}
	}
	if in.GuardType != nil {
		if !in.GuardType.Valid() {
			return nil, apperr.InvalidParam("guard_type")
		}
		// 只改显示名/并发数时前端也会回传未变化的 guard_type；
		// 如果这里无条件 reauth，用户每次编辑账号都会丢掉已保存的登录态。
		if *in.GuardType != a.GuardType {
			a.GuardType = *in.GuardType
			reauth = true
		}
	}
	if in.GuardSecret != nil {
		if strings.TrimSpace(*in.GuardSecret) != "" {
			guard, err := s.encrypt(*in.GuardSecret)
			if err != nil {
				return nil, err
			}
			a.GuardSecretCipher = guard
			reauth = true
		} else {
			a.GuardSecretCipher = nil
		}
	}
	if in.MaxConcurrent != nil {
		if *in.MaxConcurrent <= 0 {
			return nil, apperr.InvalidParam("max_concurrent")
		}
		a.MaxConcurrent = *in.MaxConcurrent
	}
	if in.Enabled != nil {
		if *in.Enabled && a.Status == domain.SteamAccountStatusDisabled {
			a.Status = domain.SteamAccountStatusNeeds2FA
			reauth = true
		}
		if !*in.Enabled {
			a.Status = domain.SteamAccountStatusDisabled
		}
	}
	if reauth {
		a.SessionState = domain.SteamSessionNone
		a.LastError = ""
		if a.Status != domain.SteamAccountStatusDisabled {
			a.Status = domain.SteamAccountStatusNeeds2FA
		}
	}
	if err := s.Store.UpdateSteamAccount(ctx, a); err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "steam.account.update", "steam_account:"+a.ID,
		"username="+a.Username+" reauth="+boolText(reauth), domain.AuditResultOK)
	return a, nil
}

// List 列出账号。
func (s *SteamAccountService) List(ctx context.Context, limit, offset int) ([]domain.SteamAccount, error) {
	return s.Store.ListSteamAccounts(ctx, limit, offset)
}

// Get 按 ID 查询账号。
func (s *SteamAccountService) Get(ctx context.Context, id string) (*domain.SteamAccount, error) {
	return s.Store.GetSteamAccount(ctx, id)
}

// Disable 软删除：保留账号记录，停止其参与任何下载/更新。
func (s *SteamAccountService) Disable(ctx context.Context, id, operatorID string) error {
	if err := s.Store.SetSteamAccountStatus(ctx, id, domain.SteamAccountStatusDisabled, "disabled by admin"); err != nil {
		return err
	}
	s.audit(ctx, operatorID, "steam.account.disable", "steam_account:"+id, "", domain.AuditResultOK)
	return nil
}

// TestLogin 执行一次 SteamCMD 登录测试。
//
// 这是管理员维护动作，不属于"游戏下载"；但它同样不暴露凭据，只是验证账号是否可用。
func (s *SteamAccountService) TestLogin(ctx context.Context, id, guardCode, operatorID string) (*domain.SteamAccount, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status == domain.SteamAccountStatusDisabled {
		return nil, apperr.New("steam.account_disabled", 409)
	}
	explicitGuard := strings.TrimSpace(guardCode) != ""
	password, guard, err := s.credentials(a, guardCode)
	if err != nil {
		return nil, err
	}
	// 登录成功后还会顺手同步一次许可，两步都可能各跑几分钟；租约要覆盖完整流程。
	lease, err := s.acquire(ctx, a, "test_login", id, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	defer s.release(ctx, lease)

	sessionHome := s.sessionHomeFor(a.ID)
	// 用户刚输入验证码时不要再先试缓存登录：失败会额外触发一次 Steam Guard 通知。
	res, loginErr := s.steamTestLogin(ctx, sessionHome, a.Username, password, guard, !explicitGuard)
	now := time.Now().UnixMilli()
	a.LastOutput = truncateSteamOutput(res.Output)
	switch {
	case loginErr != nil && res.Needs2FA:
		a.Status = domain.SteamAccountStatusNeeds2FA
		a.LastError = "needs_2fa"
	case loginErr != nil:
		a.Status = domain.SteamAccountStatusError
		a.LastError = apperr.CodeOf(loginErr)
	default:
		a.Status = domain.SteamAccountStatusActive
		a.SessionState = domain.SteamSessionValid
		a.LastLoginAt = now
		a.LastError = ""
		a.LastGuardAt = now
	}
	if err := s.Store.UpdateSteamAccount(ctx, a); err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "steam.account.test_login", "steam_account:"+a.ID,
		"status="+string(a.Status), auditResult(loginErr))
	if loginErr != nil {
		return a, loginErr
	}
	// 登录成功后不再立刻发起第二次 SteamCMD 登录去同步许可：
	// 邮箱验证码通常一次有效，第二次登录很容易把刚验证成功的状态又打回 needs_2fa。
	// 许可同步由手动「同步许可」或后台调度器完成。
	return a, nil
}

// SyncLicenses 同步账号拥有的 AppID（licenses_print 的 Apps 行）。
func (s *SteamAccountService) SyncLicenses(ctx context.Context, id, guardCode, operatorID string) ([]domain.SteamAccountApp, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status == domain.SteamAccountStatusDisabled {
		return nil, apperr.New("steam.account_disabled", 409)
	}
	explicitGuard := strings.TrimSpace(guardCode) != ""
	password, guard, err := s.credentials(a, guardCode)
	if err != nil {
		return nil, err
	}
	lease, err := s.acquire(ctx, a, "license_sync", id, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	defer s.release(ctx, lease)
	apps, err := s.syncLicensesWith(ctx, a, password, guard, !explicitGuard)
	s.audit(ctx, operatorID, "steam.account.license_sync", "steam_account:"+a.ID,
		"apps="+itoa(len(apps)), auditResult(err))
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// ImportClientGame 是客户端本机 Steam 库同步上来的一个游戏。
type ImportClientGame struct {
	AppID     int64
	Name      string
	Installed bool
}

// ImportClientGames 把客户端本机 Steam 库同步到账号许可清单。
//
// 这条路径不登录 SteamCMD，也不访问 Steam 商店：客户端已经持有游戏名称/列表，
// 服务端只负责保存 AppID 与名称，后续下载仍由 SteamCMD 按 AppID 执行。
func (s *SteamAccountService) ImportClientGames(ctx context.Context, accountID string, games []ImportClientGame, operatorID string) (int, error) {
	if _, err := s.Get(ctx, accountID); err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()
	metas := make([]domain.SteamAppMeta, 0, len(games))
	imported := 0
	for _, game := range games {
		if game.AppID <= 0 {
			continue
		}
		if err := s.Store.UpsertSteamAccountApp(ctx, accountID, game.AppID, "client_steam"); err != nil {
			return imported, err
		}
		imported++
		if name := strings.TrimSpace(game.Name); name != "" && !steamAppNameIsPlaceholder(name) {
			metas = append(metas, domain.SteamAppMeta{
				AppID: game.AppID, Name: name, AppType: 0, UpdatedAt: now,
			})
		}
	}
	if len(metas) > 0 {
		// 客户端只带名称：合并已有封面/平台，避免把服务端之前解析到的元数据清空。
		ids := make([]int64, 0, len(metas))
		for _, meta := range metas {
			ids = append(ids, meta.AppID)
		}
		existing, err := s.Store.GetSteamApps(ctx, ids)
		if err != nil {
			return imported, err
		}
		for i := range metas {
			old := existing[metas[i].AppID]
			metas[i].Image = old.Image
			metas[i].Platforms = old.Platforms
		}
		if err := s.Store.UpsertSteamApps(ctx, metas); err != nil {
			return imported, err
		}
	}
	s.audit(ctx, operatorID, "steam.account.client_import", "steam_account:"+accountID,
		"apps="+itoa(imported), domain.AuditResultOK)
	return imported, nil
}

// HandleAuthError 把版本检查/更新路径上的登录错误同步回账号状态。
//
// 这样账号一失效，界面就会显示 needs_2fa/error 并弹出验证入口；调度器也会
// 停止继续用该账号尝试，避免每个 tick 都留下一条重复错误。
func (s *SteamAccountService) HandleAuthError(ctx context.Context, a *domain.SteamAccount, err error) {
	if a == nil || err == nil {
		return
	}
	code := apperr.CodeOf(err)
	switch code {
	case "steam.account_needs_2fa":
		a.Status = domain.SteamAccountStatusNeeds2FA
		a.SessionState = domain.SteamSessionExpired
	case "steam.account_login_failed":
		a.Status = domain.SteamAccountStatusError
		a.SessionState = domain.SteamSessionExpired
	default:
		return
	}
	a.LastCheckedAt = time.Now().UnixMilli()
	a.LastError = code
	_ = s.Store.UpdateSteamAccount(context.WithoutCancel(ctx), a)
}

// ListApps 列出账号已同步的 AppID。
func (s *SteamAccountService) ListApps(ctx context.Context, id string) ([]domain.SteamAccountApp, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.Store.ListSteamAccountApps(ctx, id)
}

// Available 按 AppID 查询可用账号。
func (s *SteamAccountService) Available(ctx context.Context, appID int64) ([]SteamAccountAvailable, error) {
	accounts, err := s.Store.ListSteamAccounts(ctx, 500, 0)
	if err != nil {
		return nil, err
	}
	out := make([]SteamAccountAvailable, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		item := SteamAccountAvailable{Account: a}
		if a.Status != domain.SteamAccountStatusActive {
			item.Reason = string(a.Status)
			out = append(out, item)
			continue
		}
		if appID > 0 {
			owns, err := s.Store.SteamAccountOwnsApp(ctx, a.ID, appID)
			if err != nil {
				return nil, err
			}
			item.OwnsApp = owns
			if !owns {
				item.Reason = "no_license"
				out = append(out, item)
				continue
			}
		} else {
			item.OwnsApp = true
		}
		leases, err := s.Store.ListActiveSteamLeases(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		item.Busy = len(leases) >= a.MaxConcurrent
		if item.Busy {
			item.Reason = "busy"
		} else {
			item.Available = true
		}
		out = append(out, item)
	}
	return out, nil
}

// AcquireLease 为内容更新任务占用账号。
func (s *SteamAccountService) AcquireLease(ctx context.Context, accountID, holder, holderID string, ttl time.Duration) (*domain.SteamAccountLease, error) {
	a, err := s.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if err := steamAccountUsableForUpdate(a); err != nil {
		return nil, err
	}
	return s.acquire(ctx, a, holder, holderID, ttl)
}

// ReleaseLease 释放租约。
func (s *SteamAccountService) ReleaseLease(ctx context.Context, leaseID string) error {
	return s.Store.ReleaseSteamLease(ctx, leaseID)
}

// ResetStaleLeases 在服务端启动时清理所有残留的活跃租约。
func (s *SteamAccountService) ResetStaleLeases(ctx context.Context) (int64, error) {
	return s.Store.ExpireAllActiveSteamLeases(ctx, "server_restart")
}

// ForceReleaseLeases 强制释放某账号的全部活跃租约（管理员排障入口）。
func (s *SteamAccountService) ForceReleaseLeases(ctx context.Context, accountID, operatorID string) (int64, error) {
	if _, err := s.Get(ctx, accountID); err != nil {
		return 0, err
	}
	n, err := s.Store.ReleaseSteamLeasesByAccount(ctx, accountID, "manual_release")
	if err != nil {
		return 0, err
	}
	s.audit(ctx, operatorID, "steam.account.lease_release", "steam_account:"+accountID,
		"released="+itoa(int(n)), domain.AuditResultOK)
	return n, nil
}

// ActiveLeaseCount 返回账号当前活跃租约数。
func (s *SteamAccountService) ActiveLeaseCount(ctx context.Context, accountID string) (int, error) {
	leases, err := s.Store.ListActiveSteamLeases(ctx, accountID)
	if err != nil {
		return 0, err
	}
	return len(leases), nil
}

// CredentialsForUpdate 返回一次内容更新所需的登录凭据（仅 app 包内部使用）。
//
// 该方法是"仅用于存储库下载"的强制入口：除 SteamGameService 外不应调用。
func (s *SteamAccountService) CredentialsForUpdate(a *domain.SteamAccount, guardCode string) (string, string, error) {
	if err := steamAccountUsableForUpdate(a); err != nil {
		return "", "", err
	}
	return s.credentials(a, guardCode)
}

func (s *SteamAccountService) syncLicensesWith(ctx context.Context, a *domain.SteamAccount, password, guard string, allowCached bool) ([]domain.SteamAccountApp, error) {
	apps, output, err := s.steamSyncLicenses(ctx, s.sessionHomeFor(a.ID), a.Username, password, guard, allowCached)
	now := time.Now().UnixMilli()
	a.LastOutput = truncateSteamOutput(output)
	if err != nil {
		a.LastCheckedAt = now
		a.LastError = apperr.CodeOf(err)
		switch a.LastError {
		case "steam.account_needs_2fa":
			// 缓存会话失效且没有可用的 TOTP：需要用户补一次验证码，
			// 界面据此提示，而不是让调度器无限重试。
			a.Status = domain.SteamAccountStatusNeeds2FA
			a.SessionState = domain.SteamSessionExpired
		case "steam.account_login_failed":
			a.Status = domain.SteamAccountStatusError
			a.SessionState = domain.SteamSessionExpired
		}
		_ = s.Store.UpdateSteamAccount(ctx, a)
		return nil, err
	}
	list := make([]domain.SteamAccountApp, 0, len(apps))
	for _, id := range apps {
		list = append(list, domain.SteamAccountApp{
			AccountID:  a.ID,
			AppID:      id,
			Source:     "licenses_print",
			VerifiedAt: now,
		})
	}
	if err := s.Store.ReplaceSteamAccountApps(ctx, a.ID, list); err != nil {
		return nil, err
	}
	a.LastCheckedAt = now
	a.LastLoginAt = now
	a.LastGuardAt = now
	a.Status = domain.SteamAccountStatusActive
	a.SessionState = domain.SteamSessionValid
	a.LastError = ""
	_ = s.Store.UpdateSteamAccount(ctx, a)
	return list, nil
}

func (s *SteamAccountService) credentials(a *domain.SteamAccount, guardCode string) (string, string, error) {
	if a.CredentialKind == domain.SteamCredentialRefreshToken {
		return "", "", apperr.New("steam.account_refresh_token_unsupported", 501)
	}
	password, err := s.decrypt(a.CredentialCipher)
	if err != nil {
		return "", "", err
	}
	code := strings.TrimSpace(guardCode)
	if code == "" && a.GuardType == domain.SteamGuardTOTP && a.HasGuardSecret() {
		secretText, err := s.decrypt(a.GuardSecretCipher)
		if err != nil {
			return "", "", err
		}
		code, err = steamTOTPCode(secretText, time.Now())
		if err != nil {
			return "", "", err
		}
	}
	return password, code, nil
}

func (s *SteamAccountService) acquire(ctx context.Context, a *domain.SteamAccount, holder, holderID string, ttl time.Duration) (*domain.SteamAccountLease, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	lease := &domain.SteamAccountLease{
		AccountID:  a.ID,
		Holder:     holder,
		HolderID:   holderID,
		State:      domain.SteamLeaseActive,
		AcquiredAt: time.Now().UnixMilli(),
		ExpiresAt:  time.Now().Add(ttl).UnixMilli(),
	}
	if err := s.Store.AcquireSteamLease(ctx, lease, a.MaxConcurrent); err != nil {
		return nil, err
	}
	return lease, nil
}

func (s *SteamAccountService) release(ctx context.Context, lease *domain.SteamAccountLease) {
	if lease == nil {
		return
	}
	_ = s.Store.ReleaseSteamLease(context.WithoutCancel(ctx), lease.ID)
}

func (s *SteamAccountService) encrypt(v string) ([]byte, error) {
	if s.Cipher == nil {
		return nil, apperr.New("steam.cipher_unavailable", 500)
	}
	b, err := s.Cipher.EncryptString(v)
	if err != nil {
		return nil, apperr.New("steam.encrypt_failed", 500).WithCause(err)
	}
	return b, nil
}

func (s *SteamAccountService) decrypt(b []byte) (string, error) {
	if s.Cipher == nil {
		return "", apperr.New("steam.cipher_unavailable", 500)
	}
	v, err := s.Cipher.DecryptString(b)
	if err != nil {
		return "", apperr.New("steam.decrypt_failed", 500).WithCause(err)
	}
	return v, nil
}

func (s *SteamAccountService) sessionHomeFor(accountID string) string {
	root := s.steamSessionRoot()
	if accountID == "" {
		return root
	}
	return filepath.Join(root, accountID)
}

func auditResult(err error) string {
	if err != nil {
		return domain.AuditResultError
	}
	return domain.AuditResultOK
}

// truncateSteamOutput 保留最近一段 SteamCMD 输出用于管理端诊断。
//
// 输出已经由 runSteamCmd 抹掉密码/验证码；这里只做长度限制，避免把几 MB 日志写进账号行。
func truncateSteamOutput(s string) string {
	const limit = 8192
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}
