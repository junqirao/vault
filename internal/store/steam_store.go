package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// BackfillRepoGroups 把现有 repositories.meta.group 去重登记为 general 分组。
//
// 老库只有自由文本组名，没有服务端分组实体；升级到 schema v11 后需要让这些组名
// 出现在「游戏组/分组设置」里，否则用户会以为原有分组丢了。
func (s *Store) BackfillRepoGroups(ctx context.Context) error {
	var rows []struct {
		Meta string `db:"meta"`
	}
	if err := s.q.SelectContext(ctx, &rows, `SELECT meta FROM repositories`); err != nil {
		return fmt.Errorf("store: 读取存储库分组失败: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		var meta domain.RepoMeta
		fromJSON(row.Meta, &meta)
		name := strings.TrimSpace(meta.Group)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, err := s.GetRepoGroupByName(ctx, name); err == nil {
			continue
		} else if !isNoRows(err) {
			// GetRepoGroupByName 把"不存在"转成了业务错误，这里用 SQL 直接判断更稳妥。
			var n int
			if qErr := s.q.GetContext(ctx, &n,
				s.q.Rebind(`SELECT COUNT(*) FROM repo_groups WHERE name = ?`), name); qErr != nil {
				return fmt.Errorf("store: 探测分组是否存在失败: %w", qErr)
			}
			if n > 0 {
				continue
			}
		}
		g := &domain.RepoGroup{
			Name: name, Kind: domain.RepoGroupGeneral,
			TargetPlatform: "windows", MaxConcurrent: 1,
		}
		if err := s.CreateRepoGroup(ctx, g); err != nil {
			if e, ok := apperr.As(err); ok && e.Code == "group.name_taken" {
				continue
			}
			return err
		}
	}
	return nil
}

// RenameRepoGroupLabel 在分组改名时同步更新存储库的 meta.group。
//
// 组名同时是客户端分组展示与本机 group_mounts 的索引键；不改的话库会留在旧组。
func (s *Store) RenameRepoGroupLabel(ctx context.Context, oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return nil
	}
	var rows []struct {
		ID   string `db:"id"`
		Meta string `db:"meta"`
	}
	if err := s.q.SelectContext(ctx, &rows, `SELECT id, meta FROM repositories`); err != nil {
		return fmt.Errorf("store: 读取存储库分组失败: %w", err)
	}
	for _, row := range rows {
		var meta domain.RepoMeta
		fromJSON(row.Meta, &meta)
		if strings.TrimSpace(meta.Group) != oldName {
			continue
		}
		meta.Group = newName
		if _, err := s.q.ExecContext(ctx,
			s.q.Rebind(`UPDATE repositories SET meta = ? WHERE id = ?`), toJSON(meta), row.ID); err != nil {
			return fmt.Errorf("store: 更新存储库分组失败: %w", err)
		}
	}
	return nil
}

// ---------- steam_accounts ----------

const steamAccountCols = `id, username, display_name, status, credential_cipher, credential_kind,
	auth_token_cipher, token_updated_at, session_state, guard_type, guard_secret_cipher,
	last_guard_at, max_concurrent, last_login_at, last_checked_at, last_error, last_output, session_home,
	created_at, updated_at`

type steamAccountRow struct {
	ID                string `db:"id"`
	Username          string `db:"username"`
	DisplayName       string `db:"display_name"`
	Status            string `db:"status"`
	CredentialCipher  []byte `db:"credential_cipher"`
	CredentialKind    string `db:"credential_kind"`
	AuthTokenCipher   []byte `db:"auth_token_cipher"`
	TokenUpdatedAt    int64  `db:"token_updated_at"`
	SessionState      string `db:"session_state"`
	GuardType         string `db:"guard_type"`
	GuardSecretCipher []byte `db:"guard_secret_cipher"`
	LastGuardAt       int64  `db:"last_guard_at"`
	MaxConcurrent     int    `db:"max_concurrent"`
	LastLoginAt       int64  `db:"last_login_at"`
	LastCheckedAt     int64  `db:"last_checked_at"`
	LastError         string `db:"last_error"`
	LastOutput        string `db:"last_output"`
	SessionHome       string `db:"session_home"`
	CreatedAt         int64  `db:"created_at"`
	UpdatedAt         int64  `db:"updated_at"`
}

