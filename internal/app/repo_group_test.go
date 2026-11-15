package app

import (
	"context"
	"testing"

	"vault/internal/domain"
)

func TestSteamGroupAutoUpdateSettings(t *testing.T) {
	ctx := context.Background()
	app, _, _ := newSteamTestApp(t)
	account, err := app.SteamAccounts().Create(ctx, CreateSteamAccountInput{
		Username: "auto-update", Credential: "secret", MaxConcurrent: 1,
	}, "admin")
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}

	// 不传 auto_update 时，Steam 分组默认开启自动更新。
	group, err := app.RepoGroups().Create(ctx, CreateRepoGroupInput{
		Name: "steam-auto", Kind: domain.RepoGroupSteam,
		SteamAccountID: &account.ID, TargetPlatform: "windows",
		UpdateWindow: "03:00-06:00",
	}, "admin")
	if err != nil {
		t.Fatalf("创建 Steam 分组失败: %v", err)
	}
	if !group.AutoUpdate {
		t.Fatal("Steam 分组未显式设置时应默认开启自动更新")
	}
	if group.UpdateWindow != "03:00-06:00" {
		t.Fatalf("自动更新时间窗口 = %q", group.UpdateWindow)
	}

	off := false
	window := "22:00-23:30"
	group, err = app.RepoGroups().Update(ctx, UpdateRepoGroupInput{
		ID: group.ID, AutoUpdate: &off, UpdateWindow: &window,
	}, "admin")
	if err != nil {
		t.Fatalf("更新 Steam 分组失败: %v", err)
	}
	if group.AutoUpdate {
		t.Fatal("显式关闭自动更新后不应被强制改回开启")
	}
	if group.UpdateWindow != window {
		t.Fatalf("更新后时间窗口 = %q，期望 %q", group.UpdateWindow, window)
	}
}
