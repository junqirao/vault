//go:build windows

package app

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// systemProxyInfo 读取 Windows 的“系统代理”设置。
//
// 注意：这是 WinINET 的每用户设置；如果 Vault 以服务身份运行在另一个账户下，
// HKCU 指向的是服务账户而不是当前登录用户。此时请使用 steam.proxy 显式配置。
func systemProxyInfo() (proxy, source, hint string) {
	if p, src, h := readInternetSettings(registry.CURRENT_USER); p != "" || src != "" {
		return p, src, h
	}
	if p, src, h := readInternetSettings(registry.LOCAL_MACHINE); p != "" || src != "" {
		return p, src, h
	}
	return "", "", ""
}

func readInternetSettings(root registry.Key) (proxy, source, hint string) {
	const path = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", "", ""
	}
	defer k.Close() //nolint:errcheck

	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil {
		enabled = 0
	}
	server, _, _ := k.GetStringValue("ProxyServer")
	pac, _, _ := k.GetStringValue("AutoConfigURL")

	if enabled != 0 && strings.TrimSpace(server) != "" {
		return normalizeProxyServer(server), "system", ""
	}
	if strings.TrimSpace(pac) != "" {
		return "", "pac", pac
	}
	return "", "", ""
}

// normalizeProxyServer 处理 Windows ProxyServer 的两种格式：
//   - "127.0.0.1:7890"
//   - "http=127.0.0.1:7890;https=127.0.0.1:7890"
func normalizeProxyServer(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") {
		var httpProxy, httpsProxy string
		for _, part := range strings.Split(raw, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(kv[0])) {
			case "https":
				httpsProxy = strings.TrimSpace(kv[1])
			case "http":
				httpProxy = strings.TrimSpace(kv[1])
			}
		}
		if httpsProxy != "" {
			raw = httpsProxy
		} else if httpProxy != "" {
			raw = httpProxy
		}
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return raw
}
