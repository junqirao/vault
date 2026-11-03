package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteClientDiffFile 覆盖客户端上传落盘：路径校验、大小校验与 mtime 保留。
func TestWriteClientDiffFile(t *testing.T) {
	root := t.TempDir()
	content := "hello"
	if err := writeClientDiffFile(root, ClientDiffNeededFile{
		Path: "sub/a.txt", Size: int64(len(content)), Mtime: 1700000000000,
	}, strings.NewReader(content)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != content {
		t.Fatalf("内容 = %q", raw)
	}
	if info, err := os.Stat(filepath.Join(root, "sub", "a.txt")); err != nil || info.ModTime().UnixMilli() != 1700000000000 {
		t.Fatalf("mtime 未保留: info=%v err=%v", info, err)
	}

	// 声明大小与实际不一致必须拒绝。
	if err := writeClientDiffFile(root, ClientDiffNeededFile{
		Path: "b.txt", Size: 99,
	}, strings.NewReader("short")); err == nil {
		t.Fatal("大小不一致应失败")
	}
	// 路径逃逸必须拒绝。
	if err := writeClientDiffFile(root, ClientDiffNeededFile{
		Path: "../escape.txt", Size: 1,
	}, strings.NewReader("x")); err == nil {
		t.Fatal("路径逃逸应失败")
	}
}
