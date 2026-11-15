package domain

// SteamAccountStatus 是 Steam 内容账号的可用状态。
//
// 该账号模块**只用于存储库内容下载/更新**：不允许玩家登录、不允许 Steam 客户端自动登录，
// 也不对外暴露任何凭据。状态只影响自动更新/手动更新任务是否可执行。
type SteamAccountStatus string

const (
	// SteamAccountStatusActive 已验证、可用于内容更新。
	SteamAccountStatusActive SteamAccountStatus = "active"
	// SteamAccountStatusNeeds2FA 需要 Steam Guard 验证码/手机令牌确认。
	SteamAccountStatusNeeds2FA SteamAccountStatus = "needs_2fa"
	// SteamAccountStatusDisabled 管理员主动禁用。
	SteamAccountStatusDisabled SteamAccountStatus = "disabled"
	// SteamAccountStatusError 密码错误、被锁、风控或 SteamCMD 失败。
	SteamAccountStatusError SteamAccountStatus = "error"
)

// Valid 校验状态取值。
func (s SteamAccountStatus) Valid() bool {
	switch s {
	case SteamAccountStatusActive, SteamAccountStatusNeeds2FA,
		SteamAccountStatusDisabled, SteamAccountStatusError:
		return true
	default:
		return false
	}
}

// Usable 返回该状态是否允许执行内容更新。
func (s SteamAccountStatus) Usable() bool { return s == SteamAccountStatusActive }

// SteamGuardType 是 Steam Guard 的类型。
type SteamGuardType string

const (
	SteamGuardNone  SteamGuardType = "none"
	SteamGuardTOTP  SteamGuardType = "totp"
	SteamGuardEmail SteamGuardType = "email"
)

// Valid 校验 Guard 类型。
func (g SteamGuardType) Valid() bool {
	return g == SteamGuardNone || g == SteamGuardTOTP || g == SteamGuardEmail
}

// SteamCredentialKind 是账号凭据类型。
type SteamCredentialKind string

const (
	SteamCredentialPassword     SteamCredentialKind = "password"
	SteamCredentialRefreshToken SteamCredentialKind = "refresh_token"
)

// Valid 校验凭据类型。
func (k SteamCredentialKind) Valid() bool {
	return k == SteamCredentialPassword || k == SteamCredentialRefreshToken
}

// SteamSessionState 是长期会话状态（refresh token / ssfn 是否可用）。
type SteamSessionState string

const (
	SteamSessionNone    SteamSessionState = "none"
	SteamSessionValid   SteamSessionState = "valid"
	SteamSessionExpired SteamSessionState = "expired"
	SteamSessionRevoked SteamSessionState = "revoked"
)

// SteamAccount 是一个 Steam 内容账号。
//
// ⚠️ CredentialCipher / GuardSecretCipher / AuthTokenCipher 永远不得进入 DTO 或日志。
type SteamAccount struct {
	ID                string              `db:"id" json:"id"`
	Username          string              `db:"username" json:"username"`
	DisplayName       string              `db:"display_name" json:"display_name"`
	Status            SteamAccountStatus  `db:"status" json:"status"`
	CredentialCipher  []byte              `db:"credential_cipher" json:"-"`
	CredentialKind    SteamCredentialKind `db:"credential_kind" json:"credential_kind"`
	AuthTokenCipher   []byte              `db:"auth_token_cipher" json:"-"`
	TokenUpdatedAt    int64               `db:"token_updated_at" json:"token_updated_at"`
	SessionState      SteamSessionState   `db:"session_state" json:"session_state"`
	GuardType         SteamGuardType      `db:"guard_type" json:"guard_type"`
	GuardSecretCipher []byte              `db:"guard_secret_cipher" json:"-"`
	LastGuardAt       int64               `db:"last_guard_at" json:"last_guard_at"`
	MaxConcurrent     int                 `db:"max_concurrent" json:"max_concurrent"`
	LastLoginAt       int64               `db:"last_login_at" json:"last_login_at"`
	LastCheckedAt     int64               `db:"last_checked_at" json:"last_checked_at"`
	LastError         string              `db:"last_error" json:"last_error"`
	LastOutput        string              `db:"last_output" json:"last_output"`
	SessionHome       string              `db:"session_home" json:"session_home"`
	CreatedAt         int64               `db:"created_at" json:"created_at"`
	UpdatedAt         int64               `db:"updated_at" json:"updated_at"`
}

// HasCredential 返回是否已设置凭据。
func (a *SteamAccount) HasCredential() bool { return a != nil && len(a.CredentialCipher) > 0 }

// HasGuardSecret 返回是否已设置 TOTP secret。
func (a *SteamAccount) HasGuardSecret() bool { return a != nil && len(a.GuardSecretCipher) > 0 }

// Usable 返回账号是否可用于内容更新。
func (a *SteamAccount) Usable() bool {
	return a != nil && a.Status == SteamAccountStatusActive
}

// SteamAccountApp 是账号拥有的一个 AppID（由 licenses_print 同步或管理员手动标记）。
type SteamAccountApp struct {
	AccountID  string `db:"account_id" json:"account_id"`
	AppID      int64  `db:"app_id" json:"app_id"`
	Source     string `db:"source" json:"source"`
	VerifiedAt int64  `db:"verified_at" json:"verified_at"`
}

