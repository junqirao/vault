package serverupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxExtractedBytes = int64(4 << 30) // 4 GiB，防止 zip bomb

// extractArchive 解压 zip 或 tar.gz 到 dest。
//
// 安全约束：拒绝绝对路径、`..` 穿越、符号链接/硬链接；限制解压总量。
func extractArchive(archivePath, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, dest)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archivePath, dest)
	default:
		return fmt.Errorf("不支持的更新包格式：%s", filepath.Base(archivePath))
	}
}

func extractZip(archivePath, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	var total int64
	for _, f := range zr.File {
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		// 只接受普通文件；zip 里不应有符号链接等特殊类型。
		if !f.Mode().IsRegular() {
			return fmt.Errorf("更新包含非普通文件：%s", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if total > maxExtractedBytes {
			return fmt.Errorf("更新包解压后超过大小上限")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := writeExtracted(target, rc, mode); err != nil {
			_ = rc.Close()
			return err
		}
		_ = rc.Close()
	}
	return nil
}

func extractTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxExtractedBytes {
				return fmt.Errorf("更新包解压后超过大小上限")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			if err := writeExtracted(target, tr, mode); err != nil {
				return err
			}
		default:
			// 明确拒绝符号链接、硬链接、设备文件等，避免把更新包变成任意路径写入。
			return fmt.Errorf("更新包含不支持的文件类型：%s", hdr.Name)
		}
	}
}

func writeExtracted(target string, src io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	return f.Close()
}

// safeJoin 把归档内路径拼到 dest 下，并拒绝绝对路径与 `..` 穿越。
func safeJoin(dest, name string) (string, error) {
	clean := strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	if clean == "" || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("更新包含非法路径：%q", name)
	}
	clean = filepath.Clean(filepath.FromSlash(clean))
	if clean == "" || clean == "." {
		return dest, nil
	}
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("更新包含非法路径：%q", name)
	}
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("更新包含绝对路径：%q", name)
	}
	target := filepath.Join(dest, clean)
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("更新包含越界路径：%q", name)
	}
	return target, nil
}

// findPackageRoot 找到解压目录中包含服务端可执行文件的那一层。
func findPackageRoot(stageDir string) (string, error) {
	var found string
	err := filepath.WalkDir(stageDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := strings.ToLower(d.Name())
		if base == "vault-server.exe" || base == "vault-server" {
			if found == "" || len(path) < len(found) {
				found = path
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("更新包中未找到 Vault-Server 可执行文件")
	}
	return filepath.Dir(found), nil
}

// validateStage 校验更新包是否包含服务端程序与客户端更新清单。
//
// 要求 updates/manifest.json + manifest.json.sig 存在：服务端更新包同时承担
// "给客户端分发更新"的职责，缺了它会把客户端的更新链路一起弄坏。
func validateStage(root string) error {
	exe := filepath.Join(root, serverExeName())
	if info, err := os.Stat(exe); err != nil || info.IsDir() {
		return fmt.Errorf("更新包缺少服务端可执行文件")
	}
	updates := filepath.Join(root, "updates")
	if info, err := os.Stat(filepath.Join(updates, "manifest.json")); err != nil || info.IsDir() {
		return fmt.Errorf("更新包缺少 updates/manifest.json（客户端更新会被中断）")
	}
	if info, err := os.Stat(filepath.Join(updates, "manifest.json.sig")); err != nil || info.IsDir() {
		return fmt.Errorf("更新包缺少 updates/manifest.json.sig（客户端更新会被中断）")
	}
	if info, err := os.Stat(filepath.Join(updates, "artifacts")); err != nil || !info.IsDir() {
		return fmt.Errorf("更新包缺少 updates/artifacts 目录（客户端更新会被中断）")
	}
	manifestPath := filepath.Join(updates, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("读取 updates/manifest.json 失败: %w", err)
	}
	var manifest struct {
		Artifacts []struct {
			Filename string `json:"filename"`
			URL      string `json:"url"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("解析 updates/manifest.json 失败: %w", err)
	}
	for _, art := range manifest.Artifacts {
		name := strings.TrimSpace(art.Filename)
		if name == "" && strings.TrimSpace(art.URL) != "" {
			name = filepath.Base(filepath.FromSlash(strings.TrimSpace(art.URL)))
		}
		if name == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(updates, "artifacts", filepath.Base(name))); err != nil {
			return fmt.Errorf("更新包缺少客户端产物：%s", name)
		}
	}
	return nil
}

func serverExeName() string {
	if runtime.GOOS == "windows" {
		return "Vault-Server.exe"
	}
	return "vault-server"
}
