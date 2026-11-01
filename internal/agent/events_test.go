package agent

import "testing"

// TestEventHubCloseReleasesSubscribers 锁定退出路径的契约：Close 之后所有订阅通道立刻
// 关闭（本地 SSE handler 因此返回，http.Server.Shutdown 不用等长连接超时），并且随后
// 再到来的 Unsubscribe / Publish 都不会 panic。
func TestEventHubCloseReleasesSubscribers(t *testing.T) {
	hub := NewEventHub()
	id, ch := hub.Subscribe()

	hub.Close()

	select {
	case _, open := <-ch:
		if open {
			t.Fatal("Close 之后订阅通道应已关闭")
		}
	default:
		t.Fatal("Close 之后订阅通道应已关闭（读取应立即返回）")
	}

	// 退出路径上的真实顺序：Close（handleShutdown）→ UnmountAll 期间仍可能发布事件
	// → SSE handler 的 defer Unsubscribe。两者都不能重复关闭通道。
	hub.Publish(Event{Type: "unmount"})
	hub.Unsubscribe(id)

	if hub.HasSubscribers() {
		t.Fatal("Close 之后不应再有订阅者")
	}
}
