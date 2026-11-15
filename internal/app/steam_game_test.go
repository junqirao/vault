package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

func TestSteamUpdateCommandArgsOrder(t *testing.T) {
	args := steamUpdateCommandArgs("user", "pass", "", "windows", `D:\stage`, 730, false, true)
	got := strings.Join(args, " ")
	force := strings.Index(got, "+force_install_dir")
	login := strings.Index(got, "+login")
	if force < 0 || login < 0 || force > login {
		t.Fatalf("+force_install_dir 必须在 +login 之前: %s", got)
	}
	if !strings.Contains(got, "+force_install_dir D:\\stage") {
		t.Fatalf("缺少 force_install_dir: %s", got)
	}
	if !strings.Contains(got, "+app_update 730") {
		t.Fatalf("缺少 app_update: %s", got)
	}

	full := steamUpdateCommandArgs("user", "pass", "ABCDE", "windows", `D:\stage`, 730, true, false)
	fullText := strings.Join(full, " ")
	if strings.Index(fullText, "+force_install_dir") > strings.Index(fullText, "+set_steam_guard_code") {
		t.Fatalf("完整登录也应先设置安装目录: %s", fullText)
	}
}

func TestSteamUpdateCompleted(t *testing.T) {
	out := "Update state (0x101) committing, progress: 49.13 (1 / 2)\nSuccess! App '2344520' fully installed.\nUnloading Steam API...OK"
	if !steamUpdateCompleted(out, 2344520) {
		t.Fatal("应识别 SteamCMD 完成标记")
	}
	if steamUpdateCompleted(out, 730) {
		t.Fatal("AppID 不匹配时不应认为完成")
	}
	if !steamUpdateCompleted("Success! App '730' fully installed.", 730) {
		t.Fatal("单行完成标记也应识别")
	}
	if !steamUpdateCompleted("Success! App '2344520' already up to date.", 2344520) {
		t.Fatal("already up to date 也应视为完成")
	}
	if steamUpdateCompleted("Update state (0x61) downloading, progress: 10.0 (1 / 10)", 730) {
		t.Fatal("只有下载进度不应认为安装完成")
	}
}

func TestSteamGameDownloadSampleSpeedAndOutput(t *testing.T) {
	svc := &SteamGameService{
		progress: make(map[string]SteamGameProgress),
		outputs:  make(map[string][]string),
	}
	svc.setProgress("game-1", "downloading", 0)
	// 伪造上一秒的采样点：1 秒内新增 1000 字节 => 约 1000 B/s。
	svc.progressMu.Lock()
	p := svc.progress["game-1"]
	p.speedAt = time.Now().Add(-time.Second)
	p.speedBytes = 1000
	p.BytesDone = 1000
	svc.progress["game-1"] = p
	svc.progressMu.Unlock()

	svc.setDownloadProgress("game-1", steamDownloadSample{
		Percent: 10, BytesDone: 2000, BytesTotal: 20000, Stage: "downloading",
	})
	p = svc.Progress("game-1")
	if p.Percent != 10 || p.BytesDone != 2000 || p.BytesTotal != 20000 || p.Stage != "downloading" {
		t.Fatalf("进度合并错误: %+v", p)
	}
	if p.SpeedBytesPerSec < 500 || p.SpeedBytesPerSec > 1500 {
		t.Fatalf("速度估算 = %d B/s，期望约 1000", p.SpeedBytesPerSec)
	}

	for i := 0; i < steamGameOutputMaxLines+50; i++ {
		svc.appendOutput("game-1", fmt.Sprintf("line-%d", i))
	}
	lines := svc.Output("game-1")
	if len(lines) != steamGameOutputMaxLines {
		t.Fatalf("输出保留 %d 行，期望 %d", len(lines), steamGameOutputMaxLines)
	}
	if lines[len(lines)-1] != fmt.Sprintf("line-%d", steamGameOutputMaxLines+49) {
		t.Fatalf("应保留最新输出，最后一行 = %q", lines[len(lines)-1])
	}
}

