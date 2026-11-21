package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSteamLibraryRoot(t *testing.T) {
	cases := map[string]string{
		`F:\Game\SteamLibrary`:                   `F:\Game\SteamLibrary`,
		`F:\Game\SteamLibrary\steamapps`:         `F:\Game\SteamLibrary`,
		`F:\Game\SteamLibrary\steamapps\common`:  `F:\Game\SteamLibrary`,
		`F:\Game\SteamLibrary\steamapps\common\`: `F:\Game\SteamLibrary`,
	}
	for input, want := range cases {
		if got := steamLibraryRoot(input); got != want {
			t.Fatalf("steamLibraryRoot(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestWriteSteamAppManifest(t *testing.T) {
	library := t.TempDir()
	mountPath := filepath.Join(library, "steamapps", "common", "My Game")
	spec := &MountSpec{
		SteamAppID:    730,
		SteamAppName:  "Counter-Strike 2",
		SteamBuildID:  "12345",
		DiskSizeBytes: 123456789,
	}
	path, err := writeSteamAppManifest(spec, library, mountPath)
	if err != nil {
		t.Fatalf("写入 appmanifest 失败: %v", err)
	}
	want := filepath.Join(library, "steamapps", "appmanifest_730.acf")
	if path != want {
		t.Fatalf("manifest 路径 = %q，期望 %q", path, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 appmanifest 失败: %v", err)
	}
	text := string(raw)
	for _, needle := range []string{`"appid"	"730"`, `"installdir"	"My Game"`, `"buildid"	"12345"`} {
		if !strings.Contains(text, needle) {
			t.Fatalf("appmanifest 缺少 %q:\n%s", needle, text)
		}
	}
}

// TestWriteSteamAppManifestKeepsOfficialDepots 锁定"不要用最小清单覆盖官方清单"。
//
// 真实反馈：游戏能被 Steam 识别，但一直显示"更新失败"。原因是旧实现只写
// appid/installdir/StateFlags/buildid，丢掉了 InstalledDepots / MountedDepots /
// SizeOnDisk —— Steam 认为 depot 没装齐，于是反复排一次注定失败的更新。
//
// 盘内（SteamCMD 安装目录）本来就有一份与内容完全对应的官方 appmanifest，
// 必须拿它当基线，只改随挂载点变化的字段。
func TestWriteSteamAppManifestKeepsOfficialDepots(t *testing.T) {
	library := t.TempDir()
	mountPath := filepath.Join(library, "steamapps", "common", "Palworld")
	repoApps := filepath.Join(mountPath, "steamapps")
	if err := os.MkdirAll(repoApps, 0o755); err != nil {
		t.Fatal(err)
	}
	official := `"AppState"
{
	"appid"		"1623730"
	"Universe"		"1"
	"name"		"Palworld"
	"StateFlags"		"1026"
	"installdir"		"Palworld"
	"LastUpdated"		"1700000000"
	"SizeOnDisk"		"30000000000"
	"StagingSize"		"123"
	"buildid"		"19000000"
	"UpdateResult"		"8"
	"BytesToDownload"		"123456"
	"BytesDownloaded"		"45"
	"InstalledDepots"
	{
		"1623731"
		{
			"manifest"		"555666777"
			"size"		"29999999999"
		}
	}
}
`
	officialPath := filepath.Join(repoApps, "appmanifest_1623730.acf")
	if err := os.WriteFile(officialPath, []byte(official), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := &MountSpec{
		SteamAppID:    1623730,
		SteamAppName:  "Palworld",
		SteamBuildID:  "19000000",
		DiskSizeBytes: 198870827008,
	}
	path, err := writeSteamAppManifest(spec, library, mountPath)
	if err != nil {
		t.Fatalf("写入 appmanifest 失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 appmanifest 失败: %v", err)
	}
	text := string(raw)

	// 内容与 depot 信息必须原样保留：它们是 Steam 判断"要不要更新"的依据。
	for _, needle := range []string{
		`"InstalledDepots"`,
		`"555666777"`,
		`"29999999999"`,
		`"buildid"		"19000000"`,
		`"SizeOnDisk"		"30000000000"`,
		`"installdir"		"Palworld"`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("官方清单字段 %q 应被保留:\n%s", needle, text)
		}
	}
	// 挂载/更新状态字段必须归零，否则 Steam 会继续显示"更新失败"。
	for _, needle := range []string{
		`"StateFlags"		"4"`,
		`"UpdateResult"		"0"`,
		`"BytesToDownload"		"0"`,
		`"BytesDownloaded"		"0"`,
		`"StagingSize"		"0"`,
		`"AutoUpdateBehavior"		"1"`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("清单应把 %q 改成挂载态:\n%s", needle, text)
		}
	}
	if strings.Contains(text, `"1026"`) || strings.Contains(text, `"123456"`) {
		t.Fatalf("旧的更新状态不应残留:\n%s", text)
	}
}

// TestResolveSteamMountDirUsesManifestInstallDir 覆盖 Steam 游戏目录命名：
// 服务端库名是 "Palworld (1623730)"，但 Steam 的 installdir 通常是 "Palworld"；
// 挂载目录必须跟 installdir 一致，否则 Steam 可能认不出游戏。
func TestResolveSteamMountDirUsesManifestInstallDir(t *testing.T) {
	library := t.TempDir()
	steamapps := filepath.Join(library, "steamapps")
	if err := os.MkdirAll(steamapps, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(steamapps, "appmanifest_1623730.acf")
	if err := os.WriteFile(manifest, []byte("\"AppState\"\n{\n\t\"installdir\"\t\t\"Palworld\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newMountDirEngine(t, fmt.Sprintf(
		`{"group_mounts":{"steam":{"mount_mode":"directory","steam_library":%q,"use_for_repos":true}}}`,
		library,
	))
	req := MountRequest{RepoID: "repo-palworld", RepoName: "Palworld (1623730)", RepoGroup: "steam"}
	spec := &MountSpec{SteamAppID: 1623730, SteamAppName: "Palworld"}
	got, err := e.resolveMountDir(req, spec)
	if err != nil {
		t.Fatalf("解析挂载目录失败: %v", err)
	}
	want := filepath.Join(library, "steamapps", "common", "Palworld")
	if !strings.EqualFold(got, want) {
		t.Fatalf("挂载目录 = %q，期望 %q", got, want)
	}
}

// TestResolveSteamMountDirRewritesLegacyAppIDFolder 覆盖旧版本写下的
// "Palworld (1623730)" installdir：重新挂载时应改用游戏名 "Palworld"。
func TestResolveSteamMountDirRewritesLegacyAppIDFolder(t *testing.T) {
	library := t.TempDir()
	steamapps := filepath.Join(library, "steamapps")
	if err := os.MkdirAll(steamapps, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(steamapps, "appmanifest_1623730.acf")
	if err := os.WriteFile(manifest, []byte("\"AppState\"\n{\n\t\"installdir\"\t\t\"Palworld (1623730)\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newMountDirEngine(t, fmt.Sprintf(
		`{"group_mounts":{"steam":{"mount_mode":"directory","steam_library":%q,"use_for_repos":true}}}`,
		library,
	))
	req := MountRequest{RepoID: "repo-palworld", RepoName: "Palworld (1623730)", RepoGroup: "steam"}
	spec := &MountSpec{SteamAppID: 1623730, SteamAppName: "Palworld"}
	got, err := e.resolveMountDir(req, spec)
	if err != nil {
		t.Fatalf("解析挂载目录失败: %v", err)
	}
	want := filepath.Join(library, "steamapps", "common", "Palworld")
	if !strings.EqualFold(got, want) {
		t.Fatalf("挂载目录 = %q，期望 %q", got, want)
	}
}
