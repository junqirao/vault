package agent

import "testing"

// TestShortcutTarget 锁定快捷方式目标口径：盘符模式补根斜杠，目录模式原样。
//
// 与前端手动打开挂载点的 openTargetOf 保持一致 —— 两者指向的应该是同一个位置。
func TestShortcutTarget(t *testing.T) {
	cases := []struct {
		name      string
		mountMode string
		mountPath string
		want      string
	}{
		{"盘符补根斜杠", mountModeLetter, `E:`, `E:\`},
		{"盘符已带斜杠不重复补", mountModeLetter, `E:\`, `E:\`},
		{"目录模式原样", mountModeDirectory, `D:\Vault\游戏`, `D:\Vault\游戏`},
		{"空路径保持空", mountModeLetter, "  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortcutTarget(tc.mountMode, tc.mountPath); got != tc.want {
				t.Fatalf("shortcutTarget(%q, %q) = %q，期望 %q", tc.mountMode, tc.mountPath, got, tc.want)
			}
		})
	}
}
