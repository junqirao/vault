package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// newMountDirEngine 构造一个只带本地配置与空状态存储的引擎：目录解析是纯计算，不碰 iSCSI。
//
// 状态存储是必须的：目录名要避让"被别的库占着的同名目录"（见 uniqueMountDir）。
func newMountDirEngine(t *testing.T, cfgJSON string) *mountEngine {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	if cfgJSON != "" {
		if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
			t.Fatalf("准备配置失败：%v", err)
		}
	}
	store, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	state, err := NewStateStore(filepath.Join(dir, "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	a := &Agent{logger: testLogger(), cfg: store, store: state}
	a.engine = &mountEngine{a: a}
	return a.engine
}

// TestResolveMountDirAddsRepoLevel 锁定"本地指定的目录是父目录"这条规则：
// 实际挂载点是它下面一层**与存储库同名**的子目录 —— 多个库共用一个父目录时天然分开，
// 目录名就是库名（真实反馈："挂载目录时，目录名应是存储库的名"）。
func TestResolveMountDirAddsRepoLevel(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		req  MountRequest
		spec *MountSpec
		want string
	}{
		{
			name: "对话框/每库配置填绝对目录 → 作为父目录再加一层库名",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x", MountPath: `D:\Vault`},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `D:\Vault\game-x`,
		},
		{
			name: "每库配置填相对目录 → 相对默认挂载根，再加一层库名",
			cfg:  `{"repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"libs"}}}`,
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `C:\Vault\libs\game-x`,
		},
		{
			name: "本地没配 → 退化为默认根下的一层库名",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `C:\Vault\game-x`,
		},
		{
			name: "服务端下发的相对路径也归一到同一命名（避免 srv\\lib 与 lib 两种目录名并存）",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server", MountPath: `vault-server\game-x`},
			want: `C:\Vault\game-x`,
		},
		{
			name: "服务端下发绝对路径（服务端管理员的显式指定）→ 原样使用",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server", MountPath: `E:\data\game-x`},
			want: `E:\data\game-x`,
		},
		{
			// 以前目录名是 `<服务端名称>_<库名>`，多服务端下每个人都要猜前缀；
			// 现在只认库名：别名/服务端名称都不该出现在目录名里。
			name: "服务端名称/别名不出现在目录名里",
			cfg:  `{"server_alias":"my-nas","repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"D:\\Vault"}}}`,
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `D:\Vault\game-x`,
		},
		{
			// 分组配置勾了「使用分组配置」且勾选了本库：父目录也从分组配置来。
			name: "分组配置生效时用分组的父目录",
			cfg: `{"group_mounts":{"games":{"mount_mode":"directory","mount_dir":"D:\\GroupVault","use_for_repos":true}},` +
				`"repo_mounts":{"repo-a":{"mount_mode":"letter"}}}`,
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x", RepoGroup: "games"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `D:\GroupVault\game-x`,
		},
		{
			// 老记录/旧配置可能把盘符当挂载点传进来：目录模式下不能拿它当父目录
			// （否则会算出 `C:\Vault\E:`）。
			name: "请求里的旧盘符挂载点被忽略",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x", MountPath: `E:`},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `C:\Vault\game-x`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMountDirEngine(t, c.cfg)
			got, err := e.resolveMountDir(c.req, c.spec)
			if err != nil {
				t.Fatalf("解析挂载目录失败：%v", err)
			}
			if got != c.want {
				t.Fatalf("挂载目录 = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestResolveMountDirIsIdempotentForRemount 重挂/恢复会把**上次的最终挂载点**当请求传回来，
// 若再加一层，每重挂一次就会多套一层目录（D:\Vault\lib\lib\lib…），用户的挂载点会越跑越深。
func TestResolveMountDirIsIdempotentForRemount(t *testing.T) {
	e := newMountDirEngine(t, "")
	spec := &MountSpec{ServerName: "vault-server"}
	req := MountRequest{RepoID: "repo-a", RepoName: "game-x", MountPath: `D:\Vault`}

	first, err := e.resolveMountDir(req, spec)
	if err != nil {
		t.Fatalf("首次解析失败：%v", err)
	}
	// 把上次结果当作本次请求（remount 的真实做法）：必须原样返回。
	req.MountPath = first
	second, err := e.resolveMountDir(req, spec)
	if err != nil {
		t.Fatalf("重挂解析失败：%v", err)
	}
	if second != first {
		t.Fatalf("重挂后挂载点漂移：%q → %q", first, second)
	}

	// Windows 文件系统不区分大小写：大小写不同也要认出"已经是最终挂载点"。
	req.MountPath = `D:\vault\GAME-X`
	third, err := e.resolveMountDir(req, spec)
	if err != nil {
		t.Fatalf("重挂解析失败：%v", err)
	}
	if third != req.MountPath {
		t.Fatalf("大小写不同却被当作新路径：%q → %q", req.MountPath, third)
	}
}

// TestResolveMountDirSanitizesLeaf 库名可能含 `\` `/` 等字符，直接拼进目录名会让
// 路径跑到父目录之外（路径穿越），必须先清洗。
func TestResolveMountDirSanitizesLeaf(t *testing.T) {
	e := newMountDirEngine(t, "")
	got, err := e.resolveMountDir(MountRequest{
		RepoID: "repo-a", RepoName: `..\..\evil`, MountPath: `D:\Vault`,
	}, &MountSpec{ServerName: "srv/../x"})
	if err != nil {
		t.Fatalf("解析挂载目录失败：%v", err)
	}
	// `..\..\evil` → `..-..-evil`，再去掉首部的点 → `-..-evil`：整体仍是**一个**目录名，
	// 不会真的退到上两级去。
	want := `D:\Vault\-..-evil`
	if got != want {
		t.Fatalf("挂载目录 = %q，期望 %q", got, want)
	}
}

// TestResolveMountDirAvoidsSameNameCollision 目录名现在就是库名：两台服务端（或两个分组）里
// 的同名库会算出同一个目录，第二个库绝不能挂到第一个库的数据上 —— 被别的库占着就加后缀。
func TestResolveMountDirAvoidsSameNameCollision(t *testing.T) {
	e := newMountDirEngine(t, `{"default_mount_dir":"D:\\Vault"}`)
	e.a.store.PutMount(&MountState{
		AllocationID: "alloc-other",
		RepoID:       "repo-b",
		RepoName:     "game-x",
		MountPath:    `D:\Vault\game-x`,
		MountMode:    mountModeDirectory,
		State:        MountStateMounted,
	}, nil)

	got, err := e.resolveMountDir(MountRequest{AllocationID: "alloc-1", RepoID: "repo-a", RepoName: "game-x"}, nil)
	if err != nil {
		t.Fatalf("解析挂载目录失败：%v", err)
	}
	if want := `D:\Vault\game-x-2`; got != want {
		t.Fatalf("同名库应避让到 %q，实际 %q", want, got)
	}

	// 同一个库重挂/恢复：它本来就占着这个目录，必须原样复用，不能多出一个后缀。
	got, err = e.resolveMountDir(MountRequest{AllocationID: "alloc-other", RepoID: "repo-b", RepoName: "game-x"}, nil)
	if err != nil {
		t.Fatalf("解析挂载目录失败：%v", err)
	}
	if want := `D:\Vault\game-x`; got != want {
		t.Fatalf("同库重挂应复用 %q，实际 %q", want, got)
	}
}

// TestMountDirLeafFallsBack 库名缺失时退到分配 ID（至少不会让所有库挤进同一个目录）；
// 都没有时退到 "vault"。
func TestMountDirLeafFallsBack(t *testing.T) {
	cases := []struct {
		repo, allocation, want string
	}{
		{repo: "game-x", want: "game-x"},
		{repo: "  ", allocation: "alloc-7", want: "alloc-7"},
		{repo: "", allocation: "", want: "vault"},
	}
	for _, c := range cases {
		if got := mountDirLeaf(c.repo, c.allocation); got != c.want {
			t.Fatalf("mountDirLeaf(%q, %q) = %q，期望 %q", c.repo, c.allocation, got, c.want)
		}
	}
}

// TestWithRecordedConfigPrefersCurrentPref 锁定"改配置后重挂要生效"：
// 记录里是盘符、库里配的却是目录时，重挂必须按目录来 —— 否则用户改完配置点重新挂载
// 还是盘符（真实反馈："都配置成了挂载到目录，但是挂载出来的全是盘符"）。
func TestWithRecordedConfigPrefersCurrentPref(t *testing.T) {
	e := newMountDirEngine(t, `{
		"repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"D:\\Vault"}},
		"group_mounts":{"games":{"mount_mode":"directory","mount_dir":"D:\\GroupVault","use_for_repos":true}}
	}`)
	record := MountState{
		AllocationID: "alloc-1",
		RepoID:       "repo-a",
		RepoName:     "game-x",
		RepoGroup:    "games",
		MountMode:    mountModeLetter,
		MountPath:    `E:`,
	}

	req := e.withRecordedConfig(MountRequest{AllocationID: "alloc-1", RepoID: "repo-a", RepoName: "game-x", RepoGroup: "games"}, record)
	if req.MountMode != mountModeDirectory {
		t.Fatalf("形态应取配置里的目录，实际 %q", req.MountMode)
	}
	// 分组优先：父目录用分组那份，旧盘符不能被当成父目录。
	if want := `D:\GroupVault`; req.MountPath != want {
		t.Fatalf("父目录 = %q，期望 %q", req.MountPath, want)
	}

	// 配置里没表态时沿用上次：上次是目录模式，记录里的最终挂载点就是父目录（幂等，不再加层）。
	e2 := newMountDirEngine(t, "")
	dirRecord := MountState{
		AllocationID: "alloc-2", RepoID: "repo-x", RepoName: "game-y",
		MountMode: mountModeDirectory, MountPath: `D:\Vault\game-y`,
	}
	if got := e2.withRecordedConfig(MountRequest{AllocationID: "alloc-2", RepoID: "repo-x", RepoName: "game-y"}, dirRecord); got.MountMode != mountModeDirectory || got.MountPath != `D:\Vault\game-y` {
		t.Fatalf("配置没表态时应沿用上次的目录挂载，实际 mode=%q path=%q", got.MountMode, got.MountPath)
	}
}
