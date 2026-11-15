package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// steamCommandTimeout 是单次 SteamCMD 调用的兜底超时。
//
// 更新大游戏可能很久，调用方会传入更长的 context；这里只防止进程永久挂住。
const steamCommandTimeout = 6 * time.Hour

// steamCmdLocks 按 SteamCMD 安装根串行化进程。
//
// 每个账号都有独立的 SteamCMD 副本（安装根），所以不同账号可以并行；
// 只有共享同一个安装根的调用才会互相等待。锁的 key 是安装根路径，而不是账号 ID，
// 这样即便两个账号错误地指向同一目录，也不会并发写坏登录态。
var steamCmdLocks sync.Map // map[string]*sync.Mutex

func steamCmdLockFor(key string) *sync.Mutex {
	if strings.TrimSpace(key) == "" {
		key = "__shared__"
	}
	actual, _ := steamCmdLocks.LoadOrStore(key, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// steamLoginResult 是一次 SteamCMD 登录的结果。
type steamLoginResult struct {
	OK       bool
	Needs2FA bool
	Message  string
	Output   string
}

// steamCmdPath 返回当前生效的 steamcmd 可执行文件路径。
//
// 配置留空时从 PATH 查找；Windows 同时尝试 steamcmd.exe。找不到时返回空串，
// 由调用方返回 steam.steamcmd_unavailable，而不是把底层 exec 错误直接暴露给前端。
func (d Deps) steamCmdPath() string {
	if p := strings.TrimSpace(d.raw().Steam.SteamCmdPath); p != "" && fileExists(p) {
		return p
	}
	dir := d.steamCmdInstallDir()
	candidates := []string{filepath.Join(dir, steamCmdBinaryName())}
	if runtime.GOOS != "windows" {
		candidates = append(candidates, filepath.Join(dir, "steamcmd"))
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	for _, name := range []string{"steamcmd", "steamcmd.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// steamSessionRoot 返回账号会话目录根。
//
// 默认放在配置文件所在目录下的 steam/sessions：ssfn/config 必须跨重启保留，
// 放系统临时目录很容易被清理，导致每次更新都重新触发 Steam Guard。
func (d Deps) steamSessionRoot() string {
	if p := strings.TrimSpace(d.raw().Steam.SessionRoot); p != "" {
		return p
	}
	if base := d.configDir(); base != "" {
		return filepath.Join(base, "steam", "sessions")
	}
	return filepath.Join(os.TempDir(), "vault-steam", "sessions")
}

// steamStagingRoot 返回游戏更新暂存目录根。
func (d Deps) steamStagingRoot() string {
	if p := strings.TrimSpace(d.raw().Steam.StagingDir); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "vault-steam", "staging")
}

// runSteamCmd 执行一次 SteamCMD，并把密码/验证码从返回文本中抹掉。
//
// ⚠️ SteamCMD 没有安全的凭据文件/stdin 登录接口，密码只能作为命令行参数传入；
// 因此本进程必须运行在受控账户下，且绝不能把 args 写进日志/审计。
func (d Deps) runSteamCmd(ctx context.Context, sessionHome string, secrets []string, args ...string) (string, error) {
	return d.runSteamCmdWithTimeout(ctx, steamCommandTimeout, sessionHome, secrets, args...)
}

// runSteamCmdWithTimeout 是带显式超时的版本。
//
// 登录/许可同步/版本检查必须用短超时：Steam Guard 或网络异常时 SteamCMD 可能一直等，
// 长超时会让账号租约被占住几个小时，后续请求全部得到 steam.account_busy。
func (d Deps) runSteamCmdWithTimeout(ctx context.Context, timeout time.Duration, sessionHome string, secrets []string, args ...string) (string, error) {
	return d.runSteamCmdWithProgress(ctx, timeout, sessionHome, secrets, nil, args...)
}

// steamOutputWriter 同时收集 SteamCMD 输出并按行回调。
//
// 进度解析只需要行回调，但输出仍要保留最近 64KB 用于诊断，且需要从回调和最终
// 文本里抹掉密码/验证码。
type steamOutputWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	pending []byte
	secrets []string
	onLine  func(string)
}

func (w *steamOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.buf.Len() > 128*1024 {
		raw := w.buf.Bytes()
		trimmed := append([]byte(nil), raw[len(raw)-64*1024:]...)
		w.buf.Reset()
		_, _ = w.buf.Write(trimmed)
	}
	for _, b := range p {
		// SteamCMD 的进度行常用 \r 原地刷新；\n 与 \r 都当作一行结束。
		if b == '\n' || b == '\r' {
			w.emitLocked()
			continue
		}
		if len(w.pending) < 4096 {
			w.pending = append(w.pending, b)
		}
	}
	return len(p), nil
}

func (w *steamOutputWriter) emitLocked() {
	line := strings.TrimRight(string(w.pending), "\r")
	w.pending = w.pending[:0]
	for _, secret := range w.secrets {
		if secret != "" {
			line = strings.ReplaceAll(line, secret, "***")
		}
	}
	if w.onLine != nil && line != "" {
		w.onLine(line)
	}
}

func (w *steamOutputWriter) Flush() {
	w.mu.Lock()
	w.emitLocked()
	w.mu.Unlock()
}

func (w *steamOutputWriter) String() string {
	w.mu.Lock()
	out := w.buf.String()
	w.mu.Unlock()
	if len(out) > 64*1024 {
		out = out[len(out)-64*1024:]
	}
	for _, secret := range w.secrets {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "***")
		}
	}
	return out
}

// runSteamCmdWithProgress 是带行回调的 SteamCMD 执行入口。
func (d Deps) runSteamCmdWithProgress(ctx context.Context, timeout time.Duration, sessionHome string, secrets []string, onLine func(string), args ...string) (string, error) {
	if sessionHome != "" {
		if err := os.MkdirAll(sessionHome, 0o700); err != nil {
			return "", apperr.New("steam.session_dir_failed", 500).WithCause(err)
		}
	}
	if timeout <= 0 {
		timeout = steamCommandTimeout
	}

	// 同一个 SteamCMD 安装根同一时刻只允许一个进程：共享安装会并发写坏 config。
	// 不同账号使用不同安装根，因此可以并行下载。
	lock := steamCmdLockFor(d.steamCmdLockKey(sessionHome))
	lock.Lock()
	defer lock.Unlock()

	path, cmdRoot, err := d.steamCmdForSession(sessionHome)
	if err != nil {
		return "", err
	}

	// 超时从真正开始执行算起，不把等待闸门的时间算进 SteamCMD 预算。
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Dir = cmdRoot
	cmd.Env = os.Environ()
	if sessionHome != "" {
		cmd.Env = append(cmd.Env,
			"HOME="+sessionHome,
			"USERPROFILE="+sessionHome,
			"XDG_CONFIG_HOME="+filepath.Join(sessionHome, ".config"),
		)
	}
	cmd.Env = append(cmd.Env, d.steamProxyEnv()...)
	writer := &steamOutputWriter{secrets: secrets, onLine: onLine}
	cmd.Stdout = writer
	cmd.Stderr = writer
	err = cmd.Run()
	writer.Flush()
	out := writer.String()
	if err != nil {
		if runCtx.Err() != nil {
			return out, apperr.New("steam.steamcmd_timeout", 504).WithCause(runCtx.Err())
		}
		return out, apperr.New("steam.steamcmd_failed", 502).WithCause(err)
	}
	return out, nil
}

// steamSessionRelPaths 返回 SteamCMD 登录态文件（相对安装目录）。
func steamSessionRelPaths(root string) []string {
	var out []string
	for _, rel := range []string{
		filepath.Join("config", "config.vdf"),
		filepath.Join("config", "loginusers.vdf"),
	} {
		if fileExists(filepath.Join(root, rel)) {
			out = append(out, rel)
		}
	}
	for _, pattern := range []string{"ssfn*", filepath.Join("config", "ssfn*")} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			info, statErr := os.Stat(match)
			if statErr != nil || info.IsDir() {
				continue
			}
			if rel, relErr := filepath.Rel(root, match); relErr == nil {
				out = append(out, rel)
			}
		}
	}
	return out
}