// SteamAppMeta 是 Steam 商店元数据缓存（名称/封面/平台），按 AppID 共享。
type SteamAppMeta struct {
	AppID     int64  `db:"app_id" json:"app_id"`
	Name      string `db:"name" json:"name"`
	Image     string `db:"image" json:"image"`
	Platforms string `db:"platforms" json:"platforms"` // 逗号分隔：windows,linux
	// AppType 是商店条目的类型：0=game，1=demo，4=dlc，6=software/application。
	// -1 表示尚未解析（老数据/内置应用），列表只展示 0 且名称可用的条目。
	AppType   int   `db:"app_type" json:"app_type"`
	UpdatedAt int64 `db:"updated_at" json:"updated_at"`
}

// SteamAccountLease 是账号级租约，保证同一账号同一时刻只有一个 SteamCMD 会话。
type SteamAccountLease struct {
	ID         string `db:"id" json:"id"`
	AccountID  string `db:"account_id" json:"account_id"`
	Holder     string `db:"holder" json:"holder"`
	HolderID   string `db:"holder_id" json:"holder_id"`
	State      string `db:"state" json:"state"`
	AcquiredAt int64  `db:"acquired_at" json:"acquired_at"`
	ExpiresAt  int64  `db:"expires_at" json:"expires_at"`
	ReleasedAt int64  `db:"released_at" json:"released_at"`
	LastError  string `db:"last_error" json:"last_error"`
}

// 租约状态取值。
const (
	SteamLeaseActive   = "active"
	SteamLeaseReleased = "released"
	SteamLeaseExpired  = "expired"
)

// RepoGroupKind 是服务端分组的类型。
type RepoGroupKind string

const (
	RepoGroupGeneral RepoGroupKind = "general"
	RepoGroupSteam   RepoGroupKind = "steam"
)

// Valid 校验分组类型。
func (k RepoGroupKind) Valid() bool {
	return k == RepoGroupGeneral || k == RepoGroupSteam
}

// RepoGroupAccessMode 是 Steam 游戏库的访问范围。
type RepoGroupAccessMode string

const (
	// RepoGroupAccessAll 任意登录用户都可查看/挂载。
	RepoGroupAccessAll RepoGroupAccessMode = "all"
	// RepoGroupAccessSelected 只有分组指定成员可查看/挂载。
	RepoGroupAccessSelected RepoGroupAccessMode = "selected"
)

// Valid 校验访问范围。
func (m RepoGroupAccessMode) Valid() bool {
	return m == RepoGroupAccessAll || m == RepoGroupAccessSelected
}

// RepoGroup 是服务端分组实体。Steam 组绑定一个内容账号，并保存下载平台与自动更新配置。
type RepoGroup struct {
	ID             string              `db:"id" json:"id"`
	Name           string              `db:"name" json:"name"`
	Kind           RepoGroupKind       `db:"kind" json:"kind"`
	Description    string              `db:"description" json:"description"`
	SteamAccountID *string             `db:"steam_account_id" json:"steam_account_id,omitempty"`
	TargetPlatform string              `db:"target_platform" json:"target_platform"`
	AutoUpdate     bool                `db:"auto_update" json:"auto_update"`
	UpdateSchedule string              `db:"update_schedule" json:"update_schedule"`
	UpdateWindow   string              `db:"update_window" json:"update_window"`
	MaxConcurrent  int                 `db:"max_concurrent" json:"max_concurrent"`
	StorageID      string              `db:"storage_id" json:"storage_id"`
	AccessMode     RepoGroupAccessMode `db:"access_mode" json:"access_mode"`
	CreatedAt      int64               `db:"created_at" json:"created_at"`
	UpdatedAt      int64               `db:"updated_at" json:"updated_at"`
}

// RepoGroupMember 是"仅指定用户可访问"游戏组里的一个成员。
type RepoGroupMember struct {
	GroupID   string `db:"group_id" json:"group_id"`
	UserID    string `db:"user_id" json:"user_id"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
}

// IsSteam 返回是否为 Steam 游戏组。
func (g *RepoGroup) IsSteam() bool { return g != nil && g.Kind == RepoGroupSteam }

// Game 是一个游戏库（一个 AppID 对应一个共享存储库）。
type Game struct {
	ID               string  `db:"id" json:"id"`
	GroupID          string  `db:"group_id" json:"group_id"`
	RepoID           *string `db:"repo_id" json:"repo_id,omitempty"`
	AppID            int64   `db:"app_id" json:"app_id"`
	AppName          string  `db:"app_name" json:"app_name"`
	InstallDir       string  `db:"install_dir" json:"install_dir"`
	BuildID          string  `db:"build_id" json:"build_id"`
	AvailableBuildID string  `db:"available_build_id" json:"available_build_id"`
	TargetPlatform   string  `db:"target_platform" json:"target_platform"`
	SteamAccountID   *string `db:"steam_account_id" json:"steam_account_id,omitempty"`
	// AutoUpdate 为 nil 表示继承分组配置。
	AutoUpdate    *bool  `db:"auto_update" json:"auto_update,omitempty"`
	Status        string `db:"status" json:"status"`
	LastCheckedAt int64  `db:"last_checked_at" json:"last_checked_at"`
	LastUpdatedAt int64  `db:"last_updated_at" json:"last_updated_at"`
	LastError     string `db:"last_error" json:"last_error"`
	CreatedAt     int64  `db:"created_at" json:"created_at"`
	UpdatedAt     int64  `db:"updated_at" json:"updated_at"`
}

// 游戏状态。
const (
	GameStatusReady    = "ready"
	GameStatusUpdating = "updating"
	// GameStatusPaused 表示用户暂停/停止了下载，保留暂存内容，等待手动继续。
	GameStatusPaused = "paused"
	GameStatusError  = "error"
)
