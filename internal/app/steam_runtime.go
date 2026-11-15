package app

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"vault/internal/apperr"
)

// SteamRuntimeStatus 是服务端 SteamCMD 安装状态。
type SteamRuntimeStatus struct {
	Installed  bool   `json:"installed"`
	Path       string `json:"path"`
	Platform   string `json:"platform"`
	InstallDir string `json:"install_dir"`
	CanInstall bool   `json:"can_install"`
	LastError  string `json:"last_error"`
	// Installing 表示后台安装是否正在进行。
	Installing bool `json:"installing"`
	// Phase 当前阶段：preparing | downloading | extracting | bootstrapping | done | failed。
	Phase string `json:"phase"`
	// Progress 0-100；阶段切换时可能短暂回退，前端按阶段+进度展示即可。
	Progress int `json:"progress"`
	// Message 语言无关的进度细节（如已下载字节）。
	Message string `json:"message"`
	// Proxy / ProxySource / ProxyHint 当前用于访问 Steam 的代理与来源。
	Proxy       string `json:"proxy"`
	ProxySource string `json:"proxy_source"`
	ProxyHint   string `json:"proxy_hint"`
}

type steamInstallState struct {
	installing bool
	phase      string
	progress   int
	message    string
	lastError  string
}

// steamCmdInstallDir 返回自动安装 SteamCMD 的目标目录。
func (d Deps) steamCmdInstallDir() string {
	if p := strings.TrimSpace(d.raw().Steam.InstallDir); p != "" {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		if base := d.configDir(); base != "" {
			return filepath.Join(base, p)
		}
		return p
	}
	if base := d.configDir(); base != "" {
		return filepath.Join(base, "steamcmd")
	}
	return filepath.Join(os.TempDir(), "vault-steam", "steamcmd")
}

func steamCmdBinaryName() string {
	if runtime.GOOS == "windows" {
		return "steamcmd.exe"
	}
	return "steamcmd.sh"
}

func fileExists(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// steamProxyResolved 返回实际使用的代理、来源与提示。
//
// 优先级：steam.proxy（配置文件/环境变量覆盖）→ Windows 系统代理 → 进程环境变量。
func (d Deps) steamProxyResolved() (raw, source, hint string) {
	if p := strings.TrimSpace(d.raw().Steam.Proxy); p != "" {
		return p, "config", ""
	}
	if p, src, h := systemProxyInfo(); p != "" || src != "" {
		return p, src, h
	}
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, "env", ""
		}
	}
	return "", "none", ""
}

