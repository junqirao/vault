package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// 配额分摊与推荐值都是纯计算，但直接决定"每块盘能缓存多少"，边界（未知容量、封顶、
// 下限、预算不够）任一处理错都会让某个库静默拿到 0 或超发，所以逐个锁住。

func TestSplitCacheBudgetProportionalToRepoSize(t *testing.T) {
	const gib = int64(1) << 30
	repos := []cacheRepoSize{
		{ID: "big", SizeBytes: 1024 * gib}, // 1 TiB
		{ID: "small", SizeBytes: 100 * gib},
	}
	total := 8 * gib
	got := splitCacheBudget(total, cacheMinL1Bytes, repos)

	if sum := got["big"] + got["small"]; sum > total {
		t.Fatalf("分摊总额 %d 超过总预算 %d", sum, total)
	}
	if got["big"] < cacheMinL1Bytes || got["small"] < cacheMinL1Bytes {
		t.Fatalf("配额低于下限：%v", got)
	}
	// 容量比 10:1，扣掉两边相同的保底后份额比应接近 10:1。
	ratio := float64(got["big"]) / float64(got["small"])
	if ratio < 8 || ratio > 12 {
		t.Fatalf("大库/小库 配额比 %.2f，期望接近 10", ratio)
	}
	for id, q := range got {
		if q%cacheQuotaGranularity != 0 {
			t.Fatalf("%s 的配额 %d 未对齐到 MiB", id, q)
		}
	}
}

func TestSplitCacheBudgetCapsAtRepoSize(t *testing.T) {
	const gib = int64(1) << 30
	// 库比预算小得多：缓存比数据还大没有意义，配额封顶在库容量。
	got := splitCacheBudget(8*gib, cacheMinL1Bytes, []cacheRepoSize{{ID: "one", SizeBytes: gib}})
	if got["one"] != gib {
		t.Fatalf("单库配额 %d，期望封顶在库容量 %d", got["one"], gib)
	}

	// 封顶后剩余部分转给没封顶的库（水填法），而不是白白浪费：
	// 已知库被封在自身容量（1 GiB），剩下 7 GiB 全部落到没有容量上限的那个库。
	got = splitCacheBudget(8*gib, cacheMinL1Bytes, []cacheRepoSize{
		{ID: "known", SizeBytes: 1 * gib},
		{ID: "unknown"},
	})
	if got["known"] != gib {
		t.Fatalf("已知库配额 %d，期望封顶在库容量 %d", got["known"], gib)
	}
	if got["unknown"] != 7*gib {
		t.Fatalf("未知容量的库应吸收剩余预算（%d），实际 %d", 7*gib, got["unknown"])
	}
}

