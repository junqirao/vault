package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWaitServerConnected 锁定"服务端连上之前不执行自动挂载"的等待语义。
//
// 真实反馈："自动挂载要连接到服务器之后才会执行，否则会报错"。拿到会话 ≠ 连得上
// （SystemInfo 还没成功、事件流还没建立），此时挂载只会留下一串失败记录。
func TestWaitServerConnected(t *testing.T) {
	t.Run("已连接时零延迟就绪", func(t *testing.T) {
		a := newServerConnectTestAgent(t, nil)
		key := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t"})
		a.store.SetServerConnected(key, true, "")

		start := time.Now()
		if !a.waitServerConnected(context.Background(), key) {
			t.Fatal("已连接时应判定为就绪")
		}
		if elapsed := time.Since(start); elapsed > autoMountWaitInterval {
			t.Fatalf("已连接时不该再等待，实际耗时 %v", elapsed)
		}
	})

	t.Run("等待期间连上则就绪", func(t *testing.T) {
		a := newServerConnectTestAgent(t, nil)
		key := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t"})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		go func() {
			time.Sleep(autoMountWaitInterval / 2)
			a.store.SetServerConnected(key, true, "")
		}()

		if !a.waitServerConnected(ctx, key) {
			t.Fatal("等待期间服务端连上后应判定为就绪")
		}
	})

	t.Run("始终连不上则超时未就绪", func(t *testing.T) {
		a := newServerConnectTestAgent(t, nil)
		key := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t"})
		ctx, cancel := context.WithTimeout(context.Background(), autoMountWaitInterval*2)
		defer cancel()

		if a.waitServerConnected(ctx, key) {
			t.Fatal("从未连上时不应判定为就绪（否则又会在没连上时去挂载）")
		}
	})
}

// TestSameSessionDuringWait 锁定"等待期间会话被换掉就不再动手"：同一台重新登录 / 退出登录
// 都属于会话已更换，旧任务的自动挂载不该插一脚。
//
// 多服务端下"另一台推来会话"**不算**更换：本台没变，等待任务该照常继续 —— 这是本次改造的
// 关键点之一，早先"全局只有一个会话"的判定会把别人的登录当成自己的会话被换掉。
func TestSameSessionDuringWait(t *testing.T) {
	a := newServerConnectTestAgent(t, nil)
	key := a.store.SetSession("", &Session{ServerURL: "https://10.0.0.1:8443", Token: "t1"})
	want, ok := a.store.Session(key)
	if !ok {
		t.Fatal("推送会话后应能读回")
	}
	if !sameSession(a.store, key, want) {
		t.Fatal("未被更换的会话应判定为同一个")
	}

	// 另一台服务端推来会话：本台会话未变，等待任务应继续。
	other := a.store.SetSession("", &Session{ServerURL: "https://10.0.0.2:8443", Token: "t9"})
	if other == key || other == "" {
		t.Fatalf("另一台服务端应各自一把键，实际 %q vs %q", other, key)
	}
	if !sameSession(a.store, key, want) {
		t.Fatal("另一台推来会话不该让本台的等待任务失效")
	}

	// 同一台服务端重新登录：地址没变但令牌换了，同样是"会话已更换"。
	a.store.SetSession(key, &Session{ServerURL: "https://10.0.0.1:8443", Token: "t2"})
	if sameSession(a.store, key, want) {
		t.Fatal("换了令牌的会话不该被判定为同一个")
	}

	// 退出登录后会话被清除：更不该继续挂着。
	a.store.ClearSession(key)
	if sameSession(a.store, key, want) {
		t.Fatal("会话被清除后不该判定为同一个")
	}
}