// copySteamCmdSession 把登录态文件从 src 复制到 dst。
//
// 只用于把旧版"共享安装 + 快照"迁移到账号独立目录；新逻辑不再在每次运行前后搬运。
func copySteamCmdSession(src, dst string) error {
	var firstErr error
	for _, rel := range steamSessionRelPaths(src) {
		if err := copySteamFile(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	return firstErr
}

func copySteamFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// steamAuthError 把 SteamCMD 输出中的登录/验证码问题转换成明确业务错误。
//
// 更新/检查路径以前只返回 steamcmd_failed，界面无法判断"是网络问题还是需要
// 重新过 Steam Guard"。这里统一识别，让调度器/前端能提示用户去补一次验证。
func steamAuthError(out string, err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "steam guard"),
		strings.Contains(low, "two-factor"),
		strings.Contains(low, "two factor"),
		strings.Contains(low, "mobile authenticator"):
		return apperr.New("steam.account_needs_2fa", 409).WithCause(err)
	case strings.Contains(low, "invalid password"),
		strings.Contains(low, "login failure"),
		strings.Contains(low, "invalid login"),
		strings.Contains(low, "account logon denied"):
		return apperr.New("steam.account_login_failed", 401).WithCause(err)
	}
	return err
}

// httpStatusNotImplemented 避免为一条错误单独 import net/http 造成命名冲突。
const httpStatusNotImplemented = 501

