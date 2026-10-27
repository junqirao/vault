package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// writeClientTestDir 造一个"看起来完整"的客户端程序目录（必备文件 + 指定文件数）。
func writeClientTestDir(t *testing.T, dir string, files int) {
	t.Helper()
	must := []string{clientExeName, clientAsarRel}
	for _, name := range must {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// 补足文件数（必备文件已占 2 个）。
	for i := len(must); i < files; i++ {
		p := filepath.Join(dir, "locales", "filler-"+itoa(i)+".pak")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestIsCompleteClientDir(t *testing.T) {
	t.Run("空目录不合格", func(t *testing.T) {
		if isCompleteClientDir(t.TempDir()) {
			t.Fatal("空目录不应被判为完整客户端目录")
		}
	})

	t.Run("缺 Vault.exe 不合格", func(t *testing.T) {
		dir := t.TempDir()
		writeClientTestDir(t, dir, clientMinFiles)
		if err := os.Remove(filepath.Join(dir, clientExeName)); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if isCompleteClientDir(dir) {
			t.Fatal("缺 Vault.exe 时不应判为完整")
		}
	})

	t.Run("文件数不足不合格", func(t *testing.T) {
		dir := t.TempDir()
		writeClientTestDir(t, dir, clientMinFiles-1)
		if isCompleteClientDir(dir) {
			t.Fatalf("文件数少于 %d 时不应判为完整", clientMinFiles)
		}
	})

	t.Run("完整目录合格", func(t *testing.T) {
		dir := t.TempDir()
		writeClientTestDir(t, dir, clientMinFiles)
		if !isCompleteClientDir(dir) {
			t.Fatal("完整目录应判为可用")
		}
	})
}

func TestClientCurrentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.json")

	if _, ok := readClientCurrent(path); ok {
		t.Fatal("文件不存在时应返回 ok=false")
	}

	first := clientCurrentPointer{Version: "0.1.211", Dir: "0.1.211", ActivatedAt: 111}
	if err := writeClientCurrent(path, first); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	got, ok := readClientCurrent(path)
	if !ok || got.Version != first.Version || got.ActivatedAt != first.ActivatedAt {
		t.Fatalf("读回不一致: %+v ok=%v", got, ok)
	}

	// 覆盖写：Windows 上 os.Rename 不能覆盖已存在文件，必须走 MoveFileEx。
	second := clientCurrentPointer{Version: "0.1.212", Dir: "0.1.212", ActivatedAt: 222}
	if err := writeClientCurrent(path, second); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	got, ok = readClientCurrent(path)
	if !ok || got.Version != second.Version {
		t.Fatalf("覆盖后读回不一致: %+v ok=%v", got, ok)
	}

	// 坏文件不应被当成有效指针（否则会指向不存在的目录）。
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := readClientCurrent(path); ok {
		t.Fatal("损坏的指针文件应返回 ok=false")
	}
}

func TestPruneClientVersionsKeepsCurrent(t *testing.T) {
	root := t.TempDir()
	dirs := []string{"0.1.209", "0.1.210", "0.1.211"}
	for _, v := range dirs {
		writeClientTestDir(t, filepath.Join(root, v), clientMinFiles)
	}
	// 当前版本（keep）永远不会被删。
	pruneClientVersions(root, "0.1.209", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !isCompleteClientDir(filepath.Join(root, "0.1.209")) {
		t.Fatal("当前版本目录不应被清理")
	}
}
