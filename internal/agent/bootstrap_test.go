package agent

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vault/internal/updatepkg"
)

// makeClientPackage 造一个完整的"客户端程序目录 zip"并返回路径、大小与摘要。
func makeClientPackage(t *testing.T, version string, complete bool) (string, int64, string) {
	t.Helper()
	src := t.TempDir()
	writeClientTestDir(t, src, 15)
	for _, name := range []string{"Vault-Agent.exe", "Vault-Updator.exe"} {
		if !complete && name == "Vault-Updator.exe" {
			continue
		}
		if err := os.WriteFile(filepath.Join(src, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	out := filepath.Join(t.TempDir(), "Vault-Client-"+version+".zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	if err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(w, in)
		return err
	}); err != nil {
		t.Fatalf("zip walk: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	sum := sha256.Sum256(data)
	return out, int64(len(data)), hex.EncodeToString(sum[:])
}

// startUpdateServer 起一个只提供更新清单/签名/产物的最小服务端。
func startUpdateServer(t *testing.T, version, zipPath string, size int64, sha string) *httptest.Server {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	oldPub := updatePublicKey
	updatePublicKey = pub
	t.Cleanup(func() { updatePublicKey = oldPub })

	filename := filepath.Base(zipPath)
	manifest := updatepkg.Manifest{
		Schema:     updatepkg.SchemaVersion,
		Channel:    "stable",
		Version:    version,
		ReleasedAt: time.Now().UnixMilli(),
		Artifacts: []updatepkg.Artifact{{
			Kind:     updatepkg.KindClient,
			OS:       "windows",
			Arch:     "amd64",
			Filename: filename,
			Size:     size,
			SHA256:   sha,
			URL:      "artifacts/" + filename,
		}},
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sig, err := updatepkg.Sign(priv, manifestBytes)
	if err != nil {
		t.Fatalf("sign manifest: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/system/update/manifest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(manifestBytes)
	})
	mux.HandleFunc("/v1/system/update/signature", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sig))
	})
	mux.HandleFunc("/v1/system/update/artifacts/"+filename, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, zipPath)
	})
	return httptest.NewServer(mux)
}

