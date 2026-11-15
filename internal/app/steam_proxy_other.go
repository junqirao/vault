//go:build !windows

package app

// systemProxyInfo 在非 Windows 上不读取系统代理；由 steam.proxy 或环境变量控制。
func systemProxyInfo() (proxy, source, hint string) {
	return "", "", ""
}
