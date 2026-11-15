package store

import (
	"context"
	"testing"

	"vault/internal/domain"
)

func TestSteamAccountGroupAndGameStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	for _, table := range []string{"steam_accounts", "steam_account_apps", "steam_account_leases", "steam_apps", "repo_groups", "games"} {
		if !tableExists(t, s, table) {
			t.Fatalf("迁移后缺少表 %s", table)
		}
	}

	// 账号 CRUD + 密文列。
	a := &domain.SteamAccount{
		Username:          "content-01",
		DisplayName:       "内容账号",
		Status:            domain.SteamAccountStatusActive,
		CredentialCipher:  []byte("cipher-text"),
		CredentialKind:    domain.SteamCredentialPassword,
		SessionState:      domain.SteamSessionValid,
		GuardType:         domain.SteamGuardTOTP,
		GuardSecretCipher: []byte("guard-cipher"),
		MaxConcurrent:     1,
	}
	if err := s.CreateSteamAccount(ctx, a); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	got, err := s.GetSteamAccount(ctx, a.ID)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if string(got.CredentialCipher) != "cipher-text" || string(got.GuardSecretCipher) != "guard-cipher" {
		t.Fatalf("密文列未正确往返")
	}
	if got.Status != domain.SteamAccountStatusActive {
		t.Fatalf("状态 = %q", got.Status)
	}

	// 许可覆盖与查询。
	apps := []domain.SteamAccountApp{
		{AppID: 730, Source: "licenses_print"},
		{AppID: 440, Source: "licenses_print"},
	}
	if err := s.ReplaceSteamAccountApps(ctx, a.ID, apps); err != nil {
		t.Fatalf("写入许可失败: %v", err)
	}
	owns, err := s.SteamAccountOwnsApp(ctx, a.ID, 730)
	if err != nil || !owns {
		t.Fatalf("应拥有 730: owns=%v err=%v", owns, err)
	}
	owns, err = s.SteamAccountOwnsApp(ctx, a.ID, 999)
	if err != nil || owns {
		t.Fatalf("不应拥有 999: owns=%v err=%v", owns, err)
	}

	// 商店元数据缓存：批量 upsert 后可批量读取，并支持覆盖更新。
	meta := []domain.SteamAppMeta{
		{AppID: 730, Name: "Counter-Strike 2", Image: "a.jpg", Platforms: "windows,linux", AppType: 0},
		{AppID: 440, Name: "Team Fortress 2", Platforms: "windows", AppType: 4},
	}
	if err := s.UpsertSteamApps(ctx, meta); err != nil {
		t.Fatalf("写入商店元数据失败: %v", err)
	}
	gotMeta, err := s.GetSteamApps(ctx, []int64{730, 440, 999})
	if err != nil {
		t.Fatalf("读取商店元数据失败: %v", err)
	}
	if gotMeta[730].Name != "Counter-Strike 2" || gotMeta[730].Platforms != "windows,linux" || gotMeta[730].AppType != 0 {
		t.Fatalf("730 元数据错误: %+v", gotMeta[730])
	}
	if gotMeta[440].AppType != 4 {
		t.Fatalf("440 app_type = %d，期望 4", gotMeta[440].AppType)
	}
	if len(gotMeta) != 2 {
		t.Fatalf("元数据条数 = %d，期望 2", len(gotMeta))
	}
	if err := s.UpsertSteamApps(ctx, []domain.SteamAppMeta{{AppID: 730, Name: "CS2", Platforms: "windows"}}); err != nil {
		t.Fatalf("更新商店元数据失败: %v", err)
	}
	gotMeta, err = s.GetSteamApps(ctx, []int64{730})
	if err != nil || gotMeta[730].Name != "CS2" {
		t.Fatalf("元数据覆盖更新失败: %+v err=%v", gotMeta[730], err)
	}

	// 租约：maxConcurrent=1 时第二次必须失败，释放后可以再次获取。
	lease1 := &domain.SteamAccountLease{AccountID: a.ID, Holder: "test", HolderID: "job-1", State: domain.SteamLeaseActive}
	if err := s.AcquireSteamLease(ctx, lease1, 1); err != nil {
		t.Fatalf("第一次获取租约失败: %v", err)
	}
	lease2 := &domain.SteamAccountLease{AccountID: a.ID, Holder: "test", HolderID: "job-2", State: domain.SteamLeaseActive}
	if err := s.AcquireSteamLease(ctx, lease2, 1); err == nil {
		t.Fatalf("并发租约应被拒绝")
	}
	if err := s.ReleaseSteamLease(ctx, lease1.ID); err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}
	if err := s.AcquireSteamLease(ctx, lease2, 1); err != nil {
		t.Fatalf("释放后应能再次获取租约: %v", err)
	}
	_ = s.ReleaseSteamLease(ctx, lease2.ID)

	// 分组与游戏。
	g := &domain.RepoGroup{
		Name: "单机游戏", Kind: domain.RepoGroupSteam, SteamAccountID: &a.ID,
		TargetPlatform: "windows", AutoUpdate: true, MaxConcurrent: 1,
	}
	if err := s.CreateRepoGroup(ctx, g); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	gotGroup, err := s.GetRepoGroupByName(ctx, g.Name)
	if err != nil {
		t.Fatalf("按名称读取分组失败: %v", err)
	}
	if gotGroup.SteamAccountID == nil || *gotGroup.SteamAccountID != a.ID || !gotGroup.AutoUpdate {
		t.Fatalf("分组字段未正确往返: %+v", gotGroup)
	}

	repoID := "repo-1"
	game := &domain.Game{
		GroupID: g.ID, RepoID: &repoID, AppID: 730, AppName: "CS2",
		TargetPlatform: "windows", Status: domain.GameStatusReady,
	}
	if err := s.CreateGame(ctx, game); err != nil {
		t.Fatalf("创建游戏失败: %v", err)
	}
	gotGame, err := s.GetGameByRepoID(ctx, repoID)
	if err != nil {
		t.Fatalf("按库读取游戏失败: %v", err)
	}
	if gotGame.AppID != 730 || gotGame.RepoID == nil || *gotGame.RepoID != repoID {
		t.Fatalf("游戏字段未正确往返: %+v", gotGame)
	}
	if gotGame.AutoUpdate != nil {
		t.Fatalf("未设置的 auto_update 应为 nil（继承分组）")
	}
	auto := false
	gotGame.AutoUpdate = &auto
	if err := s.UpdateGame(ctx, gotGame); err != nil {
		t.Fatalf("更新游戏失败: %v", err)
	}
	gotGame, _ = s.GetGame(ctx, game.ID)
	if gotGame.AutoUpdate == nil || *gotGame.AutoUpdate {
		t.Fatalf("auto_update=false 未正确往返: %+v", gotGame.AutoUpdate)
	}
}
