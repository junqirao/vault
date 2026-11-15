package app

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/domain"
	"vault/internal/secret"
	"vault/internal/store"
)

func newSteamTestApp(t *testing.T) (*App, *store.Store, *secret.Cipher) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		Dialect:    store.DialectSQLite,
		SQLitePath: filepath.Join(t.TempDir(), "vault.db"),
	})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	box, err := secret.NewCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("构造 Cipher 失败: %v", err)
	}
	app := New(Deps{Store: st, Cipher: box, Log: slog.Default()})
	return app, st, box
}

func TestSteamTOTPCodeStandardVector(t *testing.T) {
	// RFC 6238 的 SHA-1 测试密钥（ASCII "12345678901234567890"）。
	// Steam 使用同一动态截断值，但映射为 5 位 Steam 字符表，而不是 6 位数字。
	const secretBase32 = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	code, err := steamTOTPCode(secretBase32, time.Unix(59, 0))
	if err != nil {
		t.Fatalf("生成 Steam Guard 失败: %v", err)
	}
	if code != "PV9M4" {
		t.Fatalf("Steam Guard = %s，期望 PV9M4", code)
	}

	// SteamDesktopAuthenticator 的 shared_secret 是 base64，也必须能识别。
	const secretBase64 = "MTIzNDU2Nzg5MDEyMzQ1Njc4OTA="
	code, err = steamTOTPCode(secretBase64, time.Unix(59, 0))
	if err != nil || code != "PV9M4" {
		t.Fatalf("base64 Steam Guard = %s, err=%v，期望 PV9M4", code, err)
	}
}

func TestAuditWriteSurvivesCanceledContext(t *testing.T) {
	app, st, _ := newSteamTestApp(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	// 业务 ctx 已经取消（客户端断开/长任务超时）时，审计仍应落库。
	app.writeAudit(canceled, &domain.AuditLog{
		UserID:   "user-1",
		Action:   "test.audit.after_cancel",
		Resource: "test",
		Result:   domain.AuditResultOK,
	})
	logs, err := st.ListAuditLogs(context.Background(), "user-1", "test.audit.after_cancel", 10, 0)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("取消后的审计应写入 1 条，实际 %d 条", len(logs))
	}
}

func TestSteamAppNameIsPlaceholder(t *testing.T) {
	cases := map[string]bool{
		"App 12345":    true,
		"  App 7  ":    true,
		"App 123 Beta": false,
		"Portal 2":     false,
		"":             false,
	}
	for name, want := range cases {
		if got := steamAppNameIsPlaceholder(name); got != want {
			t.Fatalf("%q placeholder = %v，期望 %v", name, got, want)
		}
	}
}

func TestSteamAccountUsableWithCachedSession(t *testing.T) {
	a := &domain.SteamAccount{
		Username:    "content-01",
		Status:      domain.SteamAccountStatusNeeds2FA,
		SessionHome: t.TempDir(),
	}
	if err := steamAccountUsableForUpdate(a); err == nil {
		t.Fatal("没有缓存登录标记时 needs_2fa 账号不应可用")
	}
	writeSteamLoginMarker(a.SessionHome, a.Username)
	if err := steamAccountUsableForUpdate(a); err != nil {
		t.Fatalf("有缓存登录标记时应允许尝试更新: %v", err)
	}
	a.Status = domain.SteamAccountStatusDisabled
	if err := steamAccountUsableForUpdate(a); err == nil {
		t.Fatal("禁用账号即使有缓存也不应可用")
	}
}

func TestSteamAccountCredentialEncryptionAndLease(t *testing.T) {
	ctx := context.Background()
	app, st, box := newSteamTestApp(t)
	svc := app.SteamAccounts()

	account, err := svc.Create(ctx, CreateSteamAccountInput{
		Username:      "content-01",
		DisplayName:   "内容账号",
		Credential:    "super-secret-password",
		GuardType:     domain.SteamGuardTOTP,
		GuardSecret:   "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	stored, err := st.GetSteamAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if string(stored.CredentialCipher) == "super-secret-password" {
		t.Fatal("密码不得以明文落库")
	}
	plain, err := box.DecryptString(stored.CredentialCipher)
	if err != nil || plain != "super-secret-password" {
		t.Fatalf("密码解密失败: %q %v", plain, err)
	}
	guard, err := box.DecryptString(stored.GuardSecretCipher)
	if err != nil || guard == "" {
		t.Fatalf("Guard secret 解密失败: %v", err)
	}

	// 账号初始为 needs_2fa，不能用于更新；置为 active 后可获取租约。
	if err := steamAccountUsableForUpdate(stored); err == nil {
		t.Fatal("needs_2fa 账号不应可用于更新")
	}
	stored.Status = domain.SteamAccountStatusActive
	if err := st.UpdateSteamAccount(ctx, stored); err != nil {
		t.Fatalf("更新账号状态失败: %v", err)
	}
	lease, err := svc.AcquireLease(ctx, account.ID, "game_update", "job-1", time.Minute)
	if err != nil {
		t.Fatalf("获取租约失败: %v", err)
	}
	if _, err := svc.AcquireLease(ctx, account.ID, "game_update", "job-2", time.Minute); err == nil {
		t.Fatal("同一账号不应并发获取两个租约")
	}
	if err := svc.ReleaseLease(ctx, lease.ID); err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}

	// 许可查询：只认已同步的 AppID。
	if err := st.ReplaceSteamAccountApps(ctx, account.ID, []domain.SteamAccountApp{{AppID: 730}}); err != nil {
		t.Fatalf("写入许可失败: %v", err)
	}
	available, err := svc.Available(ctx, 730)
	if err != nil {
		t.Fatalf("查询可用账号失败: %v", err)
	}
	if len(available) != 1 || !available[0].Available || !available[0].OwnsApp {
		t.Fatalf("730 应有可用账号: %+v", available)
	}
	available, err = svc.Available(ctx, 440)
	if err != nil {
		t.Fatalf("查询可用账号失败: %v", err)
	}
	if len(available) != 1 || available[0].Available {
		t.Fatalf("440 不应有可用账号: %+v", available)
	}
}