func (r steamAccountRow) toDomain() *domain.SteamAccount {
	return &domain.SteamAccount{
		ID:                r.ID,
		Username:          r.Username,
		DisplayName:       r.DisplayName,
		Status:            domain.SteamAccountStatus(r.Status),
		CredentialCipher:  r.CredentialCipher,
		CredentialKind:    domain.SteamCredentialKind(r.CredentialKind),
		AuthTokenCipher:   r.AuthTokenCipher,
		TokenUpdatedAt:    r.TokenUpdatedAt,
		SessionState:      domain.SteamSessionState(r.SessionState),
		GuardType:         domain.SteamGuardType(r.GuardType),
		GuardSecretCipher: r.GuardSecretCipher,
		LastGuardAt:       r.LastGuardAt,
		MaxConcurrent:     r.MaxConcurrent,
		LastLoginAt:       r.LastLoginAt,
		LastCheckedAt:     r.LastCheckedAt,
		LastError:         r.LastError,
		LastOutput:        r.LastOutput,
		SessionHome:       r.SessionHome,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

// CreateSteamAccount 插入一个 Steam 内容账号。
func (s *Store) CreateSteamAccount(ctx context.Context, a *domain.SteamAccount) error {
	if a.ID == "" {
		a.ID = newID()
	}
	now := nowMillis()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.MaxConcurrent <= 0 {
		a.MaxConcurrent = 1
	}
	if a.SessionState == "" {
		a.SessionState = domain.SteamSessionNone
	}
	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO steam_accounts (`+steamAccountCols+`)
		 VALUES (:id, :username, :display_name, :status, :credential_cipher, :credential_kind,
		         :auth_token_cipher, :token_updated_at, :session_state, :guard_type, :guard_secret_cipher,
		         :last_guard_at, :max_concurrent, :last_login_at, :last_checked_at, :last_error, :last_output, :session_home,
		         :created_at, :updated_at)`,
		map[string]any{
			"id":                  a.ID,
			"username":            a.Username,
			"display_name":        a.DisplayName,
			"status":              string(a.Status),
			"credential_cipher":   a.CredentialCipher,
			"credential_kind":     string(a.CredentialKind),
			"auth_token_cipher":   nullableBytes(a.AuthTokenCipher),
			"token_updated_at":    a.TokenUpdatedAt,
			"session_state":       string(a.SessionState),
			"guard_type":          string(a.GuardType),
			"guard_secret_cipher": nullableBytes(a.GuardSecretCipher),
			"last_guard_at":       a.LastGuardAt,
			"max_concurrent":      a.MaxConcurrent,
			"last_login_at":       a.LastLoginAt,
			"last_checked_at":     a.LastCheckedAt,
			"last_error":          a.LastError,
			"last_output":         a.LastOutput,
			"session_home":        a.SessionHome,
			"created_at":          a.CreatedAt,
			"updated_at":          a.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.New("steam.account_exists", 409).WithArg("username", a.Username)
	}
	if err != nil {
		return fmt.Errorf("store: 创建 Steam 账号失败: %w", err)
	}
	return nil
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// GetSteamAccount 按 ID 查询账号。
func (s *Store) GetSteamAccount(ctx context.Context, id string) (*domain.SteamAccount, error) {
	var row steamAccountRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+steamAccountCols+` FROM steam_accounts WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("steam.account_not_found", 404).WithArg("id", id)
		})
	}
	return row.toDomain(), nil
}

// GetSteamAccountByUsername 按登录名查询账号。
func (s *Store) GetSteamAccountByUsername(ctx context.Context, username string) (*domain.SteamAccount, error) {
	var row steamAccountRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+steamAccountCols+` FROM steam_accounts WHERE username = ?`), username)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("steam.account_not_found", 404).WithArg("username", username)
		})
	}
	return row.toDomain(), nil
}

// ListSteamAccounts 列出账号。
func (s *Store) ListSteamAccounts(ctx context.Context, limit, offset int) ([]domain.SteamAccount, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []steamAccountRow
	if err := s.q.SelectContext(ctx, &rows,
		s.q.Rebind(`SELECT `+steamAccountCols+` FROM steam_accounts ORDER BY created_at DESC LIMIT ? OFFSET ?`),
		limit, offset); err != nil {
		return nil, fmt.Errorf("store: 查询 Steam 账号列表失败: %w", err)
	}
	out := make([]domain.SteamAccount, 0, len(rows))
	for i := range rows {
		out = append(out, *rows[i].toDomain())
	}
	return out, nil
}

// UpdateSteamAccount 更新账号的可变字段（凭据列由调用方传入完整密文）。
func (s *Store) UpdateSteamAccount(ctx context.Context, a *domain.SteamAccount) error {
	a.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE steam_accounts SET
		 username=:username, display_name=:display_name, status=:status,
		 credential_cipher=:credential_cipher, credential_kind=:credential_kind,
		 auth_token_cipher=:auth_token_cipher, token_updated_at=:token_updated_at,
		 session_state=:session_state, guard_type=:guard_type, guard_secret_cipher=:guard_secret_cipher,
		 last_guard_at=:last_guard_at, max_concurrent=:max_concurrent,
		 last_login_at=:last_login_at, last_checked_at=:last_checked_at,
		 last_error=:last_error, last_output=:last_output, session_home=:session_home, updated_at=:updated_at
		 WHERE id=:id`,
		map[string]any{
			"id":                  a.ID,
			"username":            a.Username,
			"display_name":        a.DisplayName,
			"status":              string(a.Status),
			"credential_cipher":   a.CredentialCipher,
			"credential_kind":     string(a.CredentialKind),
			"auth_token_cipher":   nullableBytes(a.AuthTokenCipher),
			"token_updated_at":    a.TokenUpdatedAt,
			"session_state":       string(a.SessionState),
			"guard_type":          string(a.GuardType),
			"guard_secret_cipher": nullableBytes(a.GuardSecretCipher),
			"last_guard_at":       a.LastGuardAt,
			"max_concurrent":      a.MaxConcurrent,
			"last_login_at":       a.LastLoginAt,
			"last_checked_at":     a.LastCheckedAt,
			"last_error":          a.LastError,
			"last_output":         a.LastOutput,
			"session_home":        a.SessionHome,
			"updated_at":          a.UpdatedAt,
		})
	if err != nil {
		return fmt.Errorf("store: 更新 Steam 账号失败: %w", err)
	}
	return mustAffect(res, apperr.New("steam.account_not_found", 404).WithArg("id", a.ID))
}

// SetSteamAccountStatus 只更新账号状态与最近错误（避免整行回写覆盖并发的凭据修改）。
func (s *Store) SetSteamAccountStatus(ctx context.Context, id string, status domain.SteamAccountStatus, lastError string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_accounts SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`),
		string(status), lastError, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新 Steam 账号状态失败: %w", err)
	}
	return mustAffect(res, apperr.New("steam.account_not_found", 404).WithArg("id", id))
}

