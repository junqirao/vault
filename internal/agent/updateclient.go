package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
	"vault/internal/apperr"
	"vault/internal/updatepkg"
)

// 客户端本体更新（kind=client）
//
// 与渲染层资源热更（client_web）是两条独立的通道：
//   - client_web 只替换渲染层静态资源，不重启进程；
//   - client 替换**整个 Electron 程序目录**（Vault.exe + resources/app.asar + 运行时），
//     由客户端主进程在下次启动时切换到新目录（见 frontend/apps/client/electron/clientUpdate.ts）。
//
// 落盘布局（与 Electron 主进程的**冻结契约**，不要改名）：
//
//	<代理数据目录>/client/                 根目录（%ProgramData%\Vault\client）
//	<client>/<version>/                    程序目录（Vault.exe、resources/app.asar、locales/…）
//	<client>/current.json                  激活指针 {"version","dir","activated_at"}
//
// 代理只负责"下载 → 校验 → 解压 → 写指针"，**绝不自己重启客户端**：
// 何时切换由主进程决定（有活跃挂载时必须等卸载，见 §7.4.4 的说明）。

const (
	// clientDirName 客户端本体根目录名（相对于代理数据目录）。
	clientDirName = "client"
	// clientCurrentFileName 激活指针文件名（<client>/current.json）。
	clientCurrentFileName = "current.json"
	// clientExeName 客户端主程序文件名；解压后必须存在它才算有效产物。
	clientExeName = "Vault.exe"
	// clientAsarRel 客户端 renderer/主进程代码包（相对程序目录）；缺它则程序起不来。
	clientAsarRel = "resources/app.asar"
	// clientMinFiles 程序目录的最小文件数。
	//
	// 阈值与 build.py 的 MIN_APP_FILES 保持一致（精简后约 22 个文件）：只用来识破
	// "解压不完整/空壳"，不需要贴近真实数量。
	clientMinFiles = 15
	// clientVersionsKept 保留的历史版本目录数（当前 + 上一个，便于出问题时手工回退）。
	clientVersionsKept = 2
)

// 客户端本体更新状态取值（ClientUpdateState.state）。
const (
	// ClientStateIdle 空闲（尚未准备过，且磁盘上没有已就绪版本）。
	ClientStateIdle = "idle"
	// ClientStateDownloading 正在下载客户端安装包。
	ClientStateDownloading = "downloading"
	// ClientStateVerifying 正在校验（大小 / sha256）。
	ClientStateVerifying = "verifying"
	// ClientStateExtracting 正在解压并准备激活。
	ClientStateExtracting = "extracting"
	// ClientStateReady 新版本已就绪（current.json 已指向它），等客户端重启切换。
	ClientStateReady = "ready"
	// ClientStateFailed 失败（error 为稳定错误码）。
	ClientStateFailed = "failed"
)