// TestRecordedMountRequestPrefPolicy 锁定"本地记录 + 每库配置"的处置规则：
//
//	没有该库的配置   → 按记录恢复（全局 auto_mount 的老行为）；
//	配置里关了自动挂载 → 不恢复（用户明确表态优先于历史记录）；
//	配置里开了自动挂载 → 恢复，且形态/目录以配置为准（记录里是上次的旧值）。
//
// 真实反馈驱动的点：只在配置里改了形态却没重挂时，下次启动不能被记录里的旧形态带回去。
func TestRecordedMountRequestPrefPolicy(t *testing.T) {
	record := MountState{
		AllocationID: "alloc-1",
		RepoID:       "repo-a",
		RepoName:     "样品库",
		MountMode:    mountModeLetter,
		MountPath:    `E:\`,
	}
	cases := []struct {
		name      string
		group     string
		prefs     map[string]RepoMountPref
		groups    map[string]RepoMountPref
		wantMount bool
		wantMode  string
		wantPath  string
	}{
		{
			name:      "没有该库的配置 → 按记录恢复",
			prefs:     map[string]RepoMountPref{"repo-b": {AutoMount: true}},
			wantMount: true,
			wantMode:  mountModeLetter,
			wantPath:  `E:\`,
		},
		{
			name:      "配置关掉自动挂载 → 不恢复",
			prefs:     map[string]RepoMountPref{"repo-a": {MountMode: mountModeDirectory, AutoMount: false}},
			wantMount: false,
		},
		{
			name: "配置开了自动挂载并指定形态目录 → 以配置为准",
			prefs: map[string]RepoMountPref{"repo-a": {
				MountMode: mountModeDirectory,
				MountDir:  `D:\vault\样品库`,
				AutoMount: true,
			}},
			wantMount: true,
			wantMode:  mountModeDirectory,
			wantPath:  `D:\vault\样品库`,
		},
		{
			name:      "配置开了自动挂载但没表态形态 → 沿用记录",
			prefs:     map[string]RepoMountPref{"repo-a": {AutoMount: true}},
			wantMount: true,
			wantMode:  mountModeLetter,
			wantPath:  `E:\`,
		},
		{
			// 真实反馈："没有按照我的组配置进行启动后自动挂载"：分组配置接管了本库时，
			// 单库配置里那个（界面上已置灰的）auto_mount=false 不能压过分组的 auto_mount=true。
			name:  "分组配置接管并开了自动挂载 → 按分组恢复",
			group: "财务",
			prefs: map[string]RepoMountPref{"repo-a": {AutoMount: false}},
			groups: map[string]RepoMountPref{"财务": {
				UseForRepos: true,
				AutoMount:   true,
				MountMode:   mountModeDirectory,
				MountDir:    `F:\vault\共享`,
			}},
			wantMount: true,
			wantMode:  mountModeDirectory,
			wantPath:  `F:\vault\共享`,
		},
		{
			name:      "分组配置接管但关掉了自动挂载 → 不恢复",
			group:     "财务",
			prefs:     map[string]RepoMountPref{"repo-a": {AutoMount: true}},
			groups:    map[string]RepoMountPref{"财务": {UseForRepos: true, AutoMount: false}},
			wantMount: false,
		},
		{
			name:      "分组没勾选本库 → 退回单库配置",
			group:     "财务",
			prefs:     map[string]RepoMountPref{"repo-a": {AutoMount: false}},
			groups:    map[string]RepoMountPref{"财务": {UseForRepos: true, AutoMount: true, UseRepos: []string{"repo-b"}}},
			wantMount: false,
		},
		{
			name:      "分组没开「使用分组配置」 → 退回单库配置并恢复",
			group:     "财务",
			groups:    map[string]RepoMountPref{"财务": {UseForRepos: false, AutoMount: false}},
			wantMount: true,
			wantMode:  mountModeLetter,
			wantPath:  `E:\`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := record
			rec.RepoGroup = c.group
			req, ok := recordedMountRequest(rec, c.prefs, c.groups)
			if ok != c.wantMount {
				t.Fatalf("是否恢复 = %v，期望 %v", ok, c.wantMount)
			}
			if !ok {
				return
			}
			if req.AllocationID != record.AllocationID {
				t.Fatalf("allocation_id = %q，期望 %q", req.AllocationID, record.AllocationID)
			}
			if req.MountMode != c.wantMode || req.MountPath != c.wantPath {
				t.Fatalf("形态/目录 = %q/%q，期望 %q/%q", req.MountMode, req.MountPath, c.wantMode, c.wantPath)
			}
		})
	}
}

// TestConfiguredAutoMountTargetsSkipsManuallyUnmounted 锁定"用户手动卸载过的库本次运行不再自动挂载"：
//
// 卸载成功会连本地挂载记录一起删掉（见 mountEngine.unmountLocked），于是这个库在配置眼里
// 又变回"从来没挂过、但 auto_mount 开着"，任何一次会话建立（客户端推会话 / 证书免密登录 /
// 令牌定时续期都走 setSession）都会把它立刻挂回来 —— 用户看到的是"点了卸载没用"。
// 手动卸载过之后，configuredAutoMountTargets 必须把这个库排除掉。
func TestConfiguredAutoMountTargetsSkipsManuallyUnmounted(t *testing.T) {
	var guard manualUnmountGuard
	guard.block("repo-manual")

	prefs := map[string]RepoMountPref{
		"repo-auto":     {AutoMount: true},
		"repo-off":      {AutoMount: false},
		"repo-recorded": {AutoMount: true},
		"repo-manual":   {AutoMount: true},
	}
	// repo-recorded 已有本地记录 → 由 restoreRecordedMounts 负责，这里不重复处理。
	recorded := map[string]struct{}{"repo-recorded": {}}
	repos := map[string]repoMountInfo{
		"repo-auto":     {Name: "自动"},
		"repo-off":      {Name: "关掉"},
		"repo-recorded": {Name: "已挂"},
		"repo-manual":   {Name: "手动卸载"},
	}

	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(prefs, nil, repos, recorded, guard.blocked)); got != "repo-auto" {
		t.Fatalf("手动卸载 repo-manual 后该自动挂载的库 = %q，期望 %q", got, "repo-auto")
	}

	// 没手动卸载过的库必须照旧自动挂载：修复不能演变成"自动挂载整体失效"。
	want := "repo-auto,repo-manual,repo-recorded"
	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(prefs, nil, repos, map[string]struct{}{}, func(string) bool { return false })); got != want {
		t.Fatalf("未手动卸载时该自动挂载的库 = %q，期望 %q", got, want)
	}

	// 只挡被点过卸载的那一个库，别的库不受牵连。
	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(
		map[string]RepoMountPref{"repo-manual": {AutoMount: true}, "repo-other": {AutoMount: true}},
		nil,
		map[string]repoMountInfo{"repo-manual": {}, "repo-other": {}},
		map[string]struct{}{},
		guard.blocked,
	)); got != "repo-other" {
		t.Fatalf("黑名单只应挡 repo-manual，实际自动挂载 = %q，期望 %q", got, "repo-other")
	}
}

// TestConfiguredAutoMountTargetsHonoursGroupPref 锁定"分组配置能带动组内库自动挂载"。
//
// 真实反馈："没有按照我的组配置进行启动后自动挂载"。分组配置（勾了「使用分组配置」）
// 就是组内库的生效配置，界面也按它显示；启动自动挂载必须同一套口径：
//   - 组内库即使**没有单独配置**也要被挂上（这是旧实现最大的漏点：只遍历 cfg.RepoMounts）；
//   - 分组优先于单库配置（组内 auto_mount=true 时，单库里那个已置灰的 false 不算数）；
//   - 未勾选的库、其它分组的库、以及没开「使用分组配置」的分组都不受影响。
func TestConfiguredAutoMountTargetsHonoursGroupPref(t *testing.T) {
	repoPrefs := map[string]RepoMountPref{
		"repo-unconfigured": {AutoMount: false}, // 界面上已被分组配置置灰
		"repo-other-group":  {AutoMount: true},  // 不在本组：仍按自己的配置
	}
	groupPrefs := map[string]RepoMountPref{
		"财务": {
			UseForRepos: true,
			AutoMount:   true,
			UseRepos:    []string{"repo-unconfigured", "repo-picked"},
		},
		"研发": {UseForRepos: false, AutoMount: false},
	}
	repos := map[string]repoMountInfo{
		"repo-unconfigured": {Name: "未单独配置", Group: "财务"},
		"repo-picked":       {Name: "勾选", Group: "财务"},
		"repo-unpicked":     {Name: "未勾选", Group: "财务"},
		"repo-other-group":  {Name: "别的组", Group: "研发"},
	}

	got := joinAutoMountTargetIDs(configuredAutoMountTargets(repoPrefs, groupPrefs, repos, nil, func(string) bool { return false }))
	want := "repo-other-group,repo-picked,repo-unconfigured"
	if got != want {
		t.Fatalf("分组配置驱动的自动挂载 = %q，期望 %q", got, want)
	}

	// 分组没开「使用分组配置」时，组内库仍各用各的配置（repo-unpicked 没有任何配置 → 不挂）。
	passive := map[string]RepoMountPref{"财务": {UseForRepos: false, AutoMount: true}}
	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(repoPrefs, passive, repos, nil, func(string) bool { return false })); got != "repo-other-group" {
		t.Fatalf("未启用分组配置时自动挂载 = %q，期望 %q", got, "repo-other-group")
	}
}

// TestManualUnmountGuard 锁定黑名单本身的行为：按库 ID 记、前后空格归一、重复记账幂等、
// 没有库 ID 时记不下（也不能反过来挡住所有库）。
func TestManualUnmountGuard(t *testing.T) {
	var guard manualUnmountGuard
	if guard.blocked("repo-a") {
		t.Fatal("没卸载过的库不应该被挡住")
	}
	if guard.blocked("") || guard.blocked("   ") {
		t.Fatal("空 ID 不该被判定为已卸载")
	}

	guard.block("  repo-a  ") // 记录与查询都做 TrimSpace 归一
	guard.block("repo-a")     // 重复卸载同一个库是幂等的
	guard.block("")
	guard.block("   ")

	if !guard.blocked("repo-a") {
		t.Fatal("手动卸载过的库应被挡住")
	}
	if guard.blocked("repo-b") {
		t.Fatal("别的库不该被牵连")
	}

	// 卸载是 HTTP 请求并发进来的，记账/查询必须并发安全（配合 -race 跑）。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				guard.block("repo-concurrent")
				_ = guard.blocked("repo-concurrent")
			}
		}()
	}
	wg.Wait()
	if !guard.blocked("repo-concurrent") {
		t.Fatal("并发记账后应被挡住")
	}
}

// joinAutoMountTargetIDs 把选出来的库按 ID 拼成一个字符串，便于整组比较（函数已保证顺序稳定）。
func joinAutoMountTargetIDs(targets []autoMountTarget) string {
	ids := make([]string, 0, len(targets))
	for _, item := range targets {
		ids = append(ids, item.repoID)
	}
	return strings.Join(ids, ",")
}