// SetSteamAccountSession 更新长期会话相关字段。
func (s *Store) SetSteamAccountSession(ctx context.Context, id string, state domain.SteamSessionState,
	tokenCipher []byte, lastLoginAt, lastCheckedAt int64, lastError string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_accounts SET session_state = ?, auth_token_cipher = ?, token_updated_at = ?,
			last_login_at = ?, last_checked_at = ?, last_error = ?, updated_at = ? WHERE id = ?`),
		string(state), nullableBytes(tokenCipher), nowMillis(), lastLoginAt, lastCheckedAt, lastError, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新 Steam 账号会话失败: %w", err)
	}
	return mustAffect(res, apperr.New("steam.account_not_found", 404).WithArg("id", id))
}

// DeleteSteamAccount 物理删除账号（外键级联清理 apps / leases）。
//
// 业务层默认走"禁用"；只有管理员明确确认且没有分组引用时才允许物理删除。
func (s *Store) DeleteSteamAccount(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM steam_accounts WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除 Steam 账号失败: %w", err)
	}
	return mustAffect(res, apperr.New("steam.account_not_found", 404).WithArg("id", id))
}

// ---------- steam_account_apps ----------

// ReplaceSteamAccountApps 用同步结果覆盖账号的 AppID 列表。
func (s *Store) ReplaceSteamAccountApps(ctx context.Context, accountID string, apps []domain.SteamAccountApp) error {
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`DELETE FROM steam_account_apps WHERE account_id = ?`), accountID); err != nil {
		return fmt.Errorf("store: 清理 Steam 账号许可失败: %w", err)
	}
	for i := range apps {
		apps[i].AccountID = accountID
		if apps[i].VerifiedAt == 0 {
			apps[i].VerifiedAt = nowMillis()
		}
		if _, err := s.q.NamedExecContext(ctx,
			`INSERT INTO steam_account_apps (account_id, app_id, source, verified_at)
			 VALUES (:account_id, :app_id, :source, :verified_at)`, apps[i]); err != nil {
			if isDuplicate(err) {
				continue
			}
			return fmt.Errorf("store: 写入 Steam 账号许可失败: %w", err)
		}
	}
	return nil
}

// UpsertSteamAccountApp 标记账号拥有某个 AppID（手动或更新成功后）。
func (s *Store) UpsertSteamAccountApp(ctx context.Context, accountID string, appID int64, source string) error {
	_, err := s.q.ExecContext(ctx,
		s.q.Rebind(`INSERT INTO steam_account_apps (account_id, app_id, source, verified_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(account_id, app_id) DO UPDATE SET source=excluded.source, verified_at=excluded.verified_at`),
		accountID, appID, source, nowMillis())
	if err != nil {
		// MySQL 不支持上面的 SQLite 冲突语法，退化到先删后插。
		if _, delErr := s.q.ExecContext(ctx,
			s.q.Rebind(`DELETE FROM steam_account_apps WHERE account_id = ? AND app_id = ?`), accountID, appID); delErr != nil {
			return fmt.Errorf("store: 标记 Steam 账号许可失败: %w", err)
		}
		_, err = s.q.ExecContext(ctx,
			s.q.Rebind(`INSERT INTO steam_account_apps (account_id, app_id, source, verified_at) VALUES (?, ?, ?, ?)`),
			accountID, appID, source, nowMillis())
	}
	if err != nil {
		return fmt.Errorf("store: 标记 Steam 账号许可失败: %w", err)
	}
	return nil
}

// ListSteamAccountApps 列出账号拥有的 AppID。
func (s *Store) ListSteamAccountApps(ctx context.Context, accountID string) ([]domain.SteamAccountApp, error) {
	var out []domain.SteamAccountApp
	if err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT account_id, app_id, source, verified_at FROM steam_account_apps
			WHERE account_id = ? ORDER BY app_id`), accountID); err != nil {
		return nil, fmt.Errorf("store: 查询 Steam 账号许可失败: %w", err)
	}
	return out, nil
}

