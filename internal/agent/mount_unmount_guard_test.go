package agent

import "testing"

// TestSameMountPath 锁定卸载前的"挂载点还属于这块盘吗"比较口径。
//
// 背景（真实反馈："点卸载存储库 A 把 B 也给卸载了"）：记录里的盘号/盘符是上次挂载时的现场，
// 重启后可能已被系统复用给另一块盘。卸载前必须核对记录里的挂载点当前是否仍指向同一位置，
// 对不上就只能断会话、绝不能按旧盘号下线或按旧盘符移除 —— 否则会把别人的盘摘掉。
func TestSameMountPath(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"盘符相同", "E:", "E:", true},
		{"盘符末尾带分隔符", "E:", `E:\`, true},
		{"目录大小写与末尾分隔符不敏感", `D:\Vault\Lib`, `d:\vault\lib\`, true},
		{"不同盘符", "E:", "F:", false},
		{"不同目录", `D:\Vault\Lib`, `D:\Vault\Lib-2`, false},
		{"空值不算命中", "", "", false},
		{"一侧为空不算命中", "", "E:", false},
	}
	for _, tc := range cases {
		if got := sameMountPath(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameMountPath(%q, %q)=%v，期望 %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}