// steamProxyURL 返回配置的 Steam 代理；未配置或格式非法时返回 nil。
func (d Deps) steamProxyURL() *url.URL {
	raw, _, _ := d.steamProxyResolved()
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

// steamProxyEnv 把解析后的代理注入 SteamCMD 进程环境。
func (d Deps) steamProxyEnv() []string {
	raw, _, _ := d.steamProxyResolved()
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return []string{
		"HTTP_PROXY=" + raw, "HTTPS_PROXY=" + raw, "ALL_PROXY=" + raw,
		"http_proxy=" + raw, "https_proxy=" + raw, "all_proxy=" + raw,
	}
}

func maskProxy(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// steamHTTPClient 返回访问 Steam CDN 的 HTTP 客户端。
//
// 代理优先级与 SteamCMD 一致：steam.proxy → Windows 系统代理 → 进程环境变量。
// 需要绕过系统代理时，显式配置 steam.proxy 即可。
func (d Deps) steamHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u := d.steamProxyURL(); u != nil {
		transport.Proxy = http.ProxyURL(u)
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func steamInstallerURL() (downloadURL, archiveName string, err error) {
	switch runtime.GOOS {
	case "windows":
		return "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip", "steamcmd.zip", nil
	case "linux":
		return "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz", "steamcmd_linux.tar.gz", nil
	default:
		return "", "", apperr.New("steam.unsupported_platform", 501).WithArg("platform", runtime.GOOS)
	}
}

// SteamNetworkCheck 是服务端到 Steam CDN 的连通性诊断结果。
type SteamNetworkCheck struct {
	OK          bool   `json:"ok"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	LatencyMs   int64  `json:"latency_ms"`
	Error       string `json:"error"`
	Proxy       string `json:"proxy"`
	ProxySource string `json:"proxy_source"`
	ProxyHint   string `json:"proxy_hint"`
}

// CheckSteamNetwork 从**服务端进程**发起一次 Steam CDN 探测。
//
// 浏览器能访问 Steam 不代表服务端能访问：Windows 系统代理、加速器 TUN/系统代理模式、
// 以及 SteamCMD 是否继承代理都会影响结果。这个接口用于把差异直接展示出来。
func (s *SteamAccountService) CheckSteamNetwork(ctx context.Context) SteamNetworkCheck {
	downloadURL, _, err := steamInstallerURL()
	if err != nil {
		return SteamNetworkCheck{OK: false, Error: apperr.CodeOf(err)}
	}
	proxyRaw, proxySource, proxyHint := s.steamProxyResolved()
	proxy := maskProxy(proxyRaw)
	start := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, downloadURL, nil)
	if err != nil {
		return SteamNetworkCheck{OK: false, URL: downloadURL, Error: "request_failed", Proxy: proxy, ProxySource: proxySource, ProxyHint: proxyHint}
	}
	req.Header.Set("User-Agent", "Vault-Steam-Network-Check")
	resp, err := s.steamHTTPClient(20 * time.Second).Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return SteamNetworkCheck{
			OK: false, URL: downloadURL, LatencyMs: latency, Error: err.Error(),
			Proxy: proxy, ProxySource: proxySource, ProxyHint: proxyHint,
		}
	}
	defer resp.Body.Close() //nolint:errcheck
	return SteamNetworkCheck{
		OK:          resp.StatusCode >= 200 && resp.StatusCode < 400,
		URL:         downloadURL,
		Status:      resp.StatusCode,
		LatencyMs:   latency,
		Proxy:       proxy,
		ProxySource: proxySource,
		ProxyHint:   proxyHint,
	}
}

// RuntimeStatus 返回 SteamCMD 是否可用以及当前安装进度。
func (s *SteamAccountService) RuntimeStatus() SteamRuntimeStatus {
	dir := s.steamCmdInstallDir()
	path := s.steamCmdPath()
	s.runtimeMu.Lock()
	st := s.runtimeInstall
	s.runtimeMu.Unlock()
	proxyRaw, proxySource, proxyHint := s.steamProxyResolved()

	status := SteamRuntimeStatus{
		Installed:   path != "",
		Path:        path,
		Platform:    runtime.GOOS,
		InstallDir:  dir,
		CanInstall:  true,
		Installing:  st.installing,
		Phase:       st.phase,
		Progress:    st.progress,
		Message:     st.message,
		LastError:   st.lastError,
		Proxy:       maskProxy(proxyRaw),
		ProxySource: proxySource,
		ProxyHint:   proxyHint,
	}
	if status.Installed && !status.Installing {
		status.Phase = "done"
		status.Progress = 100
	}
	return status
}

type steamInstallReporter func(phase string, progress int, message string)

// StartInstallSteamCmd 在后台下载并安装 SteamCMD，立即返回当前安装状态。
//
// 安装进度可通过 RuntimeStatus / GET /v1/steam/runtime 轮询；重复调用是幂等的：
// 已安装直接返回，正在安装则返回当前进度。
func (s *SteamAccountService) StartInstallSteamCmd() (SteamRuntimeStatus, error) {
	if p := s.steamCmdPath(); p != "" {
		return s.RuntimeStatus(), nil
	}
	s.runtimeMu.Lock()
	if s.runtimeInstall.installing {
		s.runtimeMu.Unlock()
		return s.RuntimeStatus(), nil
	}
	s.runtimeInstall = steamInstallState{
		installing: true, phase: "preparing", progress: 0,
	}
	s.runtimeMu.Unlock()
	go s.runInstallSteamCmd()
	return s.RuntimeStatus(), nil
}

func (s *SteamAccountService) runInstallSteamCmd() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	report := func(phase string, progress int, message string) {
		s.runtimeMu.Lock()
		if s.runtimeInstall.installing {
			s.runtimeInstall.phase = phase
			s.runtimeInstall.progress = progress
			s.runtimeInstall.message = message
		}
		s.runtimeMu.Unlock()
	}
	status, err := s.installSteamCmdSync(ctx, report)
	s.runtimeMu.Lock()
	s.runtimeInstall.installing = false
	if err != nil {
		s.runtimeInstall.phase = "failed"
		s.runtimeInstall.lastError = apperr.CodeOf(err)
		if s.runtimeInstall.progress < 1 {
			s.runtimeInstall.progress = 1
		}
	} else {
		s.runtimeInstall.phase = "done"
		s.runtimeInstall.progress = 100
		s.runtimeInstall.message = ""
		s.runtimeInstall.lastError = status.LastError
	}
	s.runtimeMu.Unlock()
	if err != nil && s.Log != nil {
		s.Log.Error("SteamCMD 安装失败", "error", err)
	}
}

// installSteamCmdSync 下载并解压官方 SteamCMD 到配置的安装目录。
//
// 该动作只负责"把 steamcmd 放到服务端并尝试首次启动"；Linux 上仍可能需要
// 32 位运行库（lib32gcc / libstdc++ 等），失败时返回可操作的提示。
func (s *SteamAccountService) installSteamCmdSync(ctx context.Context, report steamInstallReporter) (SteamRuntimeStatus, error) {
	if report == nil {
		report = func(string, int, string) {}
	}
	if p := s.steamCmdPath(); p != "" {
		return SteamRuntimeStatus{
			Installed: true, Path: p, Platform: runtime.GOOS,
			InstallDir: s.steamCmdInstallDir(), CanInstall: true,
		}, nil
	}
	report("preparing", 2, "")
	dir := s.steamCmdInstallDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return SteamRuntimeStatus{}, apperr.New("steam.install_dir_failed", 500).
			WithArg("dir", dir).WithCause(err)
	}

	url, archive, err := steamInstallerURL()
	if err != nil {
		return SteamRuntimeStatus{}, err
	}

	tmp, err := os.CreateTemp("", "vault-steamcmd-*"+filepath.Ext(archive))
	if err != nil {
		return SteamRuntimeStatus{}, apperr.New("steam.install_download_failed", 500).WithCause(err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck
	_ = tmp.Close()

	client := s.steamHTTPClient(5 * time.Minute)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SteamRuntimeStatus{}, apperr.New("steam.install_download_failed", 500).WithCause(err)
	}
	req.Header.Set("User-Agent", "Vault-SteamCmd-Installer")
	report("downloading", 10, "")
	resp, err := client.Do(req)
	if err != nil {
		return SteamRuntimeStatus{}, apperr.New("steam.install_download_failed", 502).
			WithArg("url", url).WithCause(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SteamRuntimeStatus{}, apperr.New("steam.install_download_failed", 502).
			WithArg("status", resp.StatusCode).WithArg("url", url)
	}
	out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return SteamRuntimeStatus{}, apperr.New("steam.install_download_failed", 500).WithCause(err)
	}
	total := resp.ContentLength
	if _, err := copyWithProgress(out, resp.Body, total, func(done int64) {
		progress := 10
		if total > 0 {
			progress = 10 + int(float64(done)/float64(total)*45)
		}
		report("downloading", progress, formatDownloadProgress(done, total))
	}); err != nil {
		_ = out.Close()
		return SteamRuntimeStatus{}, apperr.New("steam.install_download_failed", 502).WithCause(err)
	}
	_ = out.Close()
	report("extracting", 58, "")

	if runtime.GOOS == "windows" {
		if err := extractZip(tmpPath, dir); err != nil {
			return SteamRuntimeStatus{}, apperr.New("steam.install_extract_failed", 500).WithCause(err)
		}
	} else {
		if err := extractTarGz(tmpPath, dir); err != nil {
			return SteamRuntimeStatus{}, apperr.New("steam.install_extract_failed", 500).WithCause(err)
		}
	}
	report("extracting", 85, "")

	status := SteamRuntimeStatus{
		Installed: fileExists(filepath.Join(dir, steamCmdBinaryName())),
		Path:      filepath.Join(dir, steamCmdBinaryName()),
		Platform:  runtime.GOOS, InstallDir: dir, CanInstall: true,
	}
	if !status.Installed {
		status.Path = s.steamCmdPath()
		if status.Path != "" {
			status.Installed = true
		}
	}
	if !status.Installed {
		return status, apperr.New("steam.install_extract_failed", 500).WithArg("dir", dir)
	}

	// 首次启动：让 SteamCMD 完成自更新。失败不抹掉已解压文件，但把原因写进状态，
	// 由界面提示安装 Linux 32 位运行库等依赖。
	report("bootstrapping", 92, "")
	bootstrapCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(bootstrapCtx, status.Path, "+quit")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "USERPROFILE="+dir)
	cmd.Env = append(cmd.Env, s.steamProxyEnv()...)
	if out, err := cmd.CombinedOutput(); err != nil {
		text := strings.TrimSpace(string(out))
		low := strings.ToLower(text)
		// SteamCMD 首次启动会自更新并可能以非 0 退出；只要输出里出现"更新完成"，
		// 就说明安装本身是成功的，不要把它误报成失败。
		if strings.Contains(low, "update complete") || strings.Contains(low, "steam>") {
			report("done", 100, "")
			return status, nil
		}
		if len(text) > 4*1024 {
			text = text[len(text)-4*1024:]
		}
		if s.Log != nil {
			s.Log.Warn("SteamCMD 首次启动失败", "path", status.Path, "output", text)
		}
		status.LastError = "首次启动失败（SteamCMD 自更新未完成）"
		if runtime.GOOS == "linux" {
			status.LastError += "；Linux 请确认已安装 32 位运行库：lib32gcc-s1 / lib32stdc++6 / libstdc++6:i386 等（详细输出见服务端日志）"
		} else {
			status.LastError += "；详细输出见服务端日志"
		}
	}
	report("done", 100, "")
	return status, nil
}

// copyWithProgress 把 src 拷贝到 dst，并按已读字节回调。
func copyWithProgress(dst io.Writer, src io.Reader, total int64, onProgress func(done int64)) (int64, error) {
	buf := make([]byte, 64*1024)
	var done int64
	lastReport := time.Now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				return done, wErr
			}
			done += int64(n)
			if time.Since(lastReport) >= 250*time.Millisecond || (total > 0 && done >= total) {
				onProgress(done)
				lastReport = time.Now()
			}
		}
		if err == io.EOF {
			onProgress(done)
			return done, nil
		}
		if err != nil {
			return done, err
		}
	}
}

func formatDownloadProgress(done, total int64) string {
	if total > 0 {
		return fmt.Sprintf("%.1f / %.1f MB", float64(done)/(1<<20), float64(total)/(1<<20))
	}
	return fmt.Sprintf("%.1f MB", float64(done)/(1<<20))
}

func safeExtractPath(root, name string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(name))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("steam: 压缩包路径越界: %s", name)
	}
	return target, nil
}

func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close() //nolint:errcheck
	for _, f := range zr.File {
		target, err := safeExtractPath(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		perm := f.Mode().Perm()
		if perm == 0 {
			perm = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
		if err != nil {
			_ = rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = out.Close()
			_ = rc.Close()
			return err
		}
		_ = out.Close()
		_ = rc.Close()
	}
	return nil
}

func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close() //nolint:errcheck
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeExtractPath(dst, hdr.Name)
		if err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode) & 0o777
		if mode == 0 {
			mode = 0o644
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				// 符号链接失败不致命：继续解压其它文件。
				continue
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			_ = out.Close()
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		}
	}
}
