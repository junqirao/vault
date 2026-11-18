package serverupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/releasever"
)

func TestSelectReleaseChannelAndAsset(t *testing.T) {
	current := mustVersion(t, "0.3.1-beta")
	releases := []githubRelease{
		{
			TagName: "v0.3.1-beta", Prerelease: true,
			Assets: []githubAsset{{Name: "Vault-Server-0.3.1-beta-win64.zip", Size: 1, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BrowserDownloadURL: "b1"}},
		},
		{
			TagName: "v0.3.2-beta.1", Prerelease: true,
			Assets: []githubAsset{{Name: "Vault-Server-0.3.2-beta.1-win64.zip", Size: 2, Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BrowserDownloadURL: "b2"}},
		},
		{
			TagName: "v0.3.2-rc.1", Prerelease: true,
			Assets: []githubAsset{{Name: "Vault-Server-0.3.2-rc.1-win64.zip", Size: 3, Digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", BrowserDownloadURL: "b3"}},
		},
		{
			TagName: "v0.3.3", Prerelease: false,
			Assets: []githubAsset{{Name: "Vault-Server-0.3.3-win64.zip", Size: 4, Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", BrowserDownloadURL: "b4"}},
		},
	}
	got, reason := selectRelease(releases, current, "beta", "windows", "amd64")
	if got == nil || got.Version != "0.3.3" || got.AssetName != "Vault-Server-0.3.3-win64.zip" || reason != "" {
		t.Fatalf("beta 应选到 0.3.3，实际 %+v reason=%q", got, reason)
	}
	if got.SHA256 == "" {
		t.Fatal("应解析 GitHub digest")
	}

	stable, _ := selectRelease(releases, mustVersion(t, "0.3.1"), "stable", "windows", "amd64")
	if stable == nil || stable.Version != "0.3.3" {
		t.Fatalf("stable 应选到 0.3.3，实际 %+v", stable)
	}

	// stable 当前只允许 stable，不能因为 0.3.9-beta 更高就提示 beta。
	onlyBeta := []githubRelease{{
		TagName: "v0.3.9-beta", Prerelease: true,
		Assets: []githubAsset{{Name: "Vault-Server-0.3.9-beta-win64.zip", Size: 1}},
	}}
	if got, _ := selectRelease(onlyBeta, mustVersion(t, "0.3.1"), "stable", "windows", "amd64"); got != nil {
		t.Fatalf("stable 不应选择预发布版本，实际 %+v", got)
	}
}

func TestSelectReleaseLinuxAssetAndNoPlatform(t *testing.T) {
	current := mustVersion(t, "0.3.1")
	releases := []githubRelease{{
		TagName: "v0.3.2", Prerelease: false,
		Assets: []githubAsset{{Name: "Vault-Server-0.3.2-linux-amd64.tar.gz", Size: 10, Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}},
	}}
	if got, reason := selectRelease(releases, current, "stable", "linux", "amd64"); got == nil || reason != "" {
		t.Fatalf("Linux 应选到 tar.gz 资产，实际 %+v reason=%q", got, reason)
	}
	if got, reason := selectRelease(releases, current, "stable", "windows", "amd64"); got != nil || reason != "no_platform_asset" {
		t.Fatalf("没有 Windows 资产时应返回 no_platform_asset，实际 %+v reason=%q", got, reason)
	}
}

func TestSelectReleaseRejectsMismatchedAssetVersion(t *testing.T) {
	releases := []githubRelease{{
		TagName: "v0.3.4", Prerelease: false,
		Assets: []githubAsset{{Name: "Vault-Server-0.3.3-win64.zip", Size: 1, Digest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}},
	}}
	if got, reason := selectRelease(releases, mustVersion(t, "0.3.1"), "stable", "windows", "amd64"); got != nil || reason != "no_platform_asset" {
		t.Fatalf("tag 与资产版本不一致时必须拒绝，实际 %+v reason=%q", got, reason)
	}
}

func TestDownloadResumeAndDigest(t *testing.T) {
	content := []byte("hello vault server update")
	sum := sha256.Sum256(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "update.zip", time.Now(), bytes.NewReader(content))
	}))
	defer server.Close()

	dir := t.TempDir()
	part := filepath.Join(dir, "update.zip.part")
	prefix := content[:6]
	if err := os.WriteFile(part, prefix, 0o600); err != nil {
		t.Fatal(err)
	}
	var last int64
	err := downloadAsset(context.Background(), server.Client(), server.URL, nil, part, int64(len(content)), hex.EncodeToString(sum[:]), func(received, total int64) {
		last = received
	})
	if err != nil {
		t.Fatalf("续传失败：%v", err)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("续传内容不一致：%q", got)
	}
	if last != int64(len(content)) {
		t.Fatalf("最终进度 = %d，期望 %d", last, len(content))
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bad.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../evil.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_ = f.Close()

	if err := extractArchive(archive, filepath.Join(dir, "out")); err == nil {
		t.Fatal("包含 .. 的 zip 必须被拒绝")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Fatal("越界文件不应被写出")
	}
}

func TestReplaceInstallAndRollback(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "install")
	stage := filepath.Join(root, "stage")
	updateDir := filepath.Join(root, "update")
	artifacts := filepath.Join(install, "updates")
	for _, d := range []string{install, filepath.Join(install, "scripts"), artifacts, stage, updateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldExe := filepath.Join(install, serverExeName())
	newExe := filepath.Join(stage, serverExeName())
	write(oldExe, "old-exe")
	write(newExe, "new-exe")
	write(filepath.Join(install, "scripts", "old.ps1"), "old-script")
	write(filepath.Join(stage, "scripts", "new.ps1"), "new-script")
	write(filepath.Join(artifacts, "old.txt"), "old-update")
	write(filepath.Join(stage, "updates", "new.txt"), "new-update")

	p := &Pending{CurrentExe: oldExe, NewExe: newExe, StageRoot: stage, ArtifactsDir: artifacts, InstallDir: install, UpdateDir: updateDir}
	rollback, err := replaceInstall(p, testLogger())
	if err != nil {
		t.Fatalf("替换失败：%v", err)
	}
	assertFile := func(path, want string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v；期望 %q", path, data, err, want)
		}
	}
	assertFile(oldExe, "new-exe")
	assertFile(filepath.Join(install, "scripts", "new.ps1"), "new-script")
	assertFile(filepath.Join(artifacts, "new.txt"), "new-update")

	if err := rollback(); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	assertFile(oldExe, "old-exe")
	assertFile(filepath.Join(install, "scripts", "old.ps1"), "old-script")
	assertFile(filepath.Join(artifacts, "old.txt"), "old-update")
}

func mustVersion(t *testing.T, raw string) releasever.Version {
	t.Helper()
	v, err := releasever.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
