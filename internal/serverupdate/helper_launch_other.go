//go:build !windows && !linux

package serverupdate

// startHelperProcess 在非 Windows/Linux 平台直接以脱离进程启动 helper。
func startHelperProcess(exe string, args []string, workDir, logPath string, _ bool) (int, error) {
	return startDetached(exe, args, workDir, logPath)
}
