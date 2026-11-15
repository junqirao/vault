package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExtractZipAndTarGz(t *testing.T) {
	dir := t.TempDir()

	zipPath := filepath.Join(dir, "steamcmd.zip")
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, err := zw.CreateHeader(&zip.FileHeader{Name: "steamcmd.exe", Method: zip.Deflate})
	if err != nil {
		t.Fatalf("创建 zip 条目失败: %v", err)
	}
	if _, err := f.Write([]byte("bin")); err != nil {
		t.Fatalf("写 zip 条目失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	if err := os.WriteFile(zipPath, zbuf.Bytes(), 0o600); err != nil {
		t.Fatalf("写 zip 文件失败: %v", err)
	}
	zipDir := filepath.Join(dir, "zip")
	if err := os.MkdirAll(zipDir, 0o755); err != nil {
		t.Fatalf("建 zip 目录失败: %v", err)
	}
	if err := extractZip(zipPath, zipDir); err != nil {
		t.Fatalf("解压 zip 失败: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(zipDir, "steamcmd.exe")); err != nil || string(body) != "bin" {
		t.Fatalf("zip 解压结果不正确: %q %v", body, err)
	}

	tarPath := filepath.Join(dir, "steamcmd_linux.tar.gz")
	var tbuf bytes.Buffer
	gz := gzip.NewWriter(&tbuf)
	tw := tar.NewWriter(gz)
	payload := []byte("#!/bin/sh\n")
	if err := tw.WriteHeader(&tar.Header{Name: "steamcmd.sh", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatalf("写 tar 头失败: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("写 tar 内容失败: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关闭 tar 失败: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关闭 gzip 失败: %v", err)
	}
	if err := os.WriteFile(tarPath, tbuf.Bytes(), 0o600); err != nil {
		t.Fatalf("写 tar 文件失败: %v", err)
	}
	tarDir := filepath.Join(dir, "tar")
	if err := os.MkdirAll(tarDir, 0o755); err != nil {
		t.Fatalf("建 tar 目录失败: %v", err)
	}
	if err := extractTarGz(tarPath, tarDir); err != nil {
		t.Fatalf("解压 tar.gz 失败: %v", err)
	}
	info, err := os.Stat(filepath.Join(tarDir, "steamcmd.sh"))
	if err != nil {
		t.Fatalf("tar 解压结果缺失: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("steamcmd.sh 应保留可执行位，实际 %v", info.Mode().Perm())
	}
}

func TestSafeExtractPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := safeExtractPath(root, "../outside"); err == nil {
		t.Fatal("应拒绝路径穿越")
	}
	if _, err := safeExtractPath(root, "a/../../outside"); err == nil {
		t.Fatal("应拒绝路径穿越")
	}
	if _, err := safeExtractPath(root, "a/b"); err != nil {
		t.Fatalf("合法路径不应被拒绝: %v", err)
	}
}
