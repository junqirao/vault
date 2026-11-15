package app

import (
	"context"
	"strings"
	"time"

	"vault/internal/domain"
)

// steamAutoCheckMinInterval 是同一游戏两次自动版本检查之间的最小间隔。
//
// 调度器 tick 默认 15 分钟；每次 tick 都登录 SteamCMD 检查所有游戏代价太高。
// 这里把"检查版本"限制在每小时一次；真正有更新时仍会立即触发更新任务。
const steamAutoCheckMinInterval = time.Hour

// steamAutoCheckMinIntervalInWindow 是处于自动更新时间窗口内时的检查间隔。
//
// 时间窗口可能只有 1 小时；如果仍沿用 1 小时的全局间隔，窗口开始前刚检查过的游戏
// 会整段窗口都跳过，看起来像"自动更新没执行"。窗口内允许更频繁地确认一次。
const steamAutoCheckMinIntervalInWindow = 15 * time.Minute

// StartSteamScheduler 启动 Steam 自动更新调度循环。
//
// 它只负责"发现可更新游戏并入队"，具体下载/合并由 SteamGameService 的更新任务执行。
// 账号模块只在这里被用于存储库内容更新，不允许任何玩家登录用途。
func (a *App) StartSteamScheduler(ctx context.Context) {
	interval := a.raw().Steam.AutoUpdateInterval.Std()
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	go func() {
		// 进程重启后，上次遗留的 updating 状态没有自动恢复机制，先收口成 error，
		// 让调度器后续还能重新尝试（否则这些游戏会被永久跳过）。
		if n, err := a.Store.ResetUpdatingGames(ctx); err != nil {
			a.Log.Warn("Steam 自动更新：重置中断状态失败", "error", err)
		} else if n > 0 {
			a.Log.Info("Steam 自动更新：已重置中断的更新状态", "count", n)
		}
		// 启动后稍等，避免和启动对账/建库任务抢占资源。
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// 启动时强制校验一次账号会话：如果快照已失效，界面能尽早提示验证，
		// 而不是等到 24 小时后或更新游戏时才暴露。
		a.refreshSteamLicensesOnce(ctx, true)
		a.runSteamAutoUpdateOnce(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.refreshSteamLicensesOnce(ctx, false)
				a.runSteamAutoUpdateOnce(ctx)
			}
		}
	}()
}

// steamLicenseRefreshInterval 是后台自动刷新账号许可的间隔。
//
// 用户买了新游戏后不需要手动点“同步许可”；服务端会定期用已保存的
// SteamCMD 会话（或 TOTP）静默刷新。只有会话真正失效、又没配置 TOTP 时，
// 账号才会回到 needs_2fa，让界面提示用户补一次验证码。
const steamLicenseRefreshInterval = 24 * time.Hour

// steamLicenseRetryInterval 是上次刷新失败后的重试间隔，避免频繁登录触发风控。
const steamLicenseRetryInterval = 6 * time.Hour

func (a *App) refreshSteamLicensesOnce(ctx context.Context, force bool) {
	if !a.raw().Steam.EnabledOrDefault() {
		return
	}
	accounts, err := a.SteamAccounts().List(ctx, 500, 0)
	if err != nil {
		a.Log.Warn("Steam 许可刷新：读取账号失败", "error", err)
		return
	}
	now := time.Now()
	for i := range accounts {
		acc := &accounts[i]
		if acc.Status == domain.SteamAccountStatusDisabled {
			continue
		}
		// 没有 TOTP 密钥、又已经明确需要验证码时，自动登录只会反复失败；
		// 等用户在界面上完成一次验证后再恢复自动刷新。
		if acc.Status == domain.SteamAccountStatusNeeds2FA && !acc.HasGuardSecret() {
			continue
		}
		interval := steamLicenseRefreshInterval
		if acc.LastError != "" {
			interval = steamLicenseRetryInterval
		}
		if !force && acc.LastCheckedAt > 0 && now.Sub(time.UnixMilli(acc.LastCheckedAt)) < interval {
			continue
		}
		// 账号正在被更新任务占用时不要插队：既避免抢 SteamCMD 闸门，
		// 也避免在长更新期间白占一个租约等待。
		if leases, leaseErr := a.SteamAccounts().ActiveLeaseCount(ctx, acc.ID); leaseErr == nil && leases > 0 {
			continue
		}
		if _, err := a.SteamAccounts().SyncLicenses(ctx, acc.ID, "", "system"); err != nil {
			a.Log.Warn("Steam 许可刷新：同步失败",
				"account_id", acc.ID, "username", acc.Username, "error", err)
			continue
		}
		a.Log.Info("Steam 许可刷新：同步完成", "account_id", acc.ID, "username", acc.Username)
	}
}