// steamLoginArgs 生成登录参数。
//
// 新版 SteamCMD 对第三种参数形式的 Guard code 支持不稳定；按 Valve 文档推荐，
// 先用 set_steam_guard_code 写入验证码，再执行 login。没有验证码时只传用户名+密码。
func steamLoginArgs(username, password, guardCode string) []string {
	// 无人值守进程不能停在交互提示上；缺密码/验证码时让 SteamCMD 直接失败。
	args := []string{"+@NoPromptForPassword", "1"}
	code := strings.TrimSpace(guardCode)
	if code != "" {
		args = append(args, "+set_steam_guard_code", code)
	}
	return append(args, "+login", username, password)
}

// steamCachedLoginArgs 使用 SteamCMD 已保存的登录态登录（不传密码/验证码）。
//
// SteamCMD 支持 `login <username>` 复用 config.vdf 中的缓存凭据；这比每次都拿
// 密码 + Steam Guard 重新认证稳定得多，尤其是邮箱验证码只能用一次。
func steamCachedLoginArgs(username string) []string {
	return []string{"+@NoPromptForPassword", "1", "+login", username}
}

const steamAccountMarkerFile = "steamcmd-account.txt"

// writeSteamLoginMarker 记录"这个会话目录最近一次成功登录的是哪个账号"。
func writeSteamLoginMarker(sessionHome, username string) {
	if strings.TrimSpace(sessionHome) == "" || strings.TrimSpace(username) == "" {
		return
	}
	_ = os.MkdirAll(sessionHome, 0o700)
	_ = os.WriteFile(filepath.Join(sessionHome, steamAccountMarkerFile), []byte(strings.TrimSpace(username)), 0o600)
}

// canUseCachedLogin 判断该账号目录里是否已有可复用的 SteamCMD 登录态。
//
// 每个账号有独立的 SteamCMD 副本，所以副本里的 config.vdf 天然属于该账号；
// 旧版快照迁移过来的 config.vdf 没有标记文件，也应允许尝试缓存登录。
func canUseCachedLogin(sessionHome, username string) bool {
	if strings.TrimSpace(sessionHome) == "" || strings.TrimSpace(username) == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(sessionHome, steamAccountMarkerFile))
	if err == nil && strings.EqualFold(strings.TrimSpace(string(raw)), strings.TrimSpace(username)) {
		return true
	}
	return fileExists(filepath.Join(sessionHome, steamCmdDirName, "config", "config.vdf")) ||
		fileExists(filepath.Join(sessionHome, "steamcmd-state", "config", "config.vdf"))
}