func TestSteamGameProgressRegistry(t *testing.T) {
	svc := &SteamGameService{
		progress:    make(map[string]SteamGameProgress),
		cancels:     make(map[string]context.CancelFunc),
		cancelModes: make(map[string]string),
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.registerCancel("game-1", cancel)
	svc.setProgress("game-1", "downloading", 42.5)

	p := svc.Progress("game-1")
	if p.Percent != 42.5 || !p.CanStop || p.Phase != "downloading" {
		t.Fatalf("下载阶段进度错误: %+v", p)
	}

	svc.setProgress("game-1", "merging", 0)
	if svc.Progress("game-1").CanStop {
		t.Fatal("合并阶段不应允许停止")
	}

	svc.setCancelMode("game-1", SteamUpdateControlPause)
	if mode := svc.takeCancelMode("game-1"); mode != SteamUpdateControlPause {
		t.Fatalf("取消模式 = %q", mode)
	}
	svc.unregisterCancel("game-1")
	svc.clearProgress("game-1")
	if p := svc.Progress("game-1"); p.Phase != "" || p.CanStop {
		t.Fatalf("清理后仍残留进度: %+v", p)
	}
}

// TestSelfServiceAllocateForSharedRepo 锁定共享 Steam 游戏库的自由挂载权限：
// 普通用户可以在 AllowSelfService=true 时为自己分配差异盘；
// 不带该标志时仍必须满足 owner/manage 权限。
func TestSelfServiceAllocateForSharedRepo(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newRepoFlowService(t)
	owner := createFlowOwner(t, st)

	repo, disk, err := svc.Create(ctx, CreateRepoInput{
		Name:      "shared-game",
		Mode:      domain.RepoModeShared,
		OwnerID:   owner.ID,
		SizeBytes: 64 << 20,
		ClientOS:  "windows",
	})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	diskSvc := &DiskService{Deps: svc.Deps, Repos: svc}
	diskSvc.finishDiskPrepare(ctx, disk)
	// 测试替身只走权限分支：把母盘状态补齐到"可派生"。
	disk.State = domain.DiskStateReady
	if err := st.UpdateDisk(ctx, disk); err != nil {
		t.Fatalf("更新测试母盘状态失败: %v", err)
	}
	repo, err = st.GetRepository(ctx, repo.ID)
	if err != nil {
		t.Fatalf("读取测试存储库失败: %v", err)
	}
	repo.State = domain.RepoStateActive
	repo.Meta.Prepare = nil
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("更新测试存储库状态失败: %v", err)
	}

	player := &domain.User{Username: "player-1", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(ctx, player); err != nil {
		t.Fatalf("建普通用户失败: %v", err)
	}

	alloc, err := svc.Allocate(ctx, AllocateInput{
		RepoID:           repo.ID,
		UserID:           player.ID,
		OperatorID:       player.ID,
		AllowSelfService: true,
	})
	if err != nil {
		t.Fatalf("普通用户应能自服务分配共享游戏库: %v", err)
	}
	if alloc.UserID != player.ID || alloc.RepoID != repo.ID {
		t.Fatalf("分配结果不正确: %+v", alloc)
	}

	other := &domain.User{Username: "player-2", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(ctx, other); err != nil {
		t.Fatalf("建第二个普通用户失败: %v", err)
	}
	if _, err := svc.Allocate(ctx, AllocateInput{
		RepoID:     repo.ID,
		UserID:     other.ID,
		OperatorID: other.ID,
	}); err == nil {
		t.Fatal("未开启自服务时普通用户应被拒绝")
	} else if e, ok := apperr.As(err); !ok || e.Code != "auth.forbidden" {
		t.Fatalf("应拒绝为 auth.forbidden，实际: %v", err)
	}
}

// TestRepoGroupAccessMode 锁定"仅指定用户可访问"的判定：
// all → 任意用户；selected → 仅成员；超级管理员始终可访问。
func TestRepoGroupAccessMode(t *testing.T) {
	ctx := context.Background()
	app, st, _ := newSteamTestApp(t)
	member := &domain.User{Username: "member", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(ctx, member); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	outsider := &domain.User{Username: "outsider", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(ctx, outsider); err != nil {
		t.Fatalf("建非成员失败: %v", err)
	}

	group, err := app.RepoGroups().Create(ctx, CreateRepoGroupInput{
		Name: "restricted", Kind: domain.RepoGroupGeneral, TargetPlatform: "windows",
		AccessMode: domain.RepoGroupAccessSelected,
		MemberIDs:  []string{member.ID},
	}, "admin")
	if err != nil {
		t.Fatalf("创建受限分组失败: %v", err)
	}
	if ok, _ := app.RepoGroups().CanAccess(ctx, group, member.ID, false); !ok {
		t.Fatal("成员应可访问")
	}
	if ok, _ := app.RepoGroups().CanAccess(ctx, group, outsider.ID, false); ok {
		t.Fatal("非成员不应可访问")
	}
	if ok, _ := app.RepoGroups().CanAccess(ctx, group, outsider.ID, true); !ok {
		t.Fatal("超级管理员应可访问")
	}

	allMode := domain.RepoGroupAccessAll
	if _, err := app.RepoGroups().Update(ctx, UpdateRepoGroupInput{
		ID: group.ID, AccessMode: &allMode,
	}, "admin"); err != nil {
		t.Fatalf("切换为全部可访问失败: %v", err)
	}
	group, err = app.RepoGroups().Get(ctx, group.ID)
	if err != nil {
		t.Fatalf("回读分组失败: %v", err)
	}
	if group.AccessMode != domain.RepoGroupAccessAll {
		t.Fatalf("access_mode = %q，期望 all", group.AccessMode)
	}
	if ok, _ := app.RepoGroups().CanAccess(ctx, group, outsider.ID, false); !ok {
		t.Fatal("all 模式下任意用户应可访问")
	}
}