func (a *App) runSteamAutoUpdateOnce(ctx context.Context) {
	if !a.raw().Steam.EnabledOrDefault() {
		return
	}
	groups, err := a.RepoGroups().List(ctx, domain.RepoGroupSteam)
	if err != nil {
		a.Log.Warn("Steam 自动更新：读取分组失败", "error", err)
		return
	}
	now := time.Now()
	for i := range groups {
		g := &groups[i]
		if !g.AutoUpdate {
			continue
		}
		if !updateWindowOK(g.UpdateWindow, now) {
			continue
		}
		// 账号需要人工验证/已禁用时，继续尝试只会反复失败并把游戏标红。
		// 等许可自动刷新或管理员验证后，下一轮会自动恢复。
		if g.SteamAccountID != nil && *g.SteamAccountID != "" {
			account, accErr := a.SteamAccounts().Get(ctx, *g.SteamAccountID)
			if accErr != nil || account.Status != domain.SteamAccountStatusActive {
				continue
			}
		}
		games, err := a.SteamGames().ListGames(ctx, g.ID)
		if err != nil {
			a.Log.Warn("Steam 自动更新：读取游戏失败", "group_id", g.ID, "error", err)
			continue
		}
		for j := range games {
			game := &games[j]
			if !a.RepoGroups().EffectiveAutoUpdate(g, game) {
				continue
			}
			if game.Status == domain.GameStatusUpdating || game.Status == domain.GameStatusPaused {
				// 暂停/停止是用户显式动作，自动更新不应该把它重新拉起来。
				continue
			}
			if game.Status == domain.GameStatusError {
				switch game.LastError {
				case "steam.account_no_license", "steam.account_disabled", "steam.account_not_active",
					"steam.account_missing":
					// 这些错误需要管理员先处理账号/许可；继续重试只会反复失败。
					continue
				}
				// 其它错误（网络/SteamCMD 临时失败）做 30 分钟退避，避免每个 tick 都重新登录。
				if game.LastCheckedAt > 0 && now.Sub(time.UnixMilli(game.LastCheckedAt)) < 30*time.Minute {
					continue
				}
			}
			if game.RepoID == nil || *game.RepoID == "" {
				continue
			}
			// 距上次检查不足最小间隔时，若已有可用版本则直接入队，否则跳过。
			// 有明确时间窗口时用更短的间隔，确保窗口内至少能检查一次。
			checkInterval := steamAutoCheckMinInterval
			if strings.TrimSpace(g.UpdateWindow) != "" {
				checkInterval = steamAutoCheckMinIntervalInWindow
			}
			due := game.LastCheckedAt == 0 || now.Sub(time.UnixMilli(game.LastCheckedAt)) >= checkInterval
			if due {
				checked, checkErr := a.SteamGames().CheckGame(ctx, game.ID)
				if checkErr != nil {
					a.Log.Warn("Steam 自动更新：检查版本失败",
						"game_id", game.ID, "app_id", game.AppID, "error", checkErr)
					continue
				}
				game = checked
			}
			if game.AvailableBuildID == "" || game.AvailableBuildID == game.BuildID {
				continue
			}
			if _, err := a.SteamGames().TriggerUpdate(ctx, game.ID, "system"); err != nil {
				a.Log.Warn("Steam 自动更新：触发更新失败",
					"game_id", game.ID, "app_id", game.AppID, "error", err)
			}
		}
	}
}
