# Benchmark：iSCSI 代理与读缓存

这份文档给 `internal/iscsiproxy`（v2 透传代理）和 `internal/iscsicache/cache`（L1/L2 读缓存）一组可复现的基准，用来回答两个问题：

- 未命中时，经过代理比直连慢多少？
- 命中时，数据从本地内存 / SSD 返回到底有多快？

文中的数字是**一台开发机上的参照**，不是产品承诺。真正对外的端到端结果见 README 的 [Performance](../README.zh-CN.md#性能)（CrystalDiskMark + 真实挂载盘）。

## 1. 基准矩阵

| 层 | Benchmark | 测什么 |
| --- | --- | --- |
| 缓存 | `BenchmarkReadCachedL1` | 4 KiB / 64 KiB / 1 MiB 的 L1 整段命中，缓存自身的查找与拷贝成本 |
| 缓存 | `BenchmarkReadCachedBoth` | 1 MiB 的 L2 命中并提升进 L1；假 L2 在内存里，只测索引 / 拷贝成本，不含真实 SSD 读延迟 |
| 缓存 | `BenchmarkInstall` | 64 KiB 冷块安装，即一次未命中交给回填 worker 的工作 |
| 缓存 | `BenchmarkStats` | 一次占用统计，客户端每 5 秒采样一次 |
| v2 代理 | `BenchmarkProxyReadOverhead` | 纯 loopback，没有网络时转发成本的下限 |
| v2 代理 | `BenchmarkProxyReadRemote` | 后端固定 1 ms 服务时间，网络 / SSD 形态 |
| v2 代理 | `BenchmarkProxyReadHDD` | 后端固定 10 ms 服务时间，HDD 形态 |

代理基准直接对 `Portal` 说原始 iSCSI：`direct` 绕过代理直连假后端，`proxy-passthrough` 走代理但不缓存，`proxy-hot` 先暖一个区间再从 L1 代答。因此输出里包含帧解析、序号改写、攒批和本地 Data-In 注入的开销，而不是只测一个函数调用。

## 2. 怎么跑

```bash
# 缓存路径：L1 / L2 / 安装 / 统计
go test ./internal/iscsicache/cache -run '^$' -bench . -benchmem -count=1

# v2 代理：HDD 形态
go test ./internal/iscsiproxy -run '^$' -bench 'BenchmarkProxyReadHDD' -benchmem -count=1

# v2 代理：1 ms 后端 / 纯 loopback
go test ./internal/iscsiproxy -run '^$' -bench 'BenchmarkProxyReadRemote' -benchmem -count=1
go test ./internal/iscsiproxy -run '^$' -bench 'BenchmarkProxyReadOverhead' -benchmem -count=1
```

`-run '^$'` 跳过普通测试，只跑 benchmark。比较两次改动时，建议多跑几轮再用 `benchstat`：

```bash
go install golang.org/x/perf/cmd/benchstat@latest

go test ./internal/iscsiproxy -run '^$' -bench 'BenchmarkProxyReadHDD' -count=5 > old.txt
# 改代码
go test ./internal/iscsiproxy -run '^$' -bench 'BenchmarkProxyReadHDD' -count=5 > new.txt
benchstat old.txt new.txt
```

`go test -bench` 默认只跑 1 秒一个用例，单次结果会飘几百分点；正式对比用 `-benchtime=2s -count=5` 更稳。

## 3. 参考环境

| 项 | 值 |
| --- | --- |
| 日期 | 2026-11-01 |
| 系统 | Windows 11 26200 |
| CPU | AMD Ryzen 9 9900X，12 核 / 24 线程 |
| 内存 | DDR5-6800，64 GB |
| Go | 1.27.1 |
| 后端 | 进程内假 LUN（内存），loopback |

下面全部是 `go test -bench` 的原始输出，参数为 `-benchtime=1s -count=1`。代理的“10 ms / 1 ms 后端”是给假后端加固定 `time.Sleep`，只模拟服务时间，不模拟排队和真实链路。

## 4. 缓存路径

```text
goos: windows
goarch: amd64
pkg: vault/internal/iscsicache/cache
cpu: AMD Ryzen 9 9900X 12-Core Processor
BenchmarkReadCachedL1/4KiB-24 	31011678	        37.55 ns/op	109081.40 MB/s	       0 B/op	       0 allocs/op
BenchmarkReadCachedL1/64KiB-24         	 2176653	       549.8 ns/op	119207.36 MB/s	       0 B/op	       0 allocs/op
BenchmarkReadCachedL1/1MiB-24          	   84010	     14315 ns/op	73247.66 MB/s	       0 B/op	       0 allocs/op
BenchmarkReadCachedBoth-24             	   29432	     40736 ns/op	25740.47 MB/s	   11979 B/op	      53 allocs/op
BenchmarkStats-24                      	  603427	      2051 ns/op	       0 B/op	       0 allocs/op
BenchmarkInstall-24                    	  353917	      3390 ns/op	19331.06 MB/s	   65584 B/op	       2 allocs/op
PASS
ok  	vault/internal/iscsicache/cache	25.428s
```

怎么读：

- **L1 是零分配的。** 4 KiB 命中 37.55 ns，按 4 万 IOPS 计算，一秒只花约 1.5 ms CPU（不到单核的 0.2%）；1 MiB 命中 14.3 µs，相对 10 ms 的 HDD 读约 0.14%。
- **L2 命中要提升到 L1。** 1 MiB 约 40.7 µs、53 次分配；这是缓存索引和拷贝的成本，真实 SSD 的读延迟要另外加。即使加上百微秒级 SSD 延迟，仍比 10 ms 的 HDD 读快一个数量级以上。
- **安装是异步的。** 未命中读不等待 3.39 µs 的冷块安装，回填在 worker 里完成；`Stats()` 约 2.05 µs，5 秒采样一次可以忽略。

## 5. v2 代理：HDD 形态

假后端每次 READ 固定 10 ms，代表一块随机读约 10 ms 的 HDD。

```text
goos: windows
goarch: amd64
pkg: vault/internal/iscsiproxy
cpu: AMD Ryzen 9 9900X 12-Core Processor
BenchmarkProxyReadHDD/direct/4KiB-24         	     100	  10485547 ns/op	   0.39 MB/s	         1.000 backend-reads/op	    4525 B/op	      11 allocs/op
BenchmarkProxyReadHDD/proxy-passthrough/4KiB-24         	     100	  10480889 ns/op	   0.39 MB/s	         1.000 backend-reads/op	    7232 B/op	      11 allocs/op
BenchmarkProxyReadHDD/proxy-hot/4KiB-24                 	   49376	     24293 ns/op	 168.61 MB/s	         0 backend-reads/op	     486 B/op	       8 allocs/op
BenchmarkProxyReadHDD/direct/64KiB-24                   	     100	  10425120 ns/op	   6.29 MB/s	         1.000 backend-reads/op	   66110 B/op	      11 allocs/op
BenchmarkProxyReadHDD/proxy-passthrough/64KiB-24        	     100	  10534212 ns/op	   6.22 MB/s	         1.000 backend-reads/op	   69631 B/op	      14 allocs/op
BenchmarkProxyReadHDD/proxy-hot/64KiB-24                	   30282	     38465 ns/op	1703.78 MB/s	         0 backend-reads/op	     767 B/op	      13 allocs/op
BenchmarkProxyReadHDD/direct/1MiB-24                    	     100	  10788780 ns/op	  97.19 MB/s	         1.000 backend-reads/op	 1051425 B/op	      15 allocs/op
BenchmarkProxyReadHDD/proxy-passthrough/1MiB-24         	      87	  11939868 ns/op	  87.82 MB/s	         1.000 backend-reads/op	 1066138 B/op	      17 allocs/op
BenchmarkProxyReadHDD/proxy-hot/1MiB-24                 	    3812	    339005 ns/op	3093.10 MB/s	         0 backend-reads/op	   19687 B/op	     133 allocs/op
PASS
ok  	vault/internal/iscsiproxy	11.909s
```

- 4 KiB / 64 KiB 的透传开销在测量噪声内，和直连基本一样；1 MiB 未命中约 11.94 ms，直连 10.79 ms（约 90%），多出来的是约 1.1 ms 的固定本地拷贝。
- `proxy-hot` 的 `backend-reads/op` 为 **0**，基准会在计时前先验证一次热读没有回头端，避免把未命中当成命中。
- `proxy-hot` 的 4 KiB 24 µs 看起来比缓存的 37 ns 大很多，因为前者包含客户端到代理、代理回客户端两次 loopback TCP；**缓存本身的查找只有几十纳秒**。真实收益体现在“不走后端网络和 HDD”，而不是这 24 µs。

## 6. v2 代理：1 ms 后端

把后端服务时间降到 1 ms，模拟更快的后端或网络。此时 1 MiB 的固定拷贝开始显眼：

```text
goos: windows
goarch: amd64
pkg: vault/internal/iscsiproxy
cpu: AMD Ryzen 9 9900X 12-Core Processor
BenchmarkProxyReadRemote/direct/4KiB-24         	     790	   1512559 ns/op	   2.71 MB/s	         1.000 backend-reads/op	    4484 B/op	      11 allocs/op
BenchmarkProxyReadRemote/proxy-passthrough/4KiB-24         	     787	   1512160 ns/op	   2.71 MB/s	         1.000 backend-reads/op	    4826 B/op	      11 allocs/op
BenchmarkProxyReadRemote/proxy-hot/4KiB-24                 	   47930	     24291 ns/op	 168.62 MB/s	         0 backend-reads/op	     489 B/op	       8 allocs/op
BenchmarkProxyReadRemote/direct/64KiB-24                   	     805	   1510462 ns/op	  43.39 MB/s	         1.000 backend-reads/op	   66070 B/op	      11 allocs/op
BenchmarkProxyReadRemote/proxy-passthrough/64KiB-24        	     795	   1519092 ns/op	  43.14 MB/s	         1.000 backend-reads/op	   66681 B/op	      13 allocs/op
BenchmarkProxyReadRemote/proxy-hot/64KiB-24                	   30187	     39658 ns/op	1652.54 MB/s	         0 backend-reads/op	     812 B/op	      13 allocs/op
BenchmarkProxyReadRemote/direct/1MiB-24                    	     772	   1549248 ns/op	 676.83 MB/s	         1.000 backend-reads/op	 1051365 B/op	      14 allocs/op
BenchmarkProxyReadRemote/proxy-passthrough/1MiB-24         	     444	   2989659 ns/op	 350.73 MB/s	         1.000 backend-reads/op	 1053929 B/op	      16 allocs/op
BenchmarkProxyReadRemote/proxy-hot/1MiB-24                 	    3494	    349620 ns/op	2999.19 MB/s	         0 backend-reads/op	   13386 B/op	     133 allocs/op
PASS
ok  	vault/internal/iscsiproxy	14.065s
```

这说明代理的未命中成本是**近似固定的本地拷贝**：后端越慢，它在总耗时里占比越小；后端越快，1 MiB 大块读越容易看到它。4 KiB / 64 KiB 无论后端多快都基本持平。

## 7. 纯 loopback 下限

没有网络、后端在内存里时，代理多出来的一跳就是全部成本：

```text
goos: windows
goarch: amd64
pkg: vault/internal/iscsiproxy
cpu: AMD Ryzen 9 9900X 12-Core Processor
BenchmarkProxyReadOverhead/direct/4KiB-24         	   49934	     24314 ns/op	 168.47 MB/s	         1.000 backend-reads/op	    4476 B/op	      11 allocs/op
BenchmarkProxyReadOverhead/proxy-passthrough/4KiB-24         	   24564	     48550 ns/op	  84.37 MB/s	         1.000 backend-reads/op	    4488 B/op	      11 allocs/op
BenchmarkProxyReadOverhead/proxy-hot/4KiB-24                 	   49692	     24718 ns/op	 165.71 MB/s	         0 backend-reads/op	     488 B/op	       8 allocs/op
BenchmarkProxyReadOverhead/direct/64KiB-24                   	   31082	     38804 ns/op	1688.90 MB/s	         1.000 backend-reads/op	   66052 B/op	      11 allocs/op
BenchmarkProxyReadOverhead/proxy-passthrough/64KiB-24        	   15729	     74913 ns/op	 874.83 MB/s	         1.000 backend-reads/op	   66255 B/op	      13 allocs/op
BenchmarkProxyReadOverhead/proxy-hot/64KiB-24                	   31255	     38438 ns/op	1704.98 MB/s	         0 backend-reads/op	     757 B/op	      13 allocs/op
BenchmarkProxyReadOverhead/direct/1MiB-24                    	    4677	    399537 ns/op	2624.48 MB/s	         1.000 backend-reads/op	 1051252 B/op	      13 allocs/op
BenchmarkProxyReadOverhead/proxy-passthrough/1MiB-24         	    1143	   1217737 ns/op	 861.09 MB/s	         1.000 backend-reads/op	 1051956 B/op	      14 allocs/op
BenchmarkProxyReadOverhead/proxy-hot/1MiB-24                 	    3681	    349713 ns/op	2998.39 MB/s	         0 backend-reads/op	   18624 B/op	     133 allocs/op
PASS
ok  	vault/internal/iscsiproxy	14.888s
```

这组数字只适合看**回归**：本机 loopback 的一次往返本来就是几十微秒，代理多一跳就会翻倍，不能拿它推断真实挂载。真实 HDD 的 10 ms 服务时间会把这一跳淹没，见上一节。

## 8. 回归口径

改代理或缓存后，先看这几项，而不是只看一个总吞吐：

| 指标 | 期望 |
| --- | --- |
| `direct` / `proxy-passthrough` 的 `backend-reads/op` | 1.0，不能变成每个缓存块一次后端命令 |
| `proxy-hot` 的 `backend-reads/op` | 0 |
| L1 命中 `allocs/op` | 0 |
| L1 4 KiB `ns/op` | 不应明显高于几十纳秒 |
| HDD 形态下 4 KiB / 64 KiB 代理 / 直连 | 接近 100% |
| HDD 形态下 1 MiB 代理 / 直连 | 不应继续下降；目前约 90% |

单次 `ns/op` 波动 5–10% 很常见；用 `benchstat` 看多轮结果，超过 10% 再判断是否回归。跑之前关掉后台任务，尽量固定电源计划。

## 9. 边界

- 假后端只实现 INQUIRY、READ CAPACITY 和 READ，不覆盖登录、CHAP、digest、多连接、ERL>0、R2T。这些路径由 `internal/iscsiproxy` 的 live 测试覆盖（需要设置 `VAULT_ISCSI_TEST_ADDR` / `VAULT_ISCSI_TEST_IQN`）。
- 代理基准是单连接、串行发命令，测的是每命令成本和命中路径，不是队列深度下的聚合吞吐。真实 Windows 挂载会保持几十条命令在飞，最终吞吐仍以挂载盘上的 CrystalDiskMark 为准。
- 缓存基准使用出厂几何（64 KiB 块、4 KiB 扇区、256 分片），和产品默认值一致。
- 这些 benchmark 不替代真机验收：冷读、热读、顺序扫描准入、L2 重启恢复都应在真实挂载盘上再跑一遍。
