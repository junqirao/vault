package domain

import "testing"

// TestRepoMetaAutoCompactDefault 默认未配置即开启；显式 false 才关闭。
func TestRepoMetaAutoCompactDefault(t *testing.T) {
	var meta RepoMeta
	if !meta.AutoCompactEnabled() {
		t.Fatal("未配置 auto_compact 时应默认开启")
	}
	off := false
	meta.AutoCompact = &off
	if meta.AutoCompactEnabled() {
		t.Fatal("显式关闭后不应再判定为开启")
	}
	on := true
	meta.AutoCompact = &on
	if !meta.AutoCompactEnabled() {
		t.Fatal("显式开启后应判定为开启")
	}
}