// SteamAccountOwnsApp 判断账号是否拥有某个 AppID。
func (s *Store) SteamAccountOwnsApp(ctx context.Context, accountID string, appID int64) (bool, error) {
	var n int
	if err := s.q.GetContext(ctx, &n,
		s.q.Rebind(`SELECT COUNT(*) FROM steam_account_apps WHERE account_id = ? AND app_id = ?`),
		accountID, appID); err != nil {
		return false, fmt.Errorf("store: 查询 Steam 账号许可失败: %w", err)
	}
	return n > 0, nil
}

// ---------- steam_apps（商店元数据缓存） ----------

// UpsertSteamApps 批量写入/更新商店元数据。
func (s *Store) UpsertSteamApps(ctx context.Context, apps []domain.SteamAppMeta) error {
	if len(apps) == 0 {
		return nil
	}
	cols := []string{"app_id", "name", "image", "platforms", "app_type", "updated_at"}
	stmt := s.upsertSQL("steam_apps", cols, []string{"app_id"}, []string{"name", "image", "platforms", "app_type", "updated_at"})
	for i := range apps {
		if apps[i].UpdatedAt == 0 {
			apps[i].UpdatedAt = nowMillis()
		}
		if _, err := s.q.ExecContext(ctx, stmt,
			apps[i].AppID, apps[i].Name, apps[i].Image, apps[i].Platforms, apps[i].AppType, apps[i].UpdatedAt); err != nil {
			return fmt.Errorf("store: 写入 Steam 商店元数据失败: %w", err)
		}
	}
	return nil
}

// GetSteamApps 按 AppID 批量读取商店元数据。
func (s *Store) GetSteamApps(ctx context.Context, appIDs []int64) (map[int64]domain.SteamAppMeta, error) {
	out := make(map[int64]domain.SteamAppMeta, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	const chunk = 500
	for start := 0; start < len(appIDs); start += chunk {
		end := start + chunk
		if end > len(appIDs) {
			end = len(appIDs)
		}
		part := appIDs[start:end]
		args := make([]any, 0, len(part))
		for _, id := range part {
			args = append(args, id)
		}
		query := `SELECT app_id, name, image, platforms, app_type, updated_at FROM steam_apps WHERE app_id IN (` +
			placeholders(len(part)) + `)`
		var rows []domain.SteamAppMeta
		if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
			return nil, fmt.Errorf("store: 查询 Steam 商店元数据失败: %w", err)
		}
		for _, row := range rows {
			out[row.AppID] = row
		}
	}
	return out, nil
}

// ---------- steam_account_leases ----------

// ListActiveSteamLeases 返回某账号当前未过期的租约。
func (s *Store) ListActiveSteamLeases(ctx context.Context, accountID string) ([]domain.SteamAccountLease, error) {
	now := nowMillis()
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_account_leases SET state = ? WHERE state = ? AND expires_at <= ?`),
		domain.SteamLeaseExpired, domain.SteamLeaseActive, now); err != nil {
		return nil, fmt.Errorf("store: 清理过期 Steam 租约失败: %w", err)
	}
	var out []domain.SteamAccountLease
	if err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT id, account_id, holder, holder_id, state, acquired_at, expires_at, released_at, last_error
			FROM steam_account_leases WHERE account_id = ? AND state = ? ORDER BY acquired_at`),
		accountID, domain.SteamLeaseActive); err != nil {
		return nil, fmt.Errorf("store: 查询 Steam 租约失败: %w", err)
	}
	return out, nil
}

// AcquireSteamLease 在账号并发上限内创建租约。
//
// 用事务 + 计数保证同一账号不会并发跑多个 SteamCMD；SQLite 的 BEGIN IMMEDIATE 与
// MySQL 的写锁都能把并发请求串行化。
func (s *Store) AcquireSteamLease(ctx context.Context, l *domain.SteamAccountLease, maxConcurrent int) error {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if l.ID == "" {
		l.ID = newID()
	}
	now := nowMillis()
	if l.AcquiredAt == 0 {
		l.AcquiredAt = now
	}
	if l.ExpiresAt <= now {
		l.ExpiresAt = now + 30*60*1000
	}
	if l.State == "" {
		l.State = domain.SteamLeaseActive
	}

	run := func(tx *Store) error {
		// MySQL 下先把账号行锁住，避免两个并发请求同时看到 0 个活跃租约。
		// SQLite 没有 SELECT ... FOR UPDATE，由单写者事务语义保证。
		if tx.dialect == DialectMySQL {
			var locked string
			if err := tx.q.GetContext(ctx, &locked,
				tx.q.Rebind(`SELECT id FROM steam_accounts WHERE id = ? FOR UPDATE`), l.AccountID); err != nil {
				return notFound(err, func() *apperr.Error {
					return apperr.New("steam.account_not_found", 404).WithArg("id", l.AccountID)
				})
			}
		}
		if _, err := tx.q.ExecContext(ctx,
			tx.q.Rebind(`UPDATE steam_account_leases SET state = ? WHERE account_id = ? AND state = ? AND expires_at <= ?`),
			domain.SteamLeaseExpired, l.AccountID, domain.SteamLeaseActive, now); err != nil {
			return fmt.Errorf("store: 清理过期 Steam 租约失败: %w", err)
		}
		var n int
		if err := tx.q.GetContext(ctx, &n,
			tx.q.Rebind(`SELECT COUNT(*) FROM steam_account_leases WHERE account_id = ? AND state = ?`),
			l.AccountID, domain.SteamLeaseActive); err != nil {
			return fmt.Errorf("store: 统计 Steam 租约失败: %w", err)
		}
		if n >= maxConcurrent {
			return apperr.New("steam.account_busy", 409).WithArg("account_id", l.AccountID)
		}
		_, err := tx.q.NamedExecContext(ctx,
			`INSERT INTO steam_account_leases (id, account_id, holder, holder_id, state, acquired_at, expires_at, released_at, last_error)
			 VALUES (:id, :account_id, :holder, :holder_id, :state, :acquired_at, :expires_at, :released_at, :last_error)`, l)
		if err != nil {
			return fmt.Errorf("store: 创建 Steam 租约失败: %w", err)
		}
		return nil
	}
	if s.root == nil {
		return run(s)
	}
	return s.Tx(ctx, run)
}