// ClientUpdateState 是客户端本体更新的状态（GET /agent/update/client）。
type ClientUpdateState struct {
	// State idle|downloading|verifying|extracting|ready|failed。
	State string `json:"state"`
	// AvailableVersion 本次尝试更新到的目标版本（解析清单后填充）。
	AvailableVersion string `json:"available_version,omitempty"`
	// ReadyVersion 已就绪、等客户端重启切换的版本（current.json 指向的版本）。
	ReadyVersion string `json:"ready_version,omitempty"`
	// ReceivedBytes / TotalBytes 下载进度。
	ReceivedBytes int64 `json:"received_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
	// Error 失败时的稳定错误码（如 agent.client_update_failed / not_newer / no_artifact）。
	Error string `json:"error,omitempty"`
	// UpdatedAt 最后一次状态变更的毫秒时间戳。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// clientCurrentPointer 是 <client>/current.json 的内容（激活指针）。
type clientCurrentPointer struct {
	// Version 已就绪的客户端版本。
	Version string `json:"version"`
	// Dir 版本目录名（当前等于 Version；保留字段以便将来支持同版本多目录）。
	Dir string `json:"dir"`
	// ActivatedAt 就绪时间（毫秒时间戳）。
	ActivatedAt int64 `json:"activated_at"`
}

// clientUpdateManager 管理客户端本体更新的内存态与下载进度。
//
// 磁盘上的"已就绪版本"以 current.json 为准（代理重启后依然有效），内存态只负责展示与并发保护。
type clientUpdateManager struct {
	a *Agent

	mu      sync.Mutex
	running bool
	seeded  bool
	state   ClientUpdateState

	received atomic.Int64
	total    atomic.Int64
	throttle progressThrottle
}

func newClientUpdateManager(a *Agent) *clientUpdateManager {
	return &clientUpdateManager{
		a:        a,
		state:    ClientUpdateState{State: ClientStateIdle},
		throttle: newProgressThrottle(0),
	}
}

// snapshot 返回状态快照；首次调用时从 current.json 恢复 ready_version。
func (m *clientUpdateManager) snapshot() ClientUpdateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.seeded {
		m.seeded = true
		if cur, ok := readClientCurrent(m.a.clientCurrentPath()); ok {
			m.state.ReadyVersion = cur.Version
			if m.state.State == "" || m.state.State == ClientStateIdle {
				m.state.State = ClientStateReady
			}
		}
	}
	if m.state.State == "" {
		m.state.State = ClientStateIdle
	}
	return m.state
}

// setState 在锁内变更状态并推送 client_update 事件。
func (m *clientUpdateManager) setState(mutate func(*ClientUpdateState)) {
	m.mu.Lock()
	mutate(&m.state)
	m.state.UpdatedAt = time.Now().UnixMilli()
	m.mu.Unlock()
	m.a.publishClientState()
}

// reset 实现 artifactProgress：重置进度基线并推送一次。
func (m *clientUpdateManager) reset(total, received int64) {
	if total > 0 {
		m.total.Store(total)
	}
	m.received.Store(received)
	m.mu.Lock()
	m.throttle.allow(true, received, time.Now())
	m.mu.Unlock()
	m.push(received, m.total.Load())
}

// add 实现 artifactProgress：累加已收字节并按节流推送。
func (m *clientUpdateManager) add(n int64) {
	if n <= 0 {
		return
	}
	got := m.received.Add(n)
	m.mu.Lock()
	ok := m.throttle.allow(false, got, time.Now())
	m.mu.Unlock()
	if !ok {
		return
	}
	m.push(got, m.total.Load())
}

func (m *clientUpdateManager) push(received, total int64) {
	m.setState(func(s *ClientUpdateState) {
		s.ReceivedBytes = received
		if total > 0 {
			s.TotalBytes = total
		}
	})
}

func (m *clientUpdateManager) finishRun() {
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// publishClientState 广播 client_update 事件（data 即 ClientUpdateState）。
func (a *Agent) publishClientState() {
	if a.client == nil {
		return
	}
	a.hub.Publish(Event{Type: "client_update", Data: a.client.snapshot()})
}

// clientUpdateState 返回客户端本体更新状态（供 GET /agent/update/client）。
func (a *Agent) clientUpdateState() ClientUpdateState {
	if a.client == nil {
		return ClientUpdateState{State: ClientStateIdle}
	}
	return a.client.snapshot()
}

// startClientUpdate 启动一次后台客户端本体更新并立即返回当前状态（幂等：已在运行直接返回）。
//
// currentVersion 是**当前正在运行的客户端版本**（由 Electron 主进程通过请求体传入），
// 用于版本单调性判断——不能拿代理版本当基准（见 resolveUpdateKindAgainst 的说明）。
func (a *Agent) startClientUpdate(currentVersion string) ClientUpdateState {
	m := a.client
	if m == nil {
		return ClientUpdateState{State: ClientStateIdle}
	}
	ready := m.snapshot().ReadyVersion
	m.mu.Lock()
	if m.running {
		s := m.state
		m.mu.Unlock()
		return s
	}
	m.running = true
	m.state = ClientUpdateState{
		State:         ClientStateDownloading,
		ReadyVersion:  ready,
		UpdatedAt:     time.Now().UnixMilli(),
	}
	s := m.state
	m.mu.Unlock()
	a.publishClientState()
	safeGo(a.logger, "client_update", func() { a.runClientUpdate(a.bgContext(), currentVersion) })
	return s
}

// runClientUpdate 后台执行"解析清单 → 下载 → 校验 → 解压 → 激活"。
func (a *Agent) runClientUpdate(ctx context.Context, currentVersion string) {
	m := a.client
	defer m.finishRun()

	// 单调性基准：取"正在运行的客户端版本"与"磁盘上已就绪版本"中的较大者。
	//
	// 只比运行版本不够：用户可能手工把客户端换回旧版，而 current.json 仍指向新版，
	// 此时必须允许重新下发（而不是被 not_newer 拦下）。
	baseline := strings.TrimSpace(currentVersion)
	if cur, ok := readClientCurrent(a.clientCurrentPath()); ok {
		if baseline == "" || mustBeNewer(cur.Version, baseline) {
			baseline = cur.Version
		}
	}

	cand, res, err := a.resolveUpdateKindAgainst(ctx, updatepkg.KindClient, baseline)
	if err != nil {
		a.logger.Warn("客户端本体更新检查失败", "error", err)
		m.setState(func(s *ClientUpdateState) {
			s.State = ClientStateFailed
			s.Error = codeOfError(err)
		})
		return
	}
	if cand == nil {
		reason := "not_available"
		if res != nil && res.Reason != "" {
			reason = res.Reason
		}
		a.logger.Info("没有可用的客户端本体更新", "reason", reason, "baseline", baseline)
		m.setState(func(s *ClientUpdateState) {
			s.State = ClientStateFailed
			s.Error = reason
		})
		return
	}

	target := cand.manifest.Version
	m.setState(func(s *ClientUpdateState) {
		s.State = ClientStateDownloading
		s.AvailableVersion = target
		s.ReceivedBytes = 0
		s.TotalBytes = cand.artifact.Size
		s.Error = ""
	})

	// 幂等：目标版本已就绪且目录完整 → 直接报告 ready，不重复下载。
	if cur, ok := readClientCurrent(a.clientCurrentPath()); ok && sameVersion(cur.Version, target) {
		if isCompleteClientDir(a.clientVersionDir(target)) {
			a.logger.Info("客户端本体已是目标版本，无需重复准备", "version", target)
			m.setState(func(s *ClientUpdateState) {
				s.State = ClientStateReady
				s.ReadyVersion = target
				s.ReceivedBytes = cand.artifact.Size
				s.TotalBytes = cand.artifact.Size
			})
			return
		}
		// 目录被删除/损坏：继续走下载，覆盖它。
	}

	if err := a.applyClientUpdate(ctx, cand); err != nil {
		a.logger.Warn("客户端本体更新失败", "version", target, "error", err)
		m.setState(func(s *ClientUpdateState) {
			s.State = ClientStateFailed
			s.Error = codeOfError(err)
		})
		return
	}
	m.setState(func(s *ClientUpdateState) {
		s.State = ClientStateReady
		s.ReadyVersion = target
		s.ReceivedBytes = cand.artifact.Size
		s.TotalBytes = cand.artifact.Size
		s.Error = ""
	})
	a.logger.Info("客户端本体已就绪，等待客户端重启切换", "version", target)
}

// applyClientUpdate 执行"下载 → 校验 → 解压 → 原子激活"；失败时不留下半激活状态。
func (a *Agent) applyClientUpdate(ctx context.Context, cand *updateCandidate) error {
	version := cand.manifest.Version
	root := a.clientRootDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return errClientUpdateFailed("prepare_dir", err)
	}
	cleanupClientTemp(root, a.logger)

	zipPath := filepath.Join(root, cand.artifact.Filename+partSuffix)
	if err := a.downloadArtifact(ctx, cand.client, cand.artifact, zipPath, a.client); err != nil {
		return err
	}
	defer func() { _ = os.Remove(zipPath) }()

	a.client.setState(func(s *ClientUpdateState) { s.State = ClientStateVerifying })
	if err := verifyArtifactFile(zipPath, cand.artifact); err != nil {
		return err
	}

	a.client.setState(func(s *ClientUpdateState) { s.State = ClientStateExtracting })

	suffix, err := randomHex(4)
	if err != nil {
		return errClientUpdateFailed("extract", err)
	}
	tmpDir := filepath.Join(root, version+webTempPrefix+suffix)
	if err := os.RemoveAll(tmpDir); err != nil {
		return errClientUpdateFailed("extract", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return errClientUpdateFailed("extract", err)
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	// 复用渲染层热更的 zip slip 防护与解压体积上限（同一套安全约束）。
	if err := extractWebZip(zipPath, tmpDir, webExtractMaxBytes(cand.artifact.Size)); err != nil {
		return errClientUpdateFailed("extract", err)
	}
	if !isCompleteClientDir(tmpDir) {
		return errClientUpdateFailed("incomplete", nil)
	}

	finalDir := a.clientVersionDir(version)
	if err := os.RemoveAll(finalDir); err != nil {
		return errClientUpdateFailed("replace_version", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return errClientUpdateFailed("activate", err)
	}
	activated = true

	if err := writeClientCurrent(a.clientCurrentPath(), clientCurrentPointer{
		Version:     version,
		Dir:         version,
		ActivatedAt: time.Now().UnixMilli(),
	}); err != nil {
		// 程序目录已就位但指针未更新：current.json 仍指向旧版本，旧版本保持可用，
		// 绝不留下"指向不存在目录"的半激活状态。
		return errClientUpdateFailed("activate_pointer", err)
	}
	pruneClientVersions(root, version, a.logger)
	return nil
}

// clientRootDir 返回客户端本体根目录：<代理数据目录>/client（%ProgramData%\Vault\client）。
func (a *Agent) clientRootDir() string { return filepath.Join(DataDir(), clientDirName) }

// clientCurrentPath 返回激活指针路径：<client>/current.json。
func (a *Agent) clientCurrentPath() string {
	return filepath.Join(a.clientRootDir(), clientCurrentFileName)
}

// clientVersionDir 返回指定版本的客户端程序目录：<client>/<version>。
func (a *Agent) clientVersionDir(version string) string {
	return filepath.Join(a.clientRootDir(), version)
}

// isCompleteClientDir 判断目录是否是**完整可运行**的客户端程序目录。
//
// 判据与 build.py 的 is_complete_app_dir 对齐：必备文件 + 最小文件数。
// 用它挡住"解压不完整 / zip 里少东西"的半成品——把一个跑不起来的目录交给主进程，
// 用户重启后就没有界面了。
func isCompleteClientDir(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, clientExeName)); err != nil || fi.IsDir() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clientAsarRel))); err != nil || fi.IsDir() {
		return false
	}
	count := 0
	walkErr := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		if count >= clientMinFiles {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return false
	}
	return count >= clientMinFiles
}

// cleanupClientTemp 清理 <client> 下遗留的临时解压目录与下载残留（尽力而为）。
func cleanupClientTemp(root string, logger *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir() && strings.Contains(name, webTempPrefix):
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				logger.Warn("清理客户端更新临时目录失败", "dir", name, "error", err)
			}
		case !e.IsDir() && strings.HasSuffix(name, partSuffix):
			_ = os.Remove(filepath.Join(root, name))
		}
	}
}

// pruneClientVersions 只保留最近 clientVersionsKept 个版本目录，更旧的删除。
//
// 保留上一个版本不是浪费：新版起不来时用户可以直接从旧目录启动（见 clientUpdate.ts
// 的坏版本判定），删光就没有退路了。
func pruneClientVersions(root, keepVersion string, logger *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type versionedDir struct {
		name string
		at   int64
	}
	dirs := make([]versionedDir, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.Contains(e.Name(), webTempPrefix) {
			continue
		}
		if e.Name() == keepVersion {
			continue
		}
		if !isCompleteClientDir(filepath.Join(root, e.Name())) {
			// 残缺目录直接清掉（半途失败留下的空壳）。
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
			continue
		}
		at := int64(0)
		if info, err := e.Info(); err == nil {
			at = info.ModTime().UnixNano()
		}
		dirs = append(dirs, versionedDir{name: e.Name(), at: at})
	}
	// 新→旧排序：保留最前面的 clientVersionsKept-1 个（当前版本已单独排除）。
	for i := 0; i < len(dirs); i++ {
		for j := i + 1; j < len(dirs); j++ {
			if dirs[j].at > dirs[i].at {
				dirs[i], dirs[j] = dirs[j], dirs[i]
			}
		}
	}
	for i, d := range dirs {
		if i < clientVersionsKept-1 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, d.name)); err != nil {
			logger.Warn("清理旧客户端版本目录失败", "dir", d.name, "error", err)
		} else {
			logger.Info("已清理旧客户端版本目录", "dir", d.name)
		}
	}
}

// writeClientCurrent 原子写入激活指针（临时文件 + MoveFileEx 替换）。
//
// 与 writeWebCurrent 用同一套语义：Windows 上 os.Rename **不会覆盖**已存在的目标，
// 必须用 MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH)。
func writeClientCurrent(path string, ptr clientCurrentPointer) error {
	data, err := json.MarshalIndent(ptr, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readClientCurrent 读取激活指针；不存在或不可解析时 ok=false。
func readClientCurrent(path string) (clientCurrentPointer, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return clientCurrentPointer{}, false
	}
	var ptr clientCurrentPointer
	if err := json.Unmarshal(data, &ptr); err != nil {
		return clientCurrentPointer{}, false
	}
	if strings.TrimSpace(ptr.Version) == "" {
		return clientCurrentPointer{}, false
	}
	return ptr, true
}

// mustBeNewer 判断 candidate 是否严格新于 current；任一版本不可解析时按"是"处理。
//
// 这里刻意"宽松"：baseline 只用于挑出更大的那个作为后续单调性基准，
// 解析失败时宁可多比较一次，最终判定仍由 resolveUpdateKindAgainst 的正规校验负责。
func mustBeNewer(candidate, current string) bool {
	newer, err := isNewer(candidate, current)
	if err != nil {
		return true
	}
	return newer
}

// codeOfError 取错误的稳定错误码；没有则给出通用码（供状态展示）。
func codeOfError(err error) string {
	if code := apperr.CodeOf(err); code != "" {
		return code
	}
	return CodeClientUpdateFailed
}

// String 便于日志输出（避免直接打印结构体指针）。
func (s ClientUpdateState) String() string {
	return fmt.Sprintf("state=%s available=%s ready=%s error=%s", s.State, s.AvailableVersion, s.ReadyVersion, s.Error)
}