func TestSteamAccountUpdatePreservesSessionWhenGuardUnchanged(t *testing.T) {
	ctx := context.Background()
	app, st, _ := newSteamTestApp(t)
	svc := app.SteamAccounts()
	account, err := svc.Create(ctx, CreateSteamAccountInput{
		Username: "content-edit", Credential: "secret",
		GuardType: domain.SteamGuardTOTP, GuardSecret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	account.Status = domain.SteamAccountStatusActive
	account.SessionState = domain.SteamSessionValid
	if err := st.UpdateSteamAccount(ctx, account); err != nil {
		t.Fatalf("写入会话状态失败: %v", err)
	}

	name := "新显示名"
	guard := domain.SteamGuardTOTP
	kind := domain.SteamCredentialPassword
	updated, err := svc.Update(ctx, UpdateSteamAccountInput{
		ID: account.ID, DisplayName: &name, GuardType: &guard, CredentialKind: &kind,
	}, "admin")
	if err != nil {
		t.Fatalf("更新账号失败: %v", err)
	}
	if updated.Status != domain.SteamAccountStatusActive || updated.SessionState != domain.SteamSessionValid {
		t.Fatalf("未修改凭据时不应重置登录态: status=%s session=%s", updated.Status, updated.SessionState)
	}
	if updated.DisplayName != name {
		t.Fatalf("显示名未更新: %q", updated.DisplayName)
	}
}

func TestSteamOwnedGamesUsesCachedMetadata(t *testing.T) {
	ctx := context.Background()
	app, st, _ := newSteamTestApp(t)
	svc := app.SteamAccounts()
	account, err := svc.Create(ctx, CreateSteamAccountInput{
		Username: "content-games", Credential: "secret", MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	if err := st.ReplaceSteamAccountApps(ctx, account.ID, []domain.SteamAccountApp{
		{AppID: 730}, {AppID: 440},
	}); err != nil {
		t.Fatalf("写入许可失败: %v", err)
	}
	if err := st.UpsertSteamApps(ctx, []domain.SteamAppMeta{
		{AppID: 730, Name: "Counter-Strike 2", Image: "cs2.jpg", Platforms: "windows,linux"},
		{AppID: 440, Name: "Team Fortress 2", Platforms: "windows"},
	}); err != nil {
		t.Fatalf("写入元数据失败: %v", err)
	}
	// 两个 AppID 都有缓存名称，不应发起网络请求。
	games, err := svc.OwnedGames(ctx, account.ID, "", "", false)
	if err != nil {
		t.Fatalf("读取已拥有游戏失败: %v", err)
	}
	if len(games) != 2 || games[0].Name != "Counter-Strike 2" || games[1].Name != "Team Fortress 2" {
		t.Fatalf("已拥有游戏列表错误: %+v", games)
	}
	if !games[0].Owned || games[0].Image != "cs2.jpg" || len(games[0].Platforms) != 2 {
		t.Fatalf("730 元数据错误: %+v", games[0])
	}
}

func TestSteamOwnedGamesOnlyReturnsBaseGamesWithStoreInfo(t *testing.T) {
	ctx := context.Background()
	app, st, _ := newSteamTestApp(t)
	svc := app.SteamAccounts()
	account, err := svc.Create(ctx, CreateSteamAccountInput{
		Username: "content-games", Credential: "secret", MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	ids := []int64{730, 378648, 1000, 620}
	apps := make([]domain.SteamAccountApp, 0, len(ids))
	for _, id := range ids {
		apps = append(apps, domain.SteamAccountApp{AppID: id})
	}
	if err := st.ReplaceSteamAccountApps(ctx, account.ID, apps); err != nil {
		t.Fatalf("写入许可失败: %v", err)
	}
	if err := st.UpsertSteamApps(ctx, []domain.SteamAppMeta{
		{AppID: 730, Name: "Counter-Strike 2", AppType: 0},
		{AppID: 378648, Name: "The Witcher 3: Blood and Wine", AppType: 4},
		{AppID: 1000, Name: "Blender", AppType: 6},
		{AppID: 620, Name: "Portal 2", AppType: 0},
	}); err != nil {
		t.Fatalf("写入元数据失败: %v", err)
	}
	games, err := svc.OwnedGames(ctx, account.ID, "", "", false)
	if err != nil {
		t.Fatalf("读取已拥有游戏失败: %v", err)
	}
	if len(games) != 2 || games[0].AppID != 730 || games[1].AppID != 620 {
		t.Fatalf("应只返回游戏本体，实际: %+v", games)
	}
}

func TestImportClientGamesMergesLicenseAndMetadata(t *testing.T) {
	ctx := context.Background()
	app, st, _ := newSteamTestApp(t)
	svc := app.SteamAccounts()
	account, err := svc.Create(ctx, CreateSteamAccountInput{
		Username: "content-client", Credential: "secret", MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	if err := st.UpsertSteamApps(ctx, []domain.SteamAppMeta{
		{AppID: 730, Name: "旧名称", Image: "local.jpg", Platforms: "windows,linux"},
	}); err != nil {
		t.Fatalf("预置元数据失败: %v", err)
	}

	count, err := svc.ImportClientGames(ctx, account.ID, []ImportClientGame{
		{AppID: 730, Name: "Counter-Strike 2", Installed: true},
		{AppID: 570, Name: "Dota 2"},
	}, "admin")
	if err != nil || count != 2 {
		t.Fatalf("客户端库同步失败: count=%d err=%v", count, err)
	}
	apps, err := st.ListSteamAccountApps(ctx, account.ID)
	if err != nil || len(apps) != 2 {
		t.Fatalf("许可清单 = %d, err=%v", len(apps), err)
	}
	meta, err := st.GetSteamApps(ctx, []int64{730, 570})
	if err != nil {
		t.Fatalf("读取元数据失败: %v", err)
	}
	if meta[730].Name != "Counter-Strike 2" || meta[730].Image != "local.jpg" || meta[730].Platforms != "windows,linux" {
		t.Fatalf("导入不应清空已有封面/平台: %+v", meta[730])
	}
	if meta[570].Name != "Dota 2" {
		t.Fatalf("570 名称 = %q", meta[570].Name)
	}
}

func TestSteamSplitPlatformsNeverReturnsNil(t *testing.T) {
	// nil 切片会被编码成 JSON null；前端按数组使用时会抛异常，导致整页错误边界。
	if got := splitPlatforms(""); got == nil || len(got) != 0 {
		t.Fatalf("空平台应返回空数组而不是 nil: %#v", got)
	}
	if got := splitPlatforms("windows,linux"); len(got) != 2 {
		t.Fatalf("平台解析结果不正确: %#v", got)
	}
}

func TestSteamLeaseForceReleaseAndStartupReset(t *testing.T) {
	ctx := context.Background()
	app, st, _ := newSteamTestApp(t)
	svc := app.SteamAccounts()
	a, err := svc.Create(ctx, CreateSteamAccountInput{
		Username: "content-lease", Credential: "secret", MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	a.Status = domain.SteamAccountStatusActive
	if err := st.UpdateSteamAccount(ctx, a); err != nil {
		t.Fatalf("更新账号失败: %v", err)
	}
	lease, err := svc.AcquireLease(ctx, a.ID, "test", "job", time.Minute)
	if err != nil {
		t.Fatalf("获取租约失败: %v", err)
	}
	_ = lease
	if n, _ := svc.ActiveLeaseCount(ctx, a.ID); n != 1 {
		t.Fatalf("活跃租约 = %d，期望 1", n)
	}
	if n, err := svc.ForceReleaseLeases(ctx, a.ID, "admin"); err != nil || n != 1 {
		t.Fatalf("强制释放失败: n=%d err=%v", n, err)
	}
	if n, _ := svc.ActiveLeaseCount(ctx, a.ID); n != 0 {
		t.Fatalf("强制释放后活跃租约 = %d，期望 0", n)
	}

	// 模拟进程重启后的残留租约。
	if _, err := svc.AcquireLease(ctx, a.ID, "test", "job2", time.Hour); err != nil {
		t.Fatalf("再次获取租约失败: %v", err)
	}
	if n, err := svc.ResetStaleLeases(ctx); err != nil || n != 1 {
		t.Fatalf("启动清理失败: n=%d err=%v", n, err)
	}
	if n, _ := svc.ActiveLeaseCount(ctx, a.ID); n != 0 {
		t.Fatalf("启动清理后活跃租约 = %d，期望 0", n)
	}
}