// ReleaseSteamLease 释放租约（幂等）。
func (s *Store) ReleaseSteamLease(ctx context.Context, id string) error {
	_, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_account_leases SET state = ?, released_at = ? WHERE id = ? AND state = ?`),
		domain.SteamLeaseReleased, nowMillis(), id, domain.SteamLeaseActive)
	if err != nil {
		return fmt.Errorf("store: 释放 Steam 租约失败: %w", err)
	}
	return nil
}

// ExpireAllActiveSteamLeases 把全部活跃租约置为过期。
//
// 服务端是单实例；启动时所有残留的 SteamCMD 进程都已经不存在，活跃租约一定是
// 上次进程崩溃/被杀留下的。不清理的话，账号会被占用到 TTL 到期，表现为
// "一直登录不上、一直 steam.account_busy"。
func (s *Store) ExpireAllActiveSteamLeases(ctx context.Context, reason string) (int64, error) {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_account_leases SET state = ?, released_at = ?, last_error = ?
			WHERE state = ?`),
		domain.SteamLeaseExpired, nowMillis(), reason, domain.SteamLeaseActive)
	if err != nil {
		return 0, fmt.Errorf("store: 清理活跃 Steam 租约失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReleaseSteamLeasesByAccount 强制释放某账号的全部活跃租约（管理员手动恢复）。
func (s *Store) ReleaseSteamLeasesByAccount(ctx context.Context, accountID, reason string) (int64, error) {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_account_leases SET state = ?, released_at = ?, last_error = ?
			WHERE account_id = ? AND state = ?`),
		domain.SteamLeaseReleased, nowMillis(), reason, accountID, domain.SteamLeaseActive)
	if err != nil {
		return 0, fmt.Errorf("store: 释放 Steam 账号租约失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RenewSteamLease 续租。
func (s *Store) RenewSteamLease(ctx context.Context, id string, expiresAt int64) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE steam_account_leases SET expires_at = ? WHERE id = ? AND state = ?`),
		expiresAt, id, domain.SteamLeaseActive)
	if err != nil {
		return fmt.Errorf("store: 续租 Steam 租约失败: %w", err)
	}
	return mustAffect(res, apperr.New("steam.lease_not_found", 404).WithArg("id", id))
}

// CountSteamGroupsUsingAccount 统计引用某账号的 Steam 分组数量（删除前提示用）。
func (s *Store) CountSteamGroupsUsingAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	if err := s.q.GetContext(ctx, &n,
		s.q.Rebind(`SELECT COUNT(*) FROM repo_groups WHERE steam_account_id = ?`), accountID); err != nil {
		return 0, fmt.Errorf("store: 统计 Steam 账号引用失败: %w", err)
	}
	return n, nil
}

// ---------- repo_groups ----------

const repoGroupCols = `id, name, kind, description, steam_account_id, target_platform,
	auto_update, update_schedule, update_window, max_concurrent, storage_id, access_mode, created_at, updated_at`

type repoGroupRow struct {
	ID             string         `db:"id"`
	Name           string         `db:"name"`
	Kind           string         `db:"kind"`
	Description    string         `db:"description"`
	SteamAccountID sql.NullString `db:"steam_account_id"`
	TargetPlatform string         `db:"target_platform"`
	AutoUpdate     bool           `db:"auto_update"`
	UpdateSchedule string         `db:"update_schedule"`
	UpdateWindow   string         `db:"update_window"`
	MaxConcurrent  int            `db:"max_concurrent"`
	StorageID      string         `db:"storage_id"`
	AccessMode     string         `db:"access_mode"`
	CreatedAt      int64          `db:"created_at"`
	UpdatedAt      int64          `db:"updated_at"`
}