func TestRunClientBootstrapInstallsWholeClient(t *testing.T) {
	const version = "0.1.300"
	zipPath, size, sha := makeClientPackage(t, version, true)
	srv := startUpdateServer(t, version, zipPath, size, sha)
	defer srv.Close()

	root := t.TempDir()
	clientRoot := filepath.Join(root, "client")
	updateDir := filepath.Join(root, "update")
	var events []BootstrapEvent
	result, err := RunClientBootstrap(context.Background(), BootstrapOptions{
		CurrentVersion: "0.1.299",
		ServerURL:      srv.URL,
		Channel:        "stable",
		ClientRoot:     clientRoot,
		UpdateDir:      updateDir,
		NoRestart:      true,
	}, func(ev BootstrapEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !result.Updated || result.Version != version {
		t.Fatalf("unexpected result: %+v", result)
	}
	ptr, ok := readClientCurrent(filepath.Join(clientRoot, clientCurrentFileName))
	if !ok || !sameVersion(ptr.Version, version) {
		t.Fatalf("current.json 未指向新版本: %+v ok=%v", ptr, ok)
	}
	dir := filepath.Join(clientRoot, version)
	for _, name := range []string{"Vault.exe", "Vault-Agent.exe", "Vault-Updator.exe", "resources/app.asar", "VERSION"} {
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil || info.IsDir() {
			t.Fatalf("新版本目录缺少 %s: %v", name, err)
		}
	}
	// 临时解压目录必须清理干净。
	entries, err := os.ReadDir(clientRoot)
	if err != nil {
		t.Fatalf("read client root: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), webTempPrefix) {
			t.Fatalf("残留临时目录: %s", e.Name())
		}
	}
	// ready 事件必须出现，且带上新版本。
	ready := false
	for _, ev := range events {
		if ev.Event == BootstrapEventReady && sameVersion(ev.Version, version) {
			ready = true
		}
	}
	if !ready {
		t.Fatalf("没有 ready 事件: %+v", events)
	}
}

func TestRunClientBootstrapPrefersLatestOverPrepared(t *testing.T) {
	const preparedVersion = "0.1.398"
	const latestVersion = "0.1.399"

	root := t.TempDir()
	clientRoot := filepath.Join(root, "client")
	// 先造一个"已解压、上次没来得及重启"的旧版本。
	preparedDir := filepath.Join(clientRoot, preparedVersion)
	writeClientTestDir(t, preparedDir, 15)
	for _, name := range []string{"Vault-Agent.exe", "Vault-Updator.exe"} {
		if err := os.WriteFile(filepath.Join(preparedDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := writeClientCurrent(filepath.Join(clientRoot, clientCurrentFileName), clientCurrentPointer{
		Version: preparedVersion, Dir: preparedVersion,
	}); err != nil {
		t.Fatalf("write pointer: %v", err)
	}

	zipPath, size, sha := makeClientPackage(t, latestVersion, true)
	srv := startUpdateServer(t, latestVersion, zipPath, size, sha)
	defer srv.Close()

	result, err := RunClientBootstrap(context.Background(), BootstrapOptions{
		CurrentVersion: "0.1.397",
		ServerURL:      srv.URL,
		ClientRoot:     clientRoot,
		UpdateDir:      filepath.Join(root, "update"),
		NoRestart:      true,
	}, func(BootstrapEvent) {})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !result.Updated || !sameVersion(result.Version, latestVersion) {
		t.Fatalf("应直接安装最新版本，实际: %+v", result)
	}
	ptr, ok := readClientCurrent(filepath.Join(clientRoot, clientCurrentFileName))
	if !ok || !sameVersion(ptr.Version, latestVersion) {
		t.Fatalf("current.json 应指向最新版本: %+v ok=%v", ptr, ok)
	}
}

func TestRunClientBootstrapRejectsIncompletePackage(t *testing.T) {
	const version = "0.1.301"
	zipPath, size, sha := makeClientPackage(t, version, false)
	srv := startUpdateServer(t, version, zipPath, size, sha)
	defer srv.Close()

	root := t.TempDir()
	clientRoot := filepath.Join(root, "client")
	_, err := RunClientBootstrap(context.Background(), BootstrapOptions{
		CurrentVersion: "0.1.300",
		ServerURL:      srv.URL,
		ClientRoot:     clientRoot,
		UpdateDir:      filepath.Join(root, "update"),
		NoRestart:      true,
	}, func(BootstrapEvent) {})
	if err == nil {
		t.Fatal("缺少 Vault-Updator.exe 的包应被拒绝")
	}
	if _, ok := readClientCurrent(filepath.Join(clientRoot, clientCurrentFileName)); ok {
		t.Fatal("被拒绝的包不应写 current.json")
	}
}

func TestPreparedClientVersion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "0.1.400")
	writeClientTestDir(t, dir, 15)
	for _, name := range []string{"Vault-Agent.exe", "Vault-Updator.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := writeClientCurrent(filepath.Join(root, clientCurrentFileName), clientCurrentPointer{
		Version: "0.1.400", Dir: "0.1.400",
	}); err != nil {
		t.Fatalf("write pointer: %v", err)
	}
	got, ok := preparedClientVersion(BootstrapOptions{CurrentVersion: "0.1.399", ClientRoot: root})
	if !ok || !sameVersion(got.Version, "0.1.400") {
		t.Fatalf("应识别已就绪版本: %+v ok=%v", got, ok)
	}
	if _, ok := preparedClientVersion(BootstrapOptions{CurrentVersion: "0.1.400", ClientRoot: root}); ok {
		t.Fatal("同版本不应被当成更新")
	}
}

func TestBlockedVersionRoundTrip(t *testing.T) {
	root := t.TempDir()
	opt := BootstrapOptions{ClientRoot: root}
	if isBlockedVersion(opt, "0.1.500") {
		t.Fatal("没有记录时不应被判为坏版本")
	}
	if err := writeBlockedVersion(opt, "0.1.500", fmt.Errorf("boom")); err != nil {
		t.Fatalf("write blocked: %v", err)
	}
	if !isBlockedVersion(opt, "0.1.500") {
		t.Fatal("写入后应识别坏版本")
	}
	if isBlockedVersion(opt, "0.1.501") {
		t.Fatal("其它版本不应被误判")
	}
}