// steamTestLogin 执行一次登录测试并解析结果。
func (d Deps) steamTestLogin(ctx context.Context, sessionHome, username, password, guardCode string, allowCached bool) (steamLoginResult, error) {
	// 优先复用缓存登录态：如果它还有效，就不需要密码/验证码，也不会触发新的 Guard。
	if allowCached && canUseCachedLogin(sessionHome, username) {
		cachedOut, cachedErr := d.runSteamCmdWithTimeout(ctx, 45*time.Second, sessionHome, nil,
			append(steamCachedLoginArgs(username), "+quit")...)
		if cachedErr == nil {
			cachedRes := parseSteamLoginOutput(cachedOut)
			cachedRes.Output = cachedOut
			if cachedRes.OK {
				writeSteamLoginMarker(sessionHome, username)
				return cachedRes, nil
			}
		}
	}

	secrets := []string{password, guardCode}
	args := append([]string{}, steamLoginArgs(username, password, guardCode)...)
	args = append(args, "+quit")
	// 首次登录可能包含 SteamCMD 自更新/网络检查；90 秒在慢代理下偏紧。
	out, err := d.runSteamCmdWithTimeout(ctx, 2*time.Minute, sessionHome, secrets, args...)
	res := parseSteamLoginOutput(out)
	res.Output = out
	if err != nil {
		// 输出里已经能判断"需要验证码/密码错误"时，优先返回更具体的业务语义。
		if res.Needs2FA {
			return res, apperr.New("steam.account_needs_2fa", 409)
		}
		if res.Message != "" {
			return res, apperr.New("steam.account_login_failed", 401).WithArg("reason", res.Message)
		}
		return res, err
	}
	if res.Needs2FA {
		return res, apperr.New("steam.account_needs_2fa", 409)
	}
	if !res.OK {
		reason := res.Message
		if reason == "" {
			reason = "unknown"
		}
		return res, apperr.New("steam.account_login_failed", 401).WithArg("reason", reason)
	}
	writeSteamLoginMarker(sessionHome, username)
	return res, nil
}

var (
	reSteamBuildID       = regexp.MustCompile(`(?im)"buildid"\s+"?([0-9]+)"?`)
	reSteamPublicBuildID = regexp.MustCompile(`(?is)"public"\s*\{[^}]*?"buildid"\s+"?([0-9]+)"?`)
	reSteamApps          = regexp.MustCompile(`(?im)^\s*-?\s*apps\s*:\s*(.+)$`)
	reSteamDownloadPct   = regexp.MustCompile(`(?i)progress:\s*([0-9]+(?:\.[0-9]+)?)`)
	// Update state (0x61) downloading, progress: 45.67 (123456789 / 270000000)
	reSteamDownloadDetail = regexp.MustCompile(`(?i)progress:\s*([0-9]+(?:\.[0-9]+)?)\s*\(\s*([0-9][0-9,]*)\s*/\s*([0-9][0-9,]*)\s*\)`)
	reSteamUpdateStage    = regexp.MustCompile(`(?i)update state\s*\([^)]*\)\s*([a-z]+)`)
	// SteamCMD 完成安装的最终标记：Success! App '2344520' fully installed.
	// 已是最新时输出 Success! App '2344520' already up to date.，同样算完成。
	reSteamFullyInstalled = regexp.MustCompile(`(?i)success!\s+app\s+'?(\d+)'?\s+(?:fully installed|(?:is\s+)?already\s+up\s+to\s+date)`)
)

// steamUpdateCompleted 判断 SteamCMD 是否真正完成了 app_update。
//
// 仅凭进程退出码不够：SteamCMD 可能在输出 Success 前就被上层误判为结束，
// 也可能因为自更新/子进程继续写 staging。必须看到该 AppID 的完成标记。
func steamUpdateCompleted(out string, appID int64) bool {
	want := strconv.FormatInt(appID, 10)
	for _, m := range reSteamFullyInstalled.FindAllStringSubmatch(out, -1) {
		if len(m) == 2 && m[1] == want {
			return true
		}
	}
	return false
}

// steamDownloadSample 是 SteamCMD 单行进度里能拿到的完整信息。
//
// SteamCMD 的典型输出：
//
//	Update state (0x61) downloading, progress: 45.67 (123456789 / 270000000)
//
// 它不直接打印速度，速度由调用方按"字节增量 / 时间"计算。
type steamDownloadSample struct {
	Percent    float64
	BytesDone  int64
	BytesTotal int64
	Stage      string
}

func parseSteamDownloadDetail(line string) (steamDownloadSample, bool) {
	m := reSteamDownloadDetail.FindStringSubmatch(line)
	if len(m) != 4 {
		return steamDownloadSample{}, false
	}
	percent, err := strconv.ParseFloat(m[1], 64)
	if err != nil || percent < 0 {
		return steamDownloadSample{}, false
	}
	if percent > 100 {
		percent = 100
	}
	done, err := strconv.ParseInt(strings.ReplaceAll(m[2], ",", ""), 10, 64)
	if err != nil || done < 0 {
		return steamDownloadSample{}, false
	}
	total, err := strconv.ParseInt(strings.ReplaceAll(m[3], ",", ""), 10, 64)
	if err != nil || total < 0 {
		return steamDownloadSample{}, false
	}
	sample := steamDownloadSample{Percent: percent, BytesDone: done, BytesTotal: total}
	if stage := reSteamUpdateStage.FindStringSubmatch(line); len(stage) == 2 {
		sample.Stage = strings.ToLower(stage[1])
	}
	return sample, true
}