func (r repoGroupRow) toDomain() *domain.RepoGroup {
	g := &domain.RepoGroup{
		ID:             r.ID,
		Name:           r.Name,
		Kind:           domain.RepoGroupKind(r.Kind),
		Description:    r.Description,
		TargetPlatform: r.TargetPlatform,
		AutoUpdate:     r.AutoUpdate,
		UpdateSchedule: r.UpdateSchedule,
		UpdateWindow:   r.UpdateWindow,
		MaxConcurrent:  r.MaxConcurrent,
		StorageID:      r.StorageID,
		AccessMode:     domain.RepoGroupAccessMode(r.AccessMode),
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if r.SteamAccountID.Valid {
		v := r.SteamAccountID.String
		g.SteamAccountID = &v
	}
	if g.AccessMode == "" {
		g.AccessMode = domain.RepoGroupAccessAll
	}
	return g
}

// CreateRepoGroup 创建分组。
func (s *Store) CreateRepoGroup(ctx context.Context, g *domain.RepoGroup) error {
	if g.ID == "" {
		g.ID = newID()
	}
	now := nowMillis()
	g.CreatedAt, g.UpdatedAt = now, now
	if g.Kind == "" {
		g.Kind = domain.RepoGroupGeneral
	}
	if g.TargetPlatform == "" {
		g.TargetPlatform = "windows"
	}
	if g.MaxConcurrent <= 0 {
		g.MaxConcurrent = 1
	}
	if g.AccessMode == "" {
		g.AccessMode = domain.RepoGroupAccessAll
	}
	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO repo_groups (`+repoGroupCols+`)
		 VALUES (:id, :name, :kind, :description, :steam_account_id, :target_platform,
		         :auto_update, :update_schedule, :update_window, :max_concurrent, :storage_id,
		         :access_mode, :created_at, :updated_at)`,
		map[string]any{
			"id":               g.ID,
			"name":             g.Name,
			"kind":             string(g.Kind),
			"description":      g.Description,
			"steam_account_id": nullableStringPtr(g.SteamAccountID),
			"target_platform":  g.TargetPlatform,
			"auto_update":      g.AutoUpdate,
			"update_schedule":  g.UpdateSchedule,
			"update_window":    g.UpdateWindow,
			"max_concurrent":   g.MaxConcurrent,
			"storage_id":       g.StorageID,
			"access_mode":      string(g.AccessMode),
			"created_at":       g.CreatedAt,
			"updated_at":       g.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.New("group.name_taken", 409).WithArg("name", g.Name)
	}
	if err != nil {
		return fmt.Errorf("store: 创建分组失败: %w", err)
	}
	return nil
}

func nullableStringPtr(v *string) any {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	return *v
}

// GetRepoGroup 按 ID 查询分组。
func (s *Store) GetRepoGroup(ctx context.Context, id string) (*domain.RepoGroup, error) {
	var row repoGroupRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+repoGroupCols+` FROM repo_groups WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("group.not_found", 404).WithArg("id", id)
		})
	}
	return row.toDomain(), nil
}

// GetRepoGroupByName 按名称查询分组。
func (s *Store) GetRepoGroupByName(ctx context.Context, name string) (*domain.RepoGroup, error) {
	var row repoGroupRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+repoGroupCols+` FROM repo_groups WHERE name = ?`), name)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("group.not_found", 404).WithArg("name", name)
		})
	}
	return row.toDomain(), nil
}

// ListRepoGroups 列出分组。
func (s *Store) ListRepoGroups(ctx context.Context, kind domain.RepoGroupKind) ([]domain.RepoGroup, error) {
	query := `SELECT ` + repoGroupCols + ` FROM repo_groups`
	args := []any{}
	if kind != "" {
		query += ` WHERE kind = ?`
		args = append(args, string(kind))
	}
	query += ` ORDER BY name`
	var rows []repoGroupRow
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询分组失败: %w", err)
	}
	out := make([]domain.RepoGroup, 0, len(rows))
	for i := range rows {
		out = append(out, *rows[i].toDomain())
	}
	return out, nil
}

// UpdateRepoGroup 更新分组。
func (s *Store) UpdateRepoGroup(ctx context.Context, g *domain.RepoGroup) error {
	g.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE repo_groups SET
		 name=:name, kind=:kind, description=:description, steam_account_id=:steam_account_id,
		 target_platform=:target_platform, auto_update=:auto_update,
		 update_schedule=:update_schedule, update_window=:update_window,
		 max_concurrent=:max_concurrent, storage_id=:storage_id,
		 access_mode=:access_mode, updated_at=:updated_at
		 WHERE id=:id`,
		map[string]any{
			"id":               g.ID,
			"name":             g.Name,
			"kind":             string(g.Kind),
			"description":      g.Description,
			"steam_account_id": nullableStringPtr(g.SteamAccountID),
			"target_platform":  g.TargetPlatform,
			"auto_update":      g.AutoUpdate,
			"update_schedule":  g.UpdateSchedule,
			"update_window":    g.UpdateWindow,
			"max_concurrent":   g.MaxConcurrent,
			"storage_id":       g.StorageID,
			"access_mode":      string(g.AccessMode),
			"updated_at":       g.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.New("group.name_taken", 409).WithArg("name", g.Name)
	}
	if err != nil {
		return fmt.Errorf("store: 更新分组失败: %w", err)
	}
	return mustAffect(res, apperr.New("group.not_found", 404).WithArg("id", g.ID))
}

// DeleteRepoGroup 删除分组。
func (s *Store) DeleteRepoGroup(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM repo_groups WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除分组失败: %w", err)
	}
	return mustAffect(res, apperr.New("group.not_found", 404).WithArg("id", id))
}

// ListRepoGroupMembers 列出"仅指定用户可访问"分组的成员。
func (s *Store) ListRepoGroupMembers(ctx context.Context, groupID string) ([]domain.RepoGroupMember, error) {
	var out []domain.RepoGroupMember
	if err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT group_id, user_id, created_at FROM repo_group_members
			WHERE group_id = ? ORDER BY created_at`), groupID); err != nil {
		return nil, fmt.Errorf("store: 查询分组成员失败: %w", err)
	}
	return out, nil
}

