package agent

import (
	"path/filepath"
	"testing"
)

// newUpdateStateTestAgent 构造只带更新状态所需依赖的代理（不启 HTTP、不碰网络）。
func newUpdateStateTestAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStateStore(filepath.Join(dir, "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	cfg, err := NewConfigStore(filepath.Join(dir, "agent-config.json"), testLogger())
	if err != nil {
		t.Fatalf("构造配置存储失败：%v", err)
	}
	return &Agent{logger: testLogger(), store: store, cfg: cfg, hub: NewEventHub()}
}

// TestClearAvailableUpdateOnDefinitiveReason 覆盖"确认无更新后必须清掉旧目标版本"。
//
// 否则 /agent/state 会一直带着上一次发现的新版本，顶栏更新提示永远消不掉
// （真实场景：用户已经更新/发布被撤回，下一次检查返回 not_newer，但界面仍显示有更新）。
func TestClearAvailableUpdateOnDefinitiveReason(t *testing.T) {
	for _, reason := range []string{"not_newer", "no_manifest", "no_artifact", "bad_version"} {
		t.Run(reason, func(t *testing.T) {
			a := newUpdateStateTestAgent(t)
			a.availableVersion = "0.2.0"

			a.clearAvailableUpdate(reason)
			if got := a.updateInfo().AvailableVersion; got != "" {
				t.Fatalf("%s 后 AvailableVersion = %q，期望空", reason, got)
			}
		})
	}
}

// TestClearAvailableUpdateKeepsOnTransientReason 覆盖"暂时性失败不能清掉已知更新"。
//
// 服务端不可达 / 尚未登录时只是这次没拿到清单，不能因此把上一次确认过的新版本藏起来。
func TestClearAvailableUpdateKeepsOnTransientReason(t *testing.T) {
	for _, reason := range []string{"server_unreachable", "no_session"} {
		t.Run(reason, func(t *testing.T) {
			a := newUpdateStateTestAgent(t)
			a.availableVersion = "0.2.0"

			a.clearAvailableUpdate(reason)
			if got := a.updateInfo().AvailableVersion; got != "0.2.0" {
				t.Fatalf("%s 后 AvailableVersion = %q，期望保留 0.2.0", reason, got)
			}
		})
	}
}
