package app

import (
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
)

// 每个账号一份 SteamCMD 安装副本。
//
// 原因（已在本机实测确认）：SteamCMD 的登录态永远写在**可执行文件所在目录**的
// config/config.vdf 里，不受 cwd、HOME、USERPROFILE、XDG_CONFIG_HOME 影响。
// 共享一个安装目录时，多账号只能靠"运行前恢复快照、运行后保存快照"模拟切换；
// 一旦进程崩溃/服务重启，刚签发的 token 就会丢，表现为反复要求 Steam Guard。
//
// 独立副本让登录态天然跟着账号走：不需要快照，不需要搬运，也不会互相覆盖。
const steamCmdDirName = "steamcmd"

// steamCmdRootFor 返回账号独立的 SteamCMD 安装根目录。
//
// 空 sessionHome 表示匿名/共享调用，直接使用主安装目录。
func (d Deps) steamCmdRootFor(sessionHome string) string {
	if strings.TrimSpace(sessionHome) == "" {
		return ""
	}
	return filepath.Join(sessionHome, steamCmdDirName)
}

// steamCmdMasterDir 返回用于复制/自动安装的"主"SteamCMD 目录。
//
// 优先使用显式配置的可执行文件所在目录，其次使用自动安装目录。
// 只有该目录里确实存在 SteamCMD 可执行文件时才认为可用于复制。
func (d Deps) steamCmdMasterDir() (string, bool) {
	if p := strings.TrimSpace(d.raw().Steam.SteamCmdPath); p != "" && fileExists(p) {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		return filepath.Dir(p), true
	}
	dir := d.steamCmdInstallDir()
	if fileExists(filepath.Join(dir, steamCmdBinaryName())) {
		return dir, true
	}
	return "", false
}

// steamCmdLockKey 返回本次调用需要串行化的安装根。
func (d Deps) steamCmdLockKey(sessionHome string) string {
	if root := d.steamCmdRootFor(sessionHome); root != "" {
		// 正常情况：账号有独立副本，锁在副本目录上。
		if _, managed := d.steamCmdMasterDir(); managed {
			return root
		}
		if fileExists(filepath.Join(root, steamCmdBinaryName())) {
			return root
		}
		// 未管理安装会退回 PATH 里的共享 steamcmd，此时必须按共享目录加锁。
		if p := lookPathSteamCmd(); p != "" {
			return filepath.Dir(p)
		}
		return root
	}
	if master, ok := d.steamCmdMasterDir(); ok {
		return master
	}
	return "__shared__"
}

// steamCmdForSession 返回本次调用应该执行的 steamcmd 路径和安装根。
//
// 账号目录第一次使用时从主安装目录复制一份；之后永久复用这份副本的登录态。
func (d Deps) steamCmdForSession(sessionHome string) (path, root string, err error) {
	master, managed := d.steamCmdMasterDir()
	if strings.TrimSpace(sessionHome) == "" {
		if managed {
			return filepath.Join(master, steamCmdBinaryName()), master, nil
		}
		if p := lookPathSteamCmd(); p != "" {
			return p, filepath.Dir(p), nil
		}
		return "", "", apperr.New("steam.steamcmd_unavailable", httpStatusNotImplemented)
	}

	root = d.steamCmdRootFor(sessionHome)
	bin := filepath.Join(root, steamCmdBinaryName())
	if fileExists(bin) {
		return bin, root, nil
	}

	if !managed {
		// 只有 PATH 里的 steamcmd、没有可复制的主安装目录时退回共享模式。
		// 这无法隔离多账号登录态，所以优先建议用运行时安装功能装一份可管理的副本。
		if p := lookPathSteamCmd(); p != "" {
			if d.Log != nil {
				d.Log.Warn("SteamCMD 未安装到可管理目录，账号将共享登录态",
					"account_home", sessionHome, "path", p)
			}
			return p, filepath.Dir(p), nil
		}
		return "", "", apperr.New("steam.steamcmd_unavailable", httpStatusNotImplemented)
	}

	if err := provisionSteamCmdRoot(master, root, sessionHome); err != nil {
		return "", "", apperr.New("steam.steamcmd_provision_failed", 500).
			WithArg("source", master).WithArg("target", root).WithCause(err)
	}
	if !fileExists(bin) {
		return "", "", apperr.New("steam.steamcmd_unavailable", httpStatusNotImplemented)
	}
	return bin, root, nil
}

func lookPathSteamCmd() string {
	for _, name := range []string{"steamcmd", "steamcmd.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// provisionSteamCmdRoot 从主安装目录复制一份完整 SteamCMD 到账号目录。
//
// 先复制到临时目录再重命名，避免复制中途失败留下半个安装。config 不复制：
// 登录态必须属于当前账号，旧版快照会单独迁移。
func provisionSteamCmdRoot(master, root, sessionHome string) error {
	if fileExists(filepath.Join(root, steamCmdBinaryName())) {
		return nil
	}
	if _, err := os.Stat(root); err == nil {
		// 目录存在但没有可执行文件，说明上次复制中断；清掉重来。
		if err := os.RemoveAll(root); err != nil {
			return err
		}
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(parent, ".steamcmd-provision-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck

	if err := copySteamCmdInstall(master, tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "config"), 0o700); err != nil {
		return err
	}
	migrateLegacySteamCmdSession(sessionHome, tmp)
	if !fileExists(filepath.Join(tmp, steamCmdBinaryName())) {
		return apperr.New("steam.steamcmd_unavailable", httpStatusNotImplemented)
	}
	if err := os.Rename(tmp, root); err != nil {
		if fileExists(filepath.Join(root, steamCmdBinaryName())) {
			return nil
		}
		return err
	}
	return nil
}

// copySteamCmdInstall 复制 SteamCMD 安装目录，跳过与登录态/日志无关的可变数据。
func copySteamCmdInstall(src, dst string) error {
	return filepath.WalkDir(src, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skipSteamCmdCopyPath(rel, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFileWithMode(p, target, info.Mode().Perm())
		}
	})
}

// skipSteamCmdCopyPath 过滤不该进入账号副本的内容。
//
// config 必须为空，否则会把主安装目录里残留的别的账号登录态带进来；
// logs/appcache/depotcache 是运行时缓存；*.old 是自更新留下的旧二进制。
func skipSteamCmdCopyPath(rel string, entry fs.DirEntry) bool {
	name := entry.Name()
	if strings.HasSuffix(strings.ToLower(name), ".old") {
		return true
	}
	if entry.IsDir() {
		switch name {
		case "config", "logs", "appcache", "depotcache", "steamapps", "userdata":
			return true
		}
	}
	switch {
	case strings.HasPrefix(rel, "ssfn"):
		return true
	case strings.HasPrefix(rel, filepath.Join("config", "ssfn")):
		return true
	}
	return false
}

func copyFileWithMode(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// migrateLegacySteamCmdSession 把旧版"共享安装 + 账号快照"里的登录态搬进新副本。
//
// 迁移成功后把旧目录改名，避免以后重建副本时又把过期 token 带回来。
func migrateLegacySteamCmdSession(sessionHome, cmdRoot string) {
	stateDir := filepath.Join(sessionHome, "steamcmd-state")
	if info, err := os.Stat(stateDir); err != nil || !info.IsDir() {
		return
	}
	if fileExists(filepath.Join(cmdRoot, "config", "config.vdf")) {
		return
	}
	if err := copySteamCmdSession(stateDir, cmdRoot); err == nil && fileExists(filepath.Join(cmdRoot, "config", "config.vdf")) {
		_ = os.Rename(stateDir, stateDir+".migrated")
	}
}
