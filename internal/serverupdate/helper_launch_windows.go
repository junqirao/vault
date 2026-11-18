//go:build windows

package serverupdate

// startHelperProcess 在 Windows 上直接以脱离进程启动 helper。
func startHelperProcess(exe string, args []string, workDir, logPath string, _ bool) (int, error) {
	return startDetached(exe, args, workDir, logPath)
}
