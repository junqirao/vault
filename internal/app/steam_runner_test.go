package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSteamCmdRootIsPerAccount(t *testing.T) {
	d := Deps{}
	root := t.TempDir()
	a := d.steamCmdRootFor(filepath.Join(root, "account-a"))
	b := d.steamCmdRootFor(filepath.Join(root, "account-b"))
	if a == b {
		t.Fatalf("不同账号必须使用不同 SteamCMD 根: %q", a)
	}
	if filepath.Base(a) != steamCmdDirName {
		t.Fatalf("账号 SteamCMD 根 = %q，期望以 %q 结尾", a, steamCmdDirName)
	}
	if got := d.steamCmdRootFor(""); got != "" {
		t.Fatalf("共享调用不应有账号根，实际 %q", got)
	}
}

func TestProvisionSteamCmdRootCopiesInstall(t *testing.T) {
	master := t.TempDir()
	writeTestFile(t, filepath.Join(master, steamCmdBinaryName()), "bin")
	// 主安装目录里可能残留别的账号登录态，绝不能复制进新账号副本。
	writeTestFile(t, filepath.Join(master, "config", "config.vdf"), `"AccountName" "other-account"`)
	writeTestFile(t, filepath.Join(master, "config", "loginusers.vdf"), "users")
	writeTestFile(t, filepath.Join(master, "logs", "content_log.txt"), "log")
	writeTestFile(t, filepath.Join(master, "package", "template.txt"), "pkg")
	writeTestFile(t, filepath.Join(master, "steamcmd.exe.old"), "old")

	sessionHome := t.TempDir()
	root := filepath.Join(sessionHome, steamCmdDirName)
	if err := provisionSteamCmdRoot(master, root, sessionHome); err != nil {
		t.Fatalf("复制 SteamCMD 失败: %v", err)
	}

	if got := readTestFile(t, filepath.Join(root, steamCmdBinaryName())); got != "bin" {
		t.Fatalf("可执行文件内容 = %q", got)
	}
	if got := readTestFile(t, filepath.Join(root, "package", "template.txt")); got != "pkg" {
		t.Fatalf("必要文件未复制: %q", got)
	}
	for _, rel := range []string{
		filepath.Join("config", "config.vdf"),
		filepath.Join("config", "loginusers.vdf"),
		filepath.Join("logs", "content_log.txt"),
		"steamcmd.exe.old",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s 不应被复制进账号副本", rel)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "config")); err != nil || !info.IsDir() {
		t.Fatalf("账号副本应保留空的 config 目录: %v", err)
	}

	// 再调用一次必须幂等，不能覆盖已有登录态。
	writeTestFile(t, filepath.Join(root, "config", "config.vdf"), "keep")
	if err := provisionSteamCmdRoot(master, root, sessionHome); err != nil {
		t.Fatalf("重复复制失败: %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, "config", "config.vdf")); got != "keep" {
		t.Fatalf("重复复制覆盖了登录态: %q", got)
	}
}

func TestProvisionSteamCmdRootMigratesLegacySession(t *testing.T) {
	master := t.TempDir()
	writeTestFile(t, filepath.Join(master, steamCmdBinaryName()), "bin")

	sessionHome := t.TempDir()
	state := filepath.Join(sessionHome, "steamcmd-state")
	writeTestFile(t, filepath.Join(state, "config", "config.vdf"), `"AccountName" "content-01"`)
	writeTestFile(t, filepath.Join(state, "ssfn123"), "sentry")

	root := filepath.Join(sessionHome, steamCmdDirName)
	if err := provisionSteamCmdRoot(master, root, sessionHome); err != nil {
		t.Fatalf("复制 SteamCMD 失败: %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, "config", "config.vdf")); !strings.Contains(got, "content-01") {
		t.Fatalf("旧登录态未迁移: %q", got)
	}
	if got := readTestFile(t, filepath.Join(root, "ssfn123")); got != "sentry" {
		t.Fatalf("ssfn 未迁移: %q", got)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("迁移后旧快照应改名，避免重复使用: %v", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	return string(raw)
}

func TestParseSteamDownloadProgress(t *testing.T) {
	cases := map[string]float64{
		"Update state (0x61) downloading, progress: 45.67 (1234 / 5678)": 45.67,
		"progress: 0":   0,
		"progress: 100": 100,
		"progress: 120": 100,
	}
	for line, want := range cases {
		got, ok := parseSteamDownloadProgress(line)
		if !ok || got != want {
			t.Fatalf("解析 %q = %v, %v；期望 %v", line, got, ok, want)
		}
	}
	if _, ok := parseSteamDownloadProgress("no progress here"); ok {
		t.Fatal("普通输出不应被当成进度")
	}
}

func TestParseSteamDownloadDetail(t *testing.T) {
	line := "Update state (0x61) downloading, progress: 45.67 (123,456,789 / 270,000,000)"
	sample, ok := parseSteamDownloadDetail(line)
	if !ok {
		t.Fatal("应解析完整进度行")
	}
	if sample.Percent != 45.67 || sample.BytesDone != 123456789 || sample.BytesTotal != 270000000 {
		t.Fatalf("解析结果 = %+v", sample)
	}
	if sample.Stage != "downloading" {
		t.Fatalf("Stage = %q", sample.Stage)
	}
	if _, ok := parseSteamDownloadDetail("progress: 45.67"); ok {
		t.Fatal("没有字节数的行不应被当成完整进度")
	}
}

func TestSteamLoginArgsUsesSetGuardCode(t *testing.T) {
	args := steamLoginArgs("content-01", "secret", "ABCDE")
	want := []string{"+@NoPromptForPassword", "1", "+set_steam_guard_code", "ABCDE", "+login", "content-01", "secret"}
	if len(args) != len(want) {
		t.Fatalf("登录参数 = %#v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("登录参数[%d] = %q，期望 %q", i, args[i], want[i])
		}
	}
	noCode := steamLoginArgs("content-01", "secret", "")
	if len(noCode) != 5 || noCode[0] != "+@NoPromptForPassword" || noCode[2] != "+login" {
		t.Fatalf("无验证码参数 = %#v", noCode)
	}
}

func TestSteamCachedLoginFailureDetection(t *testing.T) {
	out := "Cached credentials not found.\nFAILED (No cached credentials and @NoPromptForPassword is set)"
	if !steamCachedLoginFailed(out) {
		t.Fatal("应识别缓存凭据缺失")
	}
	if steamCachedLoginFailed("Steam Guard code: FAILED") {
		t.Fatal("验证码失败不应被当成缓存缺失")
	}
}

func TestSteamLoginMarkerAndCachedArgs(t *testing.T) {
	dir := t.TempDir()
	if canUseCachedLogin(dir, "content-01") {
		t.Fatal("没有标记时不应使用缓存登录")
	}
	writeSteamLoginMarker(dir, "content-01")
	if !canUseCachedLogin(dir, "CONTENT-01") {
		t.Fatal("账号名大小写不同也应允许缓存登录")
	}
	if canUseCachedLogin(dir, "other-account") {
		t.Fatal("标记属于其它账号时不应使用")
	}
	// 旧版快照迁移过来的 config.vdf 没有标记文件，也应允许尝试缓存登录。
	migrated := t.TempDir()
	writeTestFile(t, filepath.Join(migrated, steamCmdDirName, "config", "config.vdf"), "cached")
	if !canUseCachedLogin(migrated, "content-01") {
		t.Fatal("独立副本已有 config.vdf 时应允许缓存登录")
	}
	legacy := t.TempDir()
	writeTestFile(t, filepath.Join(legacy, "steamcmd-state", "config", "config.vdf"), "legacy")
	if !canUseCachedLogin(legacy, "content-01") {
		t.Fatal("旧版快照有 config.vdf 时应允许先尝试缓存登录")
	}
	args := steamCachedLoginArgs("content-01")
	if len(args) != 4 || args[0] != "+@NoPromptForPassword" || args[2] != "+login" || args[3] != "content-01" {
		t.Fatalf("缓存登录参数 = %#v", args)
	}
}

func TestParseSteamLoginOutputDoesNotMisreadFailedAsOK(t *testing.T) {
	out := "Loading Steam API...OK\r\nLogging in user 'content-01' to Steam Public...\r\nFAILED (Account Logon Denied)\r\n"
	res := parseSteamLoginOutput(out)
	if res.OK {
		t.Fatalf("失败输出不应被当成成功: %+v", res)
	}
	if !res.Needs2FA {
		t.Fatalf("Account Logon Denied 应识别为需要验证码: %+v", res)
	}
}

func TestSteamAuthErrorClassification(t *testing.T) {
	err := os.ErrDeadlineExceeded
	if got := steamAuthError("Enter the current code from your Steam Guard Mobile Authenticator app", err); got == nil ||
		!strings.Contains(got.Error(), "steam.account_needs_2fa") {
		t.Fatalf("Steam Guard 输出应识别为 needs_2fa: %v", got)
	}
	if got := steamAuthError("FAILED (Invalid Password)", err); got == nil ||
		!strings.Contains(got.Error(), "steam.account_login_failed") {
		t.Fatalf("密码错误应识别为 login_failed: %v", got)
	}
	if got := steamAuthError("network timeout", err); got != err {
		t.Fatalf("普通错误不应被改写: %v", got)
	}
}