// parseSteamDownloadProgress 解析 SteamCMD 的 "progress: 45.67" 行。
func parseSteamDownloadProgress(line string) (float64, bool) {
	m := reSteamDownloadPct.FindStringSubmatch(line)
	if len(m) != 2 {
		return 0, false
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil || value < 0 {
		return 0, false
	}
	if value > 100 {
		value = 100
	}
	return value, true
}

// parseSteamLoginOutput 从 SteamCMD 输出中判断登录结果。
func parseSteamLoginOutput(out string) steamLoginResult {
	low := strings.ToLower(out)
	res := steamLoginResult{}
	switch {
	// 先判断 Guard：日志里经常同时出现 "Steam Guard" 和 "Account Logon Denied"，
	// 如果先匹配后者会被误判成密码错误。
	case strings.Contains(low, "steam guard"),
		strings.Contains(low, "two-factor"),
		strings.Contains(low, "two factor"),
		strings.Contains(low, "mobile authenticator"),
		strings.Contains(low, "invalid login auth code"),
		strings.Contains(low, "account logon denied"):
		res.Needs2FA = true
		res.Message = "needs_2fa"
		return res
	case strings.Contains(low, "invalid password"),
		strings.Contains(low, "login failure"),
		strings.Contains(low, "invalid login"),
		strings.Contains(low, "rate limit"),
		strings.Contains(low, "too many"):
		res.Message = "invalid_credential"
		return res
	case strings.Contains(low, "logged in ok"),
		strings.Contains(low, "waiting for user info...ok"),
		strings.Contains(low, "waiting for license info...ok"),
		strings.Contains(low, "logged in user"):
		res.OK = true
		return res
	}
	// 明确失败优先于泛化的 "OK"（例如 "Loading Steam API...OK" + "FAILED"）。
	if strings.Contains(low, "failed") || strings.Contains(low, "denied") || strings.Contains(low, "failure") {
		res.Message = "login_failed"
		return res
	}
	// SteamCMD 有些版本只输出 "Steam>" 与 OK，没有固定成功串。
	if strings.Contains(low, "ok") && !strings.Contains(low, "error") {
		res.OK = true
	}
	return res
}

// steamCheckAppInfo 返回指定 AppID 在当前平台分支上的 buildid。
func (d Deps) steamCheckAppInfo(ctx context.Context, sessionHome, username, password, guardCode, platform string, appID int64) (string, error) {
	if canUseCachedLogin(sessionHome, username) {
		cachedArgs := []string{}
		if platform != "" {
			cachedArgs = append(cachedArgs, "+@sSteamCmdForcePlatformType", platform)
		}
		cachedArgs = append(cachedArgs, steamCachedLoginArgs(username)...)
		cachedArgs = append(cachedArgs, "+app_info_update", "1", "+app_info_print", strconv.FormatInt(appID, 10), "+quit")
		out, err := d.runSteamCmdWithTimeout(ctx, 3*time.Minute, sessionHome, nil, cachedArgs...)
		if err == nil {
			writeSteamLoginMarker(sessionHome, username)
			return steamBuildIDFromOutput(out), nil
		}
		if parseSteamLoginOutput(out).OK {
			writeSteamLoginMarker(sessionHome, username)
		}
	}

	args := []string{}
	if platform != "" {
		args = append(args, "+@sSteamCmdForcePlatformType", platform)
	}
	args = append(args, steamLoginArgs(username, password, guardCode)...)
	args = append(args, "+app_info_update", "1", "+app_info_print", strconv.FormatInt(appID, 10), "+quit")
	out, err := d.runSteamCmdWithTimeout(ctx, 3*time.Minute, sessionHome, []string{password, guardCode}, args...)
	if err != nil {
		if parseSteamLoginOutput(out).OK {
			// app_info 失败但登录成功：仍保存会话，下次优先缓存登录。
			writeSteamLoginMarker(sessionHome, username)
		}
		return "", steamAuthError(out, err)
	}
	writeSteamLoginMarker(sessionHome, username)
	return steamBuildIDFromOutput(out), nil
}

func steamBuildIDFromOutput(out string) string {
	if m := reSteamPublicBuildID.FindStringSubmatch(out); len(m) == 2 {
		return m[1]
	}
	if m := reSteamBuildID.FindStringSubmatch(out); len(m) == 2 {
		return m[1]
	}
	return ""
}

// steamSyncLicenses 执行 licenses_print 并解析 Apps 行。
func (d Deps) steamSyncLicenses(ctx context.Context, sessionHome, username, password, guardCode string, allowCached bool) ([]int64, string, error) {
	// 先用缓存登录态；有效时完全不需要密码和 Guard。
	if allowCached && canUseCachedLogin(sessionHome, username) {
		cachedArgs := append(steamCachedLoginArgs(username), "+licenses_print", "+quit")
		out, err := d.runSteamCmdWithTimeout(ctx, 3*time.Minute, sessionHome, nil, cachedArgs...)
		if err == nil {
			apps := parseSteamLicenseApps(out)
			if len(apps) > 0 || reSteamApps.MatchString(out) {
				writeSteamLoginMarker(sessionHome, username)
				return apps, out, nil
			}
			// 缓存登录已成功，只是输出解析失败：不要再拿密码重新登录一次。
			return nil, out, apperr.New("steam.license_parse_failed", 502)
		}
		if parseSteamLoginOutput(out).OK {
			writeSteamLoginMarker(sessionHome, username)
		}
	}

	args := append([]string{}, steamLoginArgs(username, password, guardCode)...)
	args = append(args, "+licenses_print", "+quit")
	out, err := d.runSteamCmdWithTimeout(ctx, 3*time.Minute, sessionHome, []string{password, guardCode}, args...)
	if err != nil {
		if parseSteamLoginOutput(out).OK {
			writeSteamLoginMarker(sessionHome, username)
		}
		return nil, out, steamAuthError(out, err)
	}
	writeSteamLoginMarker(sessionHome, username)
	apps := parseSteamLicenseApps(out)
	// 解析不到 Apps 行时不要用空列表覆盖已有许可：SteamCMD 输出格式变化/账号
	// 尚未完全登录时，保留旧数据比把用户游戏库清空更安全。
	if len(apps) == 0 && !reSteamApps.MatchString(out) {
		return nil, out, apperr.New("steam.license_parse_failed", 502)
	}
	return apps, out, nil
}

// parseSteamLicenseApps 解析 licenses_print 输出中的 `Apps : 730, 440, ...` 行。
func parseSteamLicenseApps(out string) []int64 {
	seen := map[int64]bool{}
	var ids []int64
	for _, m := range reSteamApps.FindAllStringSubmatch(out, -1) {
		if len(m) != 2 {
			continue
		}
		for _, part := range strings.FieldsFunc(m[1], func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		}) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// steamUpdateCommandArgs 生成 app_update 的参数。
//
// SteamCMD 要求 +force_install_dir 在 +login 之前；放到 login 后面会打印
// "Please use force_install_dir before logon!"，并可能继续使用 SteamCMD 默认的
// steamapps 目录，导致更新写错位置、暂存目录看起来"已经是最新"。
func steamUpdateCommandArgs(username, password, guardCode, platform, installDir string, appID int64, validate, cached bool) []string {
	args := make([]string, 0, 12)
	if platform != "" {
		args = append(args, "+@sSteamCmdForcePlatformType", platform)
	}
	args = append(args, "+force_install_dir", installDir)
	if cached {
		args = append(args, steamCachedLoginArgs(username)...)
	} else {
		args = append(args, steamLoginArgs(username, password, guardCode)...)
	}
	args = append(args, "+app_update", strconv.FormatInt(appID, 10))
	if validate {
		args = append(args, "validate")
	}
	return append(args, "+quit")
}

// steamUpdateApp 把游戏更新到暂存目录。
func (d Deps) steamUpdateApp(ctx context.Context, sessionHome, username, password, guardCode, platform string, appID int64, installDir string, validate bool, onLine func(string)) (string, error) {
	// 优先用缓存身份直接下载；只有缓存登录失效时才回退密码 + Guard。
	if canUseCachedLogin(sessionHome, username) {
		args := steamUpdateCommandArgs(username, password, guardCode, platform, installDir, appID, validate, true)
		out, err := d.runSteamCmdWithProgress(ctx, steamCommandTimeout, sessionHome, nil, onLine, args...)
		if err == nil || parseSteamLoginOutput(out).OK {
			writeSteamLoginMarker(sessionHome, username)
		}
		if err == nil {
			return out, nil
		}
		mapped := steamUpdateError(out, appID, err)
		if !steamCachedLoginFailed(out) {
			switch apperr.CodeOf(mapped) {
			case "steam.account_needs_2fa", "steam.account_login_failed":
				// 缓存登录在下载途中失效：继续走下面的完整登录，允许本次使用 Guard 验证码。
			default:
				return out, mapped
			}
		}
		// 没有缓存凭据（SteamCMD 输出 "Cached credentials not found"）：回退完整登录。
	}

	args := steamUpdateCommandArgs(username, password, guardCode, platform, installDir, appID, validate, false)
	out, err := d.runSteamCmdWithProgress(ctx, steamCommandTimeout, sessionHome, []string{password, guardCode}, onLine, args...)
	if err == nil || parseSteamLoginOutput(out).OK {
		writeSteamLoginMarker(sessionHome, username)
	}
	return out, steamUpdateError(out, appID, err)
}

func steamCachedLoginFailed(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "cached credential") || strings.Contains(low, "no cached credentials")
}

func steamUpdateError(out string, appID int64, err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(out)
	if strings.Contains(low, "no subscription") ||
		strings.Contains(low, "not authorized") ||
		strings.Contains(low, "does not own") ||
		strings.Contains(low, "no license") {
		return apperr.New("steam.account_no_license", 403).WithArg("app_id", appID)
	}
	if authErr := steamAuthError(out, err); authErr != err {
		return authErr
	}
	return err
}

// steamACFBuildID 从 steamapps/appmanifest_<appid>.acf 读取 buildid。
func steamACFBuildID(installDir string, appID int64) string {
	path := filepath.Join(installDir, "steamapps", fmt.Sprintf("appmanifest_%d.acf", appID))
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := reSteamBuildID.FindSubmatch(raw); len(m) == 2 {
		return string(m[1])
	}
	return ""
}

// steamErrIsNotInstalled 判断 SteamCMD 错误是否因为 SteamCMD 不可用。
func steamErrIsNotInstalled(err error) bool {
	if err == nil {
		return false
	}
	e, ok := apperr.As(err)
	return ok && e.Code == "steam.steamcmd_unavailable"
}

// ensureSteamSessionHome 返回并创建账号的持久化会话目录。
func ensureSteamSessionHome(root, accountID string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("steam: 会话目录根为空")
	}
	dir := filepath.Join(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// steamPlatformOrDefault 规范化平台参数。
func steamPlatformOrDefault(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "linux":
		return "linux"
	case "windows", "":
		return "windows"
	default:
		return ""
	}
}

// steamAccountUsableForUpdate 是一个显式用途闸门：账号模块只允许用于存储库下载。
//
// 目前没有玩家登录/通用命令入口；保留该函数是为了把"仅下载"约束写进代码路径，
// 后续任何人新增调用点时都必须经过它。
func steamAccountUsableForUpdate(a *domain.SteamAccount) error {
	if a == nil {
		return apperr.New("steam.account_not_found", 404)
	}
	if a.Status == domain.SteamAccountStatusDisabled {
		return apperr.New("steam.account_disabled", 409).WithArg("account_id", a.ID)
	}
	if a.Status != domain.SteamAccountStatusActive {
		// needs_2fa 可能只是上一次密码登录失败留下的状态；如果该账号已经有
		// 独立 SteamCMD 副本和缓存登录标记，先允许尝试缓存登录。缓存失效时
		// steamUpdateApp 仍会返回 needs_2fa，不会把验证码错误伪装成其它错误。
		if a.Status == domain.SteamAccountStatusNeeds2FA &&
			a.SessionHome != "" && canUseCachedLogin(a.SessionHome, a.Username) {
			return nil
		}
		return apperr.New("steam.account_not_active", 409).
			WithArg("account_id", a.ID).WithArg("status", string(a.Status))
	}
	return nil
}
