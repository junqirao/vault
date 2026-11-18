package releasever

import "testing"

func mustParse(t *testing.T, raw string) Version {
	t.Helper()
	v, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析 %q 失败：%v", raw, err)
	}
	return v
}

func TestParseAndString(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		pre  bool
	}{
		{raw: "v0.3.1", want: "0.3.1"},
		{raw: "0.3.1-beta", want: "0.3.1-beta", pre: true},
		{raw: "v0.3.1-rc.1", want: "0.3.1-rc.1", pre: true},
		{raw: "0.3.1+build.7", want: "0.3.1"},
		{raw: "v1", want: "1.0.0"},
		{raw: "1.2", want: "1.2.0"},
	}
	for _, c := range cases {
		v := mustParse(t, c.raw)
		if got := v.String(); got != c.want {
			t.Fatalf("%q String = %q，期望 %q", c.raw, got, c.want)
		}
		if got := v.IsPrerelease(); got != c.pre {
			t.Fatalf("%q IsPrerelease = %v，期望 %v", c.raw, got, c.pre)
		}
	}
}

func TestComparePrecedence(t *testing.T) {
	// semver 2.0 官方优先级示例 + 项目需要的 rc/beta 场景。
	ordered := []string{
		"0.3.1-alpha",
		"0.3.1-alpha.1",
		"0.3.1-alpha.beta",
		"0.3.1-beta",
		"0.3.1-beta.2",
		"0.3.1-beta.11",
		"0.3.1-rc.1",
		"0.3.1-rc.2",
		"0.3.1",
		"0.3.2-alpha",
		"0.3.2-beta",
		"0.3.2",
	}
	for i := 1; i < len(ordered); i++ {
		prev := mustParse(t, ordered[i-1])
		next := mustParse(t, ordered[i])
		if c := Compare(prev, next); c >= 0 {
			t.Fatalf("期望 %q < %q，实际 Compare = %d", prev, next, c)
		}
		if c := Compare(next, prev); c <= 0 {
			t.Fatalf("期望 %q > %q，实际 Compare = %d", next, prev, c)
		}
	}
}

func TestCompareStableAndBuildMetadata(t *testing.T) {
	a := mustParse(t, "0.3.1")
	b := mustParse(t, "0.3.1+build.9")
	if Compare(a, b) != 0 {
		t.Fatalf("build metadata 不应参与比较：%v vs %v", a, b)
	}
	if Compare(mustParse(t, "v0.4.0"), mustParse(t, "0.3.99")) <= 0 {
		t.Fatal("0.4.0 应大于 0.3.99")
	}
}

func TestPreChannel(t *testing.T) {
	cases := map[string]string{
		"0.3.1":         "",
		"0.3.1-alpha":   "alpha",
		"0.3.1-a.2":     "alpha",
		"0.3.1-beta.1":  "beta",
		"0.3.1-b.9":     "beta",
		"0.3.1-rc.1":    "rc",
		"0.3.1-hotfix":  "beta",
		"0.3.1-preview": "beta",
	}
	for raw, want := range cases {
		if got := mustParse(t, raw).PreChannel(); got != want {
			t.Fatalf("%q PreChannel = %q，期望 %q", raw, got, want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, raw := range []string{"", "v", "1.2.3.4", "1.2.x", "1..2", "1.2.3-", "1.2.3-rc..1", "1.2.3-beta_1"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("%q 应解析失败", raw)
		}
	}
}