func TestSplitCacheBudgetUnknownSizeFallsBackToAverage(t *testing.T) {
	const gib = int64(1) << 30
	total := int64(8) * gib

	// 一个已知（100 GiB）、一个未知：未知的按已知库的平均容量看待 → 两者份额接近。
	got := splitCacheBudget(total, cacheMinL1Bytes, []cacheRepoSize{
		{ID: "known", SizeBytes: 100 * gib},
		{ID: "unknown"},
	})
	diff := got["known"] - got["unknown"]
	if diff < 0 {
		diff = -diff
	}
	if diff > 8<<20 { // 允许 MiB 取整带来的零头
		t.Fatalf("已知/未知 配额差 %d，期望近似均分：%v", diff, got)
	}

	// 一个都不知：权重相同，退化为按库数均分（这条规则生效前的行为）。
	got = splitCacheBudget(total, cacheMinL1Bytes, []cacheRepoSize{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	if got["a"] != got["b"] || got["b"] != got["c"] {
		t.Fatalf("容量全未知时期望均分：%v", got)
	}
	if got["a"] != (total/3)/cacheQuotaGranularity*cacheQuotaGranularity {
		t.Fatalf("均分结果 %d 与 total/3 不符", got["a"])
	}
}

func TestSplitCacheBudgetBudgetBelowFloors(t *testing.T) {
	const mib = int64(1) << 20
	// 总预算连每个库的下限都凑不出来：不硬撑保底（那会超过"总上限"），按权重分，
	// 是否降级由调用方（EnsureTarget 的 L1 下限校验）决定。
	got := splitCacheBudget(100*mib, 64*mib, []cacheRepoSize{{ID: "a"}, {ID: "b"}})
	if got["a"] != 50*mib || got["b"] != 50*mib {
		t.Fatalf("期望各 50 MiB，实际 %v", got)
	}
	if got["a"]+got["b"] > 100*mib {
		t.Fatalf("超过了总预算：%v", got)
	}
}

func TestSplitCacheBudgetZeroBudget(t *testing.T) {
	got := splitCacheBudget(0, cacheMinL1Bytes, []cacheRepoSize{{ID: "a", SizeBytes: 1 << 40}})
	if len(got) != 1 || got["a"] != 0 {
		t.Fatalf("预算为 0 时应全部为 0，实际 %v", got)
	}
}

func TestCacheRecommend(t *testing.T) {
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	cases := []struct {
		name  string
		total int64
		repos int
		l1    int64
		l2    int64
	}{
		{"没有任何库", 0, 0, 0, 0},
		{"一个库都没有容量", 0, 3, 0, 0},
		{"100 GiB：L2 取 5%，L1 取总量的 5%/8", 100 * gib, 1, 640 * mib, 5 * gib},
		// L2 是每库上限 → 按平均库容量（512 GiB）的 5% = 26214 MiB；
		// L1 是全局共享 → 1 TiB 的 5%/8 = 6.4 GiB，被 2 GiB 上限截住。
		{"1 TiB / 2 库：L2 按平均容量、L1 被 2 GiB 截住", 1 << 40, 2, 2 * gib, 26214 * mib},
		{"1 GiB：两级都被下限抬起来", gib, 1, cacheMinL1Bytes, cacheMinL2Bytes},
	}
	for _, tc := range cases {
		got := cacheRecommend(tc.total, tc.repos)
		if got.L1Bytes != tc.l1 || got.L2Bytes != tc.l2 {
			t.Errorf("%s: 推荐 L1=%d L2=%d，期望 L1=%d L2=%d",
				tc.name, got.L1Bytes, got.L2Bytes, tc.l1, tc.l2)
		}
		if got.BasisBytes != tc.total || got.BasisRepos != tc.repos {
			t.Errorf("%s: 依据 %d 库 / %d 字节，期望 %d 库 / %d 字节",
				tc.name, got.BasisRepos, got.BasisBytes, tc.repos, tc.total)
		}
	}
}

func TestQuotaForSplitsAcrossEnabledCacheRepos(t *testing.T) {
	const gib = int64(1) << 30
	cfg := &ConfigStore{cfg: Config{
		CacheL1Bytes: 8 * gib,
		CacheL2Bytes: 80 * gib,
		RepoMounts: map[string]RepoMountPref{
			"big":   {CacheEnabled: true},
			"small": {CacheEnabled: true},
			// 没开缓存的库既不参与分摊，也不该影响别人的份额。
			// cacheEnabledSet=true 表示这是显式关闭；未表态的条目按默认开启。
			"off": {cacheEnabledSet: true},
		},
	}}
	m := newCacheManager(cfg, nil)
	m.rememberRepoSize("small", 100*gib)
	m.rememberRepoSize("off", 500*gib)

	// big 这次没带容量（走索引）→ 索引里也没有 → 按已知库的平均容量（100 GiB）看待。
	l1BigUnknown, _ := m.quotaFor(cacheTargetRequest{AllocationID: "a1", RepoID: "big"})
	l1Small, _ := m.quotaFor(cacheTargetRequest{AllocationID: "a2", RepoID: "small"})
	if diff := l1BigUnknown - l1Small; diff > 8<<20 || diff < -(8<<20) {
		t.Fatalf("容量未知时两者应接近均分：big=%d small=%d", l1BigUnknown, l1Small)
	}

	// 挂载参数带了容量：以它为准，同时记进索引供后续（以及推荐值）使用。
	_, _ = m.quotaFor(cacheTargetRequest{
		AllocationID: "a1", RepoID: "big", RepoSizeBytes: 1024 * gib,
	})
	if m.knownRepoSize("big") != 1024*gib {
		t.Fatalf("挂载参数里的容量没有被索引，实际 %d", m.knownRepoSize("big"))
	}

	// 两个库的容量都已知之后重算：1 TiB 的库该比 100 GiB 的分到多得多的份额。
	l1Big, l2Big := m.quotaFor(cacheTargetRequest{AllocationID: "a1", RepoID: "big"})
	l1Small, _ = m.quotaFor(cacheTargetRequest{AllocationID: "a2", RepoID: "small"})
	if l1Big <= l1Small*8 {
		t.Fatalf("1 TiB 的库应比 100 GiB 的库分到多得多的 L1：big=%d small=%d", l1Big, l1Small)
	}
	// L2 是每库上限：与库数、库容量占比都无关，单库最多就是配置值（80 GiB）。
	if l2Big != 80*gib {
		t.Fatalf("L2 每库上限应为 80 GiB，实际 %d", l2Big)
	}
	if got := l1Big + l1Small; got > cfg.cfg.CacheL1Bytes {
		t.Fatalf("L1 两个库合计 %d 超过总预算 %d", got, cfg.cfg.CacheL1Bytes)
	}

	// 幂等：同一库重复挂载拿到同一份配额（配额不能随挂载次数漂移）。
	if again, _ := m.quotaFor(cacheTargetRequest{AllocationID: "a1", RepoID: "big"}); again != l1Big {
		t.Fatalf("同一库重复计算得到不同配额：%d vs %d", again, l1Big)
	}

	// 分摊集合里的库换了顺序（map 迭代）也必须是同一个结果。
	for i := 0; i < 20; i++ {
		if got, _ := m.quotaFor(cacheTargetRequest{AllocationID: "a1", RepoID: "big"}); got != l1Big {
			t.Fatalf("第 %d 次计算得到 %d，期望 %d", i, got, l1Big)
		}
	}
}

// TestCachePoolIncludesActiveAndDefaultRepos 锁定默认开启后的分摊集合：
// 未表态/未保存过偏好的库与当前已有缓存目标的库都必须参与，显式关闭的库不参与。
func TestCachePoolIncludesActiveAndDefaultRepos(t *testing.T) {
	cfg := Config{RepoMounts: map[string]RepoMountPref{
		"explicit-on":  {CacheEnabled: true, cacheEnabledSet: true},
		"explicit-off": {CacheEnabled: false, cacheEnabledSet: true},
		"old-config":   {}, // 旧配置没写 cache_enabled → 默认开启
	}}
	got := cachePoolRepos(cfg, "mounting", []string{"active", "old-config"})
	want := []string{"active", "explicit-on", "mounting", "old-config"}
	if len(got) != len(want) {
		t.Fatalf("分摊集合 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("分摊集合 = %v，期望 %v", got, want)
		}
	}
}

func TestQuotaForDisablesL2WhenBudgetZero(t *testing.T) {
	cfg := &ConfigStore{cfg: Config{
		CacheL1Bytes: 2 << 30,
		CacheL2Bytes: 0, // 关闭 L2：只做内存缓存
		RepoMounts:   map[string]RepoMountPref{"r": {CacheEnabled: true}},
	}}
	m := newCacheManager(cfg, nil)
	l1, l2 := m.quotaFor(cacheTargetRequest{AllocationID: "a", RepoID: "r", RepoSizeBytes: 100 << 30})
	if l1 <= 0 || l2 != 0 {
		t.Fatalf("L2 预算为 0 时不该分到配额：l1=%d l2=%d", l1, l2)
	}
}

func TestStatusReportsRecommendation(t *testing.T) {
	const gib = int64(1) << 30
	cfg := &ConfigStore{cfg: Config{
		CacheL1Bytes: 8 * gib,
		CacheL2Bytes: 80 * gib,
		RepoMounts:   map[string]RepoMountPref{"r": {CacheEnabled: true}},
	}}
	m := newCacheManager(cfg, nil)
	if st := m.Status(); st.KnownRepos != 0 || st.Recommend.L1Bytes != 0 {
		t.Fatalf("还没挂过库时不该给出推荐：%+v", st.Recommend)
	}
	m.rememberRepoSize("r", 100*gib)
	st := m.Status()
	if st.KnownRepos != 1 || st.KnownRepoBytes != 100*gib {
		t.Fatalf("已知库容量统计不对：%d 库 / %d 字节", st.KnownRepos, st.KnownRepoBytes)
	}
	if st.Recommend.L2Bytes != 5*gib {
		t.Fatalf("推荐 L2 %d，期望 5 GiB", st.Recommend.L2Bytes)
	}
	if st.EnabledRepos != 1 {
		t.Fatalf("已启用缓存的库数 %d，期望 1", st.EnabledRepos)
	}
}

// TestQuotaLedgerKeepsTotalWithinBudget 锁定 L1 的"全局共享"口径：集合变化（新库加入）
// 之后，新库只能分到账本上**还剩**的 L1，不能在老库已占的份额之上再叠一份。
func TestQuotaLedgerKeepsTotalWithinBudget(t *testing.T) {
	const gib = int64(1) << 30
	store := newCacheTestStore(t)
	l1, l2 := 4*gib, 40*gib
	if _, err := store.Patch(ConfigPatch{CacheL1Bytes: &l1, CacheL2Bytes: &l2}); err != nil {
		t.Fatalf("Patch 缓存上限失败：%v", err)
	}
	m := newCacheManager(store, testLogger())
	enable := func(id string) {
		t.Helper()
		if _, err := store.SetRepoMountPref(id, RepoMountPref{CacheEnabled: true}); err != nil {
			t.Fatalf("启用 %s 的缓存失败：%v", id, err)
		}
	}

	// 集合里只有 A：L1 拿走全部全局预算，L2 拿满每库上限。
	enable("a")
	aL1, aL2 := m.quotaFor(cacheTargetRequest{AllocationID: "alloc-a", RepoID: "a", RepoSizeBytes: 1000 * gib})
	if aL1 != l1 || aL2 != l2 {
		t.Fatalf("只有 A 时 L1 应为全局预算 %d、L2 为每库上限 %d，实际 %d/%d", l1, l2, aL1, aL2)
	}
	m.markCacheTarget("alloc-a", "a")
	m.commitQuota("a", aL1, aL2)

	// B 加入后集合是 {a,b}：B 的 L1 理论份额是 2 GiB，但账本已被 A 占满 → 0（降级为直连）。
	// L2 是每库上限，不受账本影响，B 依然拿满 40 GiB。
	enable("b")
	bL1, bL2 := m.quotaFor(cacheTargetRequest{AllocationID: "alloc-b", RepoID: "b", RepoSizeBytes: 1000 * gib})
	if bL1 != 0 {
		t.Fatalf("L1 预算已被 A 占满时 B 不该再分到 %d", bL1)
	}
	if bL2 != l2 {
		t.Fatalf("L2 是每库上限，B 应分到 %d，实际 %d", l2, bL2)
	}

	// A 注销后账本归还，B 才能拿到自己按容量占比应得的 L1 份额。
	m.releaseQuota("alloc-a")
	m.unmarkCacheTarget("alloc-a")
	bL1, _ = m.quotaFor(cacheTargetRequest{AllocationID: "alloc-b", RepoID: "b", RepoSizeBytes: 1000 * gib})
	if bL1 != l1/2 {
		t.Fatalf("A 注销后 B 应分到 %d，实际 %d", l1/2, bL1)
	}
}

// TestCollectCacheFilesRemovesOrphans 锁定无主文件回收：卸载/删除库之后，既不在门户里、
// 也没有挂载记录的 l2-/l1- 文件应被删除；有主文件与无关文件不动。
func TestCollectCacheFilesRemovesOrphans(t *testing.T) {
	const mib = int64(1) << 20
	dir := t.TempDir()
	store := newCacheTestStore(t)
	budget := int64(cacheMinL2Bytes)
	if _, err := store.Patch(ConfigPatch{CacheL2Bytes: &budget, CacheL2Dir: &dir}); err != nil {
		t.Fatalf("Patch 缓存目录失败：%v", err)
	}
	m := newCacheManager(store, testLogger())
	// 状态里还有挂载记录的分配（应用重启后、自动挂载完成之前就是这种状态）。
	m.liveAllocations = func() map[string]struct{} {
		return map[string]struct{}{"alloc-live": {}}
	}

	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, mib), 0o600); err != nil {
			t.Fatalf("造缓存文件失败：%v", err)
		}
	}
	write("l2-alloc-live.bin")
	write("l1-alloc-live.bin")
	write("l2-alloc-gone.bin")
	write("l1-alloc-gone.bin")
	write("notes.txt")

	m.collectCacheFiles()

	for _, name := range []string{"l2-alloc-live.bin", "l1-alloc-live.bin"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 还有挂载记录，不该被删：%v", name, err)
		}
	}
	for _, name := range []string{"l2-alloc-gone.bin", "l1-alloc-gone.bin"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s 已无主，应被回收", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("非缓存文件不该被删：%v", err)
	}
}