// ReplaceRepoGroupMembers 覆盖式设置分组成员。
func (s *Store) ReplaceRepoGroupMembers(ctx context.Context, groupID string, userIDs []string) error {
	run := func(tx *Store) error {
		if _, err := tx.q.ExecContext(ctx,
			tx.q.Rebind(`DELETE FROM repo_group_members WHERE group_id = ?`), groupID); err != nil {
			return fmt.Errorf("store: 清理分组成员失败: %w", err)
		}
		now := nowMillis()
		seen := map[string]bool{}
		for _, userID := range userIDs {
			userID = strings.TrimSpace(userID)
			if userID == "" || seen[userID] {
				continue
			}
			seen[userID] = true
			if _, err := tx.q.ExecContext(ctx,
				tx.q.Rebind(`INSERT INTO repo_group_members (group_id, user_id, created_at) VALUES (?, ?, ?)`),
				groupID, userID, now); err != nil {
				return fmt.Errorf("store: 写入分组成员失败: %w", err)
			}
		}
		return nil
	}
	if s.root == nil {
		return run(s)
	}
	return s.Tx(ctx, run)
}

// IsRepoGroupMember 判断用户是否在分组指定成员列表里。
func (s *Store) IsRepoGroupMember(ctx context.Context, groupID, userID string) (bool, error) {
	var n int
	if err := s.q.GetContext(ctx, &n,
		s.q.Rebind(`SELECT COUNT(*) FROM repo_group_members WHERE group_id = ? AND user_id = ?`),
		groupID, userID); err != nil {
		return false, fmt.Errorf("store: 查询分组成员失败: %w", err)
	}
	return n > 0, nil
}

// ---------- games ----------

const gameCols = `id, group_id, repo_id, app_id, app_name, install_dir, build_id, available_build_id,
	target_platform, steam_account_id, auto_update, status, last_checked_at, last_updated_at,
	last_error, created_at, updated_at`

type gameRow struct {
	ID               string         `db:"id"`
	GroupID          string         `db:"group_id"`
	RepoID           sql.NullString `db:"repo_id"`
	AppID            int64          `db:"app_id"`
	AppName          string         `db:"app_name"`
	InstallDir       string         `db:"install_dir"`
	BuildID          string         `db:"build_id"`
	AvailableBuildID string         `db:"available_build_id"`
	TargetPlatform   string         `db:"target_platform"`
	SteamAccountID   sql.NullString `db:"steam_account_id"`
	AutoUpdate       sql.NullBool   `db:"auto_update"`
	Status           string         `db:"status"`
	LastCheckedAt    int64          `db:"last_checked_at"`
	LastUpdatedAt    int64          `db:"last_updated_at"`
	LastError        string         `db:"last_error"`
	CreatedAt        int64          `db:"created_at"`
	UpdatedAt        int64          `db:"updated_at"`
}

func (r gameRow) toDomain() *domain.Game {
	g := &domain.Game{
		ID:               r.ID,
		GroupID:          r.GroupID,
		AppID:            r.AppID,
		AppName:          r.AppName,
		InstallDir:       r.InstallDir,
		BuildID:          r.BuildID,
		AvailableBuildID: r.AvailableBuildID,
		TargetPlatform:   r.TargetPlatform,
		Status:           r.Status,
		LastCheckedAt:    r.LastCheckedAt,
		LastUpdatedAt:    r.LastUpdatedAt,
		LastError:        r.LastError,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
	if r.RepoID.Valid {
		v := r.RepoID.String
		g.RepoID = &v
	}
	if r.SteamAccountID.Valid {
		v := r.SteamAccountID.String
		g.SteamAccountID = &v
	}
	if r.AutoUpdate.Valid {
		v := r.AutoUpdate.Bool
		g.AutoUpdate = &v
	}
	return g
}

// CreateGame 创建游戏记录。
func (s *Store) CreateGame(ctx context.Context, g *domain.Game) error {
	if g.ID == "" {
		g.ID = newID()
	}
	now := nowMillis()
	g.CreatedAt, g.UpdatedAt = now, now
	if g.Status == "" {
		g.Status = domain.GameStatusReady
	}
	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO games (`+gameCols+`)
		 VALUES (:id, :group_id, :repo_id, :app_id, :app_name, :install_dir, :build_id, :available_build_id,
		         :target_platform, :steam_account_id, :auto_update, :status, :last_checked_at, :last_updated_at,
		         :last_error, :created_at, :updated_at)`,
		map[string]any{
			"id":                 g.ID,
			"group_id":           g.GroupID,
			"repo_id":            nullableStringPtr(g.RepoID),
			"app_id":             g.AppID,
			"app_name":           g.AppName,
			"install_dir":        g.InstallDir,
			"build_id":           g.BuildID,
			"available_build_id": g.AvailableBuildID,
			"target_platform":    g.TargetPlatform,
			"steam_account_id":   nullableStringPtr(g.SteamAccountID),
			"auto_update":        nullableBoolPtr(g.AutoUpdate),
			"status":             g.Status,
			"last_checked_at":    g.LastCheckedAt,
			"last_updated_at":    g.LastUpdatedAt,
			"last_error":         g.LastError,
			"created_at":         g.CreatedAt,
			"updated_at":         g.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.New("game.exists", 409).WithArg("app_id", g.AppID).WithArg("repo_id", derefString(g.RepoID))
	}
	if err != nil {
		return fmt.Errorf("store: 创建游戏失败: %w", err)
	}
	return nil
}

func nullableBoolPtr(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// GetGame 按 ID 查询游戏。
func (s *Store) GetGame(ctx context.Context, id string) (*domain.Game, error) {
	var row gameRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+gameCols+` FROM games WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("game.not_found", 404).WithArg("id", id)
		})
	}
	return row.toDomain(), nil
}

