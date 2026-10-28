//go:build linux

package linuxlvm

import "testing"

// TestDevMajorMinor 锁定 dev_t 解码。
//
// blockDeviceOfPath 靠它拼 /sys/dev/block/<major>:<minor>：解错就等于永远找不到
// 承载目录的块设备，目录模式的存储一律被标成 unknown —— 这正是要修的现象。
// 期望值按 glibc 的 gnu_dev_major / gnu_dev_minor 语义手工计算。
func TestDevMajorMinor(t *testing.T) {
	cases := []struct {
		name  string
		dev   uint64
		major uint64
		minor uint64
	}{
		{name: "sda1", dev: 0x0811, major: 8, minor: 17},
		{name: "nvme0n1（major 259）", dev: 0x10300, major: 259, minor: 0},
		{name: "dm-0", dev: 0xfd00, major: 253, minor: 0},
		{name: "高位 major（>4095）", dev: 0x100000023478, major: 0x1234, minor: 0x78},
		{name: "零值", dev: 0, major: 0, minor: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := devMajor(c.dev); got != c.major {
				t.Fatalf("devMajor(0x%x) = %d，期望 %d", c.dev, got, c.major)
			}
			if got := devMinor(c.dev); got != c.minor {
				t.Fatalf("devMinor(0x%x) = %d，期望 %d", c.dev, got, c.minor)
			}
		})
	}
}

// TestMediaTypeOfNonBlockSource 锁定"挂载源不是块设备时不要瞎猜"：
// overlay / tmpfs / nfs 的 source 拿来当设备名查 sysfs 必然查不到，必须回落到按路径探测，
// 而不是把 "overlay" 当成一个块设备去猜。
func TestMediaTypeOfNonBlockSource(t *testing.T) {
	for _, src := range []string{"", "overlay", "tmpfs", "none", "nfs", "/dev/does-not-exist"} {
		if got := mediaTypeOfDevice(src); got != "" {
			t.Fatalf("mediaTypeOfDevice(%q) = %q，期望空串（识别不出）", src, got)
		}
	}
}
