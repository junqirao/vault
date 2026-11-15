package api

import (
	"log/slog"
	"testing"
)

// TestSteamRoutesRegister 验证账号/分组/游戏路由与现有路由不冲突。
// chi 在注册冲突模式时会 panic；这条测试让启动期问题在 CI 里直接暴露。
func TestSteamRoutesRegister(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("路由注册 panic: %v", r)
		}
	}()
	router := New(Deps{Log: slog.Default()})
	if router == nil {
		t.Fatal("router 为 nil")
	}
}
