// Package releasever 实现对外发布版本的 semver 2.0 比较。
//
// 与 internal/semver 的区别：internal/semver 服务于客户端兼容区间，会裁掉
// `-beta` / `-rc` 这类预发布后缀；服务端自更新必须保留并正确比较它们。
package releasever

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是一个 semver 版本（不含 build metadata，比较时忽略）。
type Version struct {
	Major int
	Minor int
	Patch int
	// Pre 是点分隔的预发布标识；为空表示正式版。
	Pre []string
	// Raw 是解析时的原始字符串（仅用于展示/排障）。
	Raw string
}

// Parse 解析版本字符串，支持 `v` 前缀与 build metadata（`+build.1` 会被忽略）。
//
// 主干允许 1~3 段数字（`1`、`1.2`、`1.2.3`），缺省段按 0 处理，
// 这样发布标签与兼容区间之间的写法差异不会导致解析失败。
func Parse(raw string) (Version, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Version{}, fmt.Errorf("releasever: 版本为空")
	}
	original := s
	s = strings.TrimPrefix(s, "v")

	// build metadata 不影响优先级。
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	core := s
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core = s[:i]
		prePart := s[i+1:]
		if prePart == "" {
			return Version{}, fmt.Errorf("releasever: 预发布后缀为空: %q", raw)
		}
		pre = strings.Split(prePart, ".")
		for _, id := range pre {
			if id == "" {
				return Version{}, fmt.Errorf("releasever: 预发布标识为空: %q", raw)
			}
			if !validIdentifier(id) {
				return Version{}, fmt.Errorf("releasever: 非法预发布标识 %q: %q", id, raw)
			}
		}
	}

	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return Version{}, fmt.Errorf("releasever: 版本段数量非法: %q", raw)
	}
	nums := [3]int{}
	for i, p := range parts {
		if p == "" {
			return Version{}, fmt.Errorf("releasever: 版本段为空: %q", raw)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("releasever: 非法版本段 %q: %q", p, raw)
		}
		nums[i] = n
	}

	return Version{
		Major: nums[0],
		Minor: nums[1],
		Patch: nums[2],
		Pre:   pre,
		Raw:   original,
	}, nil
}

func validIdentifier(id string) bool {
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// String 返回规范化的版本字符串（不带 v 前缀）。
func (v Version) String() string {
	out := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		out += "-" + strings.Join(v.Pre, ".")
	}
	return out
}

// IsPrerelease 返回是否为预发布版本。
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// PreChannel 返回预发布通道（alpha / beta / rc）；正式版返回空串。
//
// 预发布标识的首段是通道名，例如 `rc.1` -> rc、`beta` -> beta。
// 未知首段按 beta 处理，避免把 `hotfix` 一类标签误判成正式版。
func (v Version) PreChannel() string {
	if len(v.Pre) == 0 {
		return ""
	}
	switch strings.ToLower(v.Pre[0]) {
	case "alpha", "a":
		return "alpha"
	case "beta", "b":
		return "beta"
	case "rc":
		return "rc"
	default:
		return "beta"
	}
}

// Compare 按 semver 优先级比较两个版本：a < b 返回 -1，相等返回 0，a > b 返回 1。
func Compare(a, b Version) int {
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	return comparePre(a.Pre, b.Pre)
}

// comparePre 实现 semver 的预发布优先级：
//   - 有预发布 < 无预发布；
//   - 逐段比较：数字段按数值，数字段 < 非数字段，非数字段按字典序；
//   - 前缀相同时，段数少的更小。
func comparePre(a, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

func compareIdentifier(a, b string) int {
	an, aNum := parseNumeric(a)
	bn, bNum := parseNumeric(b)
	switch {
	case aNum && bNum:
		return cmpInt(an, bn)
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func parseNumeric(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