// GetGameByRepoID 按存储库查询游戏。
func (s *Store) GetGameByRepoID(ctx context.Context, repoID string) (*domain.Game, error) {
	var row gameRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+gameCols+` FROM games WHERE repo_id = ?`), repoID)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("game.not_found", 404).WithArg("repo_id", repoID)
		})
	}
	return row.toDomain(), nil
}

// ListGamesByGroup 列出分组内游戏。
func (s *Store) ListGamesByGroup(ctx context.Context, groupID string) ([]domain.Game, error) {
	var rows []gameRow
	if err := s.q.SelectContext(ctx, &rows,
		s.q.Rebind(`SELECT `+gameCols+` FROM games WHERE group_id = ? ORDER BY app_id`), groupID); err != nil {
		return nil, fmt.Errorf("store: 查询游戏列表失败: %w", err)
	}
	out := make([]domain.Game, 0, len(rows))
	for i := range rows {
		out = append(out, *rows[i].toDomain())
	}
	return out, nil
}

// ListAllGames 列出全部游戏（自动更新调度用）。
func (s *Store) ListAllGames(ctx context.Context) ([]domain.Game, error) {
	var rows []gameRow
	if err := s.q.SelectContext(ctx, &rows,
		`SELECT `+gameCols+` FROM games ORDER BY group_id, app_id`); err != nil {
		return nil, fmt.Errorf("store: 查询游戏列表失败: %w", err)
	}
	out := make([]domain.Game, 0, len(rows))
	for i := range rows {
		out = append(out, *rows[i].toDomain())
	}
	return out, nil
}

// UpdateGame 更新游戏记录。
func (s *Store) UpdateGame(ctx context.Context, g *domain.Game) error {
	g.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE games SET
		 group_id=:group_id, repo_id=:repo_id, app_id=:app_id, app_name=:app_name,
		 install_dir=:install_dir, build_id=:build_id, available_build_id=:available_build_id,
		 target_platform=:target_platform, steam_account_id=:steam_account_id, auto_update=:auto_update,
		 status=:status, last_checked_at=:last_checked_at, last_updated_at=:last_updated_at,
		 last_error=:last_error, updated_at=:updated_at
		 WHERE id=:id`,
		map[string]any{
			"id":                 g.ID,
			"group_id":           g.GroupID,
			"repo_id":            nullableStringPtr(g.RepoID),
			"app_id":             g.AppID,
			"app_name":           g.AppName,
			"install_dir":        g.InstallDir,
			"build_id":           g.BuildID,
			"available_build_id": g.AvailableBuildID,
			"target_platform":    g.TargetPlatform,
			"steam_account_id":   nullableStringPtr(g.SteamAccountID),
			"auto_update":        nullableBoolPtr(g.AutoUpdate),
			"status":             g.Status,
			"last_checked_at":    g.LastCheckedAt,
			"last_updated_at":    g.LastUpdatedAt,
			"last_error":         g.LastError,
			"updated_at":         g.UpdatedAt,
		})
	if err != nil {
		return fmt.Errorf("store: 更新游戏失败: %w", err)
	}
	return mustAffect(res, apperr.New("game.not_found", 404).WithArg("id", g.ID))
}

// DeleteGame 删除游戏记录（不删除存储库本身）。
func (s *Store) DeleteGame(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM games WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除游戏失败: %w", err)
	}
	return mustAffect(res, apperr.New("game.not_found", 404).WithArg("id", id))
}

// ResetUpdatingGames 把进程崩溃后遗留的 updating 状态改回 error。
//
// 更新任务不在 job 表里（它是一条后台流水线），进程重启后没有自动恢复机制；
// 不重置的话这些游戏会永远被调度器跳过。
func (s *Store) ResetUpdatingGames(ctx context.Context) (int64, error) {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE games SET status = ?, last_error = ?, updated_at = ?
			WHERE status = ?`),
		domain.GameStatusError, "steam.update_interrupted", nowMillis(), domain.GameStatusUpdating)
	if err != nil {
		return 0, fmt.Errorf("store: 重置中断的游戏更新失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
