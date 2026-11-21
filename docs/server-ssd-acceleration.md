# 服务端 SSD「纯加速」方案调研

> 状态：调研 / 待评审
> 日期：2026-11-21
> 范围：只讨论服务端（Linux LIO / Windows iSCSI Target Server）如何使用 SSD 加速，不改变客户端 iSCSI 代理与缓存语义
> 前置阅读：`README.zh-CN.md` 性能一节、`docs/design.md`、`internal/platform/{linuxlvm,liotarget,winvhd,winbackend}`
> 行号对应 2026-11-21 工作区；后续代码改动后需重新核对

---

## 0. 一句话结论

**能，但先要承认一个前提：SSD 不承载数据、也不做缓存，它不会加速任何仍在 HDD 上的读。**

所以「纯加速」只有两条路：

1. **放置型**：把热库整体放到 SSD 池/卷（母盘 + 差异盘），SSD 上是唯一权威副本，不做淘汰、不做回写。**SSD 预算足够覆盖热库时推荐。**
2. **路径型**：数据仍在原处，只调 LIO / 内核 / Windows / NTFS / iSCSI / 网络参数。收益通常 5%~30%，不会把 HDD 变成 SSD。

除此之外，任何「HDD 存数据 + SSD 加速」的组合（dm-cache、bcache、Storage Spaces 分层、客户端 L2）**本质上都是缓存/分层**：有第二份数据、有淘汰、可能有回写。

**如果 SSD 预算有限（小 SSD + 大 HDD），必须接受一个取舍：**

- **静态放置**：SSD 只装得下多少热库，就只有这些库快；其余仍是 HDD。没有缓存语义，行为可预测。
- **动态缓存 / 分层**：同一块小 SSD 服务整个 HDD 池，所有数据都有机会变快，但命中率依赖访问倾斜，且它是缓存。
- **没有第三条路**：SSD 不装数据、不做缓存，就不会加速 HDD 上的数据。

对「避免买太多 SSD」这个目标，最经济的顺序通常是：
**先扩客户端 L2（用客户端已有的 SSD，零服务端成本）→ 再在「小 SSD dm-cache」和「静态放置 top-N」之间按热集是否稳定来选 → 最后才是全 SSD 库。**

dm-cache 本仓库已经实现（池级、默认 writethrough），它不是客户端 L2，而是服务端块级缓存；在小 SSD + 大 HDD 的预算下，它是把少量 SSD 放大到整个 HDD 池最直接的工具。

最值得投的三个点：

- **共享母盘的冷读**：一份 SSD 母盘 / 一份 SSD 热块缓存服务所有客户端，避免 N 台客户端各存一份 L2 副本。
- **所有写路径**：客户端缓存对写完全无效（写始终透传到服务端），只有服务端存储能加速写。
- **多客户端并发**：HDD 的 4K 随机 IOPS 是全局共享的，几个人同时冷启动就会把 HDD 打满；SSD 消除寻道争用。

预期边界：

- 4K 随机读写 IOPS 可数量级提升（HDD 约 350 IOPS，SSD 数万到数十万）。
- 顺序吞吐受网络上限约束（1GbE≈118 MB/s、2.5GbE≈295 MB/s、10GbE≈1.18 GB/s），SSD 再快也超不过链路。
- 客户端 L1 仍比服务端 SSD 快（内存 vs 网络）；整库 SSD 可以关 L2，dm-cache 库建议保留 L2 作为第二层。

---

## 1. 「纯加速」的判定标准

| 类别 | 是否复制数据 | 是否有淘汰 | 是否回写 | 容量代价 | 一致性风险 | 是否算纯加速 |
|---|---|---|---|---|---|---|
| 放置型（SSD 作为权威存储） | 否（唯一副本） | 否 | 否 | SSD 容量 | 无额外风险 | ✅ |
| 路径 / 参数型 | 否 | 否 | 否 | 无 | 低 | ✅ |
| 只读副本（immutable parent replica） | 是（只读） | 否 | 否 | 副本容量 | 版本同步 | ⚠️ 边界 |
| dm-cache / bcache / LVM cache | 是 | 是 | writethrough / writeback | 缓存容量 | 回写丢失、不一致 | ❌ |
| Storage Spaces tiering / 自动分层 | 是（迁移） | 是 | 可能 | SSD 容量 | 迁移 / 回写 | ❌（按缓存对待） |
| 客户端 L1 / L2 | 是 | 是 | 否 | 客户端内存 / SSD | 失效 | ❌（已有） |

判定口径：**只要同一份逻辑数据存在第二份、且需要决定「谁新谁旧 / 何时淘汰」，就是缓存。**「SSD 上是唯一副本、HDD 上没有」才是放置。

---

## 2. 现状（代码事实）

### 2.1 数据路径

```text
客户端 iSCSI initiator
  → Vault-Agent iscsiproxy（L1 RAM / L2 本地 SSD，读缓存；写透传）
  → 服务端
      Linux   : LIO iblock → /dev/mapper/<vg>-<lv>（thin LV）→ thin pool → PV(HDD/SSD)
      Windows : Windows iSCSI Target Server → VHDX（dynamic / differencing）→ NTFS 卷
```

### 2.2 Linux 服务端

- 磁盘是 **LVM thin LV**：`lvcreate --type thin -V <size>B -T <vg>/<pool>`，不预分配物理空间
  （`internal/platform/linuxlvm/thin.go:203-239`）。
- 差异盘是 **thin snapshot**，必须与母盘同 VG
  （`thin.go:266-278`，代码明确拒绝跨 VG）。
- LIO 使用 **iblock**，`dev=/dev/mapper/<vg>-<lv>`
  （`internal/platform/liotarget/configfs.go:64-68`、`runner.go:523-529`）。
- LIO 已设置 `emulate_tpu=1`、`emulate_tpws=1`、`is_nonrot=1`；**未设置**
  `hw_block_size`、`hw_max_sectors`、`emulate_write_cache`、`emulate_fua_write` 等
  （`target.go:538-573`）。
- 一个 VG 只允许一个 thin pool
  （`internal/platform/linuxlvm/poolcatalog.go:15-25`）。
- 存储通过 `storage_volumes.pool_ref` 绑定池；建库只选 `storage_id`，不选池；
  不指定时按「剩余空间最大的存储」自动选
  （`internal/app/repo.go:99-108`、`:2214-2240`）。
- 已有整库迁移：`POST /v1/repos/{id}/move`，要求无活跃租约、所有盘未挂载，
  复制完成后切路径（`internal/app/repomove.go:71-...`）。
- 已有 **dm-cache**：池级、默认 `writethrough`、可选 `writeback`，只能建池时挂
  （`internal/platform/linuxlvm/cache.go`、`pool.go:254-274`）。

### 2.3 Windows 服务端

- 母盘是 **dynamic VHDX**（2 MiB block），差异盘是 **differencing VHDX**；
  `CreateFixed` 已实现但没有任何调用方
  （`internal/platform/winvhd/manager.go:26-33`、`:79-108`；`winbackend/backend.go:102-108`）。
- 未设置 VHDX 的 logical / physical sector size
  （`manager.go:125-131`，底层结构体支持这两个字段）。
- iSCSI Target Server 通过 `Import-IscsiVirtualDisk` + target + mapping 发布；
  没有缓存、队列深度、扇区大小等性能参数
  （`internal/platform/winps/scripts/iscsi_set_target.ps1`）。
- 没有池概念，放置只能选「存储目录所在卷」；建库同样只选 `storage_id`。

### 2.4 现有缓存

- 客户端 L1/L2 是**读缓存**；写始终透传（README「写路径与一个坑」）。
- dm-cache 是服务端唯一的块级缓存，**池级**，不是按库；默认 writethrough，写仍落 HDD。
- 服务端没有读缓存、readahead、`O_DIRECT` / AIO / `io_uring`、队列调优、per-LUN 延迟指标。

### 2.5 现有放置能力的缺口

| 能力 | 现状 | 影响 |
|---|---|---|
| 按池放置 | 存储绑定 `pool_ref`，建库选 `storage_id` | 可以做到「整个存储都在 SSD」 |
| 按库放置 | 建库没有 `pool_ref`，只能选存储 | 需要先建 SSD 存储，再建库 / 搬库 |
| 按盘放置 | 同一 thin pool 内所有 LV 共享 PV | 不能只把母盘放 SSD（Linux 同池约束） |
| 自动放置 | 不指定存储时选「剩余空间最大」 | 可能落到 HDD，需要显式指定 |
| 跨池迁移 | 整库复制，非 `pvmove` / `dm-clone` | 需要停机窗口和双份空间 |

---

## 3. 方案矩阵

### 3.1 放置型（推荐）

| 编号 | 方案 | 做什么 | 加速对象 | 预期 | 成本 / 风险 | 平台 | 工作量 |
|---|---|---|---|---|---|---|---|
| **A1** | **整库上 SSD** | 建独立 SSD VG/thin pool → SSD 存储 → 新库指定 `storage_id`；旧库 `move` | 冷读、写、快照、并发 | 4K IOPS 数量级；顺序受网络 | SSD 容量；迁移需停机、无租约 | Linux / Windows | 低（现有 API） |
| A2 | 新建库默认 SSD，冷库留 HDD | 放置策略 / 默认存储 | 热库 | 同上，按命中率折算 | 需要策略与容量管理 | 两平台 | 低-中 |
| A3 | 仅元数据上 SSD | Linux：thin pool `_tmeta` 放 SSD，数据在 HDD | 快照 / 分配 / 元数据提交 | 小幅，需实测 | 混布 VG 复杂；收益不确定 | Linux | 中 |
| A4 | 母盘 SSD + 差异盘 HDD | 只读母盘放 SSD | 母盘冷读 | 理论好 | Linux thin snapshot 不允许跨池；Windows 可跨卷但破坏跨平台一致性 | Windows 实验 | 高 |
| A5 | 裸块直通 | NVMe 分区直接给 LIO，不走 LVM thin | 去掉 thin 映射 | 小幅 | 失去快照 / 共享 / 迁移；仅独享 | Linux | 高 |

> **A4 的关键限制**：Linux 差异盘是同一 thin pool 的 snapshot，不能把母盘和差异盘拆到两个池
> （`thin.go:266-278`）。因此「母盘 SSD、差异盘 HDD」在 Linux 上不是配置问题，
> 而是要换差异盘实现（全量复制或 dm-snapshot 跨设备），不建议本期做。
>
> **A5** 只适合「独享库 + 固定大小」的特殊场景；共享库的母盘/差异盘模型依赖 thin snapshot，
> 不能用裸块。

### 3.2 路径 / 参数型（数据不动）

| 编号 | 方案 | 做什么 | 预期 | 风险 | 平台 |
|---|---|---|---|---|---|
| B1 | LIO 4K / 大 I/O | `hw_block_size=4096`（确认客户端/FS 支持）、`hw_max_sectors` / `max_sectors_kb`、队列深度 | 减少拆分、提升顺序/随机 | 4Kn 兼容性；需实测 | Linux |
| B2 | LIO 写缓存策略 | `emulate_write_cache` / `emulate_fua_write`，按 SSD 是否有掉电保护设置 | 降低写延迟 | 掉电丢数据；必须确认 PLP | Linux |
| B3 | 调度 / 预读 / TRIM | `rotational=0`、`scheduler=none/mq-deadline`、`read_ahead_kb`、`fstrim` / `discard` | 降低尾延迟、保持 SSD 性能 | 参数因设备而异 | Linux |
| B4 | `is_nonrot` 按层设置 | 现在硬编码 1；SSD 库=1，HDD 库=0 | 避免 HDD 被当 SSD 关闭预读/整理 | 改语义需评估 thin 回收 | Linux |
| B5 | Windows VHDX / NTFS | 母盘 fixed VHDX、4K sector、NTFS 64K 分配单元、关闭 last access/8.3、电源高性能、写缓存、TRIM、杀软排除 VHDX | 降低动态分配 / 元数据开销 | 固定盘占空间；扇区/簇改不了要重建 | Windows |
| B6 | 网络 | 2.5G/10G、巨帧、MPIO、RSS | 提高上限 | 交换机 / 网卡成本 | 两平台 |
| B7 | 服务端 RAM | 增加内存 / 页缓存；Windows 目标走 CachedIO | 热数据免费加速 | 容量 / 成本 | 两平台 |
| B8 | 指标 | 服务端 per-LUN IOPS / p99、`iostat` / `Get-Counter` | 可观测 | 开发量 | 两平台 |

> **B 类的诚实说明**：如果数据仍在 HDD，SSD 不参与数据读取，B 类不会把 4K 随机从 350 IOPS
> 提到几万；它只是把路径上的固定开销和尾延迟压低。**SSD 要在数据路径上生效，必须承载数据（A 类）
> 或做缓存（C 类）。**

### 3.3 缓存 / 分层型（明确排除，但列出备选）

| 方案 | 为什么不是纯加速 | 什么时候才考虑 |
|---|---|---|
| dm-cache writethrough | 有副本、有淘汰；写仍落 HDD | 不能迁移数据、只想加速读 |
| dm-cache writeback | 同上 + 掉电丢未回写数据 | 能接受数据风险、写极多 |
| Storage Spaces tiering | 自动迁移 / 分层，行为接近缓存 | Windows 全栈托管、可接受复杂度 |
| 客户端 L2 | 每台客户端一份副本 | 网络差、单机重复读多 |
| 只读母盘副本 | 边界：无淘汰但是第二份数据 | 只读 CDN 式分发；Linux 受 snapshot 限制 |

---

## 4. 量化：收益从哪来

### 4.1 基线（README 实测，10 GiB，CrystalDiskMark，单位 MB/s）

| 项目 | 无缓存（HDD） | 有缓存（客户端） | 本地 SSD |
|---|---:|---:|---:|
| SEQ1M Q8T1 读 | 251.08 | **2627.13** | 3058.90 |
| SEQ1M Q1T1 读 | 238.18 | **2443.11** | 1831.42 |
| RND4K Q32T1 读 | 2.12 | **183.79** | 662.84 |
| RND4K Q1T1 读 | 1.39 | **113.41** | 35.70 |
| SEQ1M Q8T1 写 | 242.84 | 233.25 | 3524.92 |
| SEQ1M Q1T1 写 | 113.25 | 110.94 | 2863.12 |
| RND4K Q32T1 写 | 4.74 | 4.61 | 662.76 |
| RND4K Q1T1 写 | 6.17 | 6.70 | 84.75 |

换算成 4K IOPS：

- HDD：4K Q1T1 读约 **356 IOPS**，写约 **1580 IOPS**。
- 本地 SSD：4K Q1T1 读约 **9.1k IOPS**，Q32T1 约 **169k IOPS**；写 Q1T1 约 **21.7k IOPS**，Q32T1 约 **169k IOPS**。

### 4.2 写路径是重点

客户端缓存对写无效（表里「有缓存」写和无缓存几乎一样）。所以：

- 游戏安装 / 更新、存档、日志、反作弊写入，全部落在服务端差异盘。
- HDD 4K Q1T1 写 6.17 MB/s；SSD 本地 84.75 MB/s，理论上约 13 倍；顺序写 113 → 2863 MB/s，但受网络上限。
- 如果用户抱怨「装游戏 / 更新 / 存档卡」，**服务端 SSD 是唯一有效的纯加速手段**。

### 4.3 共享母盘的冷读

- 一个母盘被 N 个客户端共享；冷读（首次启动、更新后、客户端 L2 未命中）都打服务端。
- HDD 的约 350 IOPS 是**全局**的；N 个客户端同时冷启动会互相抢磁头，尾延迟急剧上升。
- SSD 一份母盘服务所有客户端；客户端 L2 是 N 份副本，容量和维护成本随 N 增长。
- 但注意：客户端 L1 命中时根本不过网络，仍比服务端 SSD 快；所以 SSD 库可以关 L2，L1 保留。

### 4.4 容量模型

设母盘 P GB、每用户差异盘 D GB、用户数 N：

- 整库上 SSD：`P + N×D`。
- 客户端 L2 方案：`N×L2`（每台一份，通常 4 GiB 上限），但只覆盖各机重复读。
- 例：`P=100 GB、N=20、D=2 GB` → 140 GB；1 TB SSD 可放约 7 个这样的库。
- 例：`P=200 GB、N=50、D=3 GB` → 350 GB；1 TB SSD 只能放 2~3 个。

结论：SSD 容量决定「能钉住多少个热库」；不是所有库都上 SSD。
如果 SSD 只能买 1~2 TB，别按「整库放置」算，按 dm-cache 的「缓存容量」算：
它不要求覆盖整个热库，只要求覆盖「最热的那一部分块」。

### 4.5 网络上限

| 链路 | 理论 | 实际（约） | 影响 |
|---|---:|---:|---|
| 1GbE | 125 MB/s | 110-118 MB/s | SSD 顺序读被完全掩盖；只对 4K IOPS / 延迟有意义 |
| 2.5GbE | 312 MB/s | 280-295 MB/s | 顺序读约 HDD 的 1.2 倍；随机仍大幅受益 |
| 10GbE | 1250 MB/s | 1.1-1.18 GB/s | SSD 顺序可发挥；随机受 iSCSI / 客户端限制 |
| 25/40GbE | 3.1/5 GB/s | 取决于 CPU / 协议栈 | 需要 MPIO / RDMA；CPU 可能成瓶颈 |

> 先确认服务器到客户端的实际链路。若还是 1GbE，优先升级网络；否则服务端 SSD 的多数顺序收益看不到。

### 4.6 收益边界（什么时候不值得）

- 客户端 L1 命中率极高、工作集很小 → 服务端 SSD 收益有限。
- 只有一台客户端、重复读同一小数据集 → 客户端 L2 更便宜。
- 网络 1GbE → 顺序收益被链路吃掉。
- 写入量很小、读为主 → 静态热库放置或小 SSD dm-cache；Linux 受同池限制，不能只搬母盘。
- SSD 是 QLC / 无 DRAM / 小 SLC 缓存 → 随机写可能掉到 HDD 水平，必须先测持续写入曲线；
  做 writeback 缓存尤其危险。

### 4.7 小 SSD + 大 HDD：物理上只有两种分法

设 HDD 容量 X、SSD 容量 Y，且 Y << X。要让「没放进 SSD 的数据」也变快：

| 做法 | SSD 上是什么 | 受益数据 | 是否缓存 | 可预测性 | 适合 |
|---|---|---|---|---|---|
| 静态放置 | 热库的唯一副本 | 放进去的库 100% | 否 | 高 | 热库少且稳定 |
| 动态缓存 / 分层 | 热数据的副本 | HDD 池的全部数据（按命中率） | 是 | 依赖访问倾斜 | 热库多 / 变化 / 长尾 |
| 客户端 L2 | 本机读副本 | 本机重复读 | 是 | 高但冷启动无 | 客户端有 SSD 余量 |
| 只调路径 | 无 | 全部（小幅） | 否 | 高 | 预算极限 |

**没有第三种物理机制**：小 SSD 要么装「固定的那一部分」，要么装「动态热门的那一部分」。
前者是放置，后者是缓存。

成本例子（数量级，不是报价）：

- 20 TB HDD 库，全 SSD 需要 20 TB SSD；按 1 TB SSD 价格 P 计，成本约 20P。
- 2 TB 热集，静态放置需要 2 TB SSD，成本约 2P，只有这 2 TB 的库快。
- 1 TB dm-cache 服务 20 TB 池，成本约 1P；如果 80% 读落在 10% 数据上，命中率可能很高，
  但首次读 / 冷数据仍走 HDD。
- 客户端 L2：0 服务端 SSD 成本，用客户端已有的 SSD；每台一份副本，冷启动和写路径帮不上。

所以「避免买太多 SSD」不等于「不用缓存」；它通常意味着「用一块小 SSD 做缓存 / 分层，
或者只给少数热库做静态放置」。前者的上限由命中率决定，后者由 SSD 容量决定。

**怎么定 SSD 大小：**

1. 先用客户端 L1/L2 命中率、读写比、每次启动的实际读量，估「活跃工作集」。
2. 没有数据就先上 1 TB（或活跃工作集的 10%~20%），观察 1-2 周。
3. 看 `GET /v1/system/lvm` 的 `cache_read_hits` / `cache_read_misses`：
   命中率长期低于 60%~70% → 加大 cache，或改静态放置 top-N。
4. 先 `writethrough`（安全）；写压力大且 SSD 有 PLP + UPS 再评估 `writeback`。
5. 当前代码的 dm-cache 只在建池时挂，在线扩容未实现；先按 1~2 TB 规划，别一次买满。

---

## 5. 推荐方案

### 5.1 选型：先看 SSD 预算

| 预算 | 推荐形态 | 数据怎么放 | 客户端 L2 | 预期 |
|---|---|---|---|---|
| 足够覆盖全部热库 | **整库放置** | 热库整库在 SSD；冷库在 HDD | SSD 库可关 L2，保留 L1 | 热库全速；冷库仍 HDD |
| 只够 1~2 TB，库很大 | **小 SSD dm-cache（Linux）/ Storage Spaces 分层（Windows）** | HDD 池不变；SSD 做池级缓存 | 保留 L2，作为第二层 | 按命中率受益；首次读仍 HDD |
| 只够放几个热库，且热库稳定 | **静态放置 top-N** | 只搬最热的几个库到 SSD | 其余库靠 L2 | 这几个库全速；其余 HDD |
| 完全不想买服务端 SSD | **客户端 L2 + 路径调优** | 数据全在 HDD | 加大 L2、保留 L1 | 只加速本机重复读；写 / 冷读不变 |

**先别急着买服务端 SSD**：客户端已经有 SSD，L2 默认只有 4 GiB。如果客户端 SSD 还有空闲，
先把活跃游戏的 L2 提到 16~32 GiB，可能比买服务端 SSD 更便宜。它的代价是：
每台客户端一份副本、冷启动无效、写路径完全帮不上。它不能替代服务端 cache，
但可以作为第一层「零硬件成本」的缓冲。

两种目标形态：

```text
形态 A（预算足够）：
  HDD VG/pool：冷库、归档、大容量
  SSD VG/pool：热库（母盘 + 差异盘整体）
  storage 绑定 pool_ref；建库/迁移按 storage_id
  客户端：SSD 库关 L2，保留 L1

形态 B（预算有限，推荐先评估）：
  HDD VG/pool + 同 VG 的小 SSD cache-pool（dm-cache，writethrough 起步）
  HDD 上的 storage/repo 不变；SSD 作为池级读缓存
  客户端：保留 L2（第二层），L1 不动
  监控 hit/miss/dirty，命中率不达标再考虑加大或改静态放置
```

不要：

- 不要把 SSD 加到 HDD VG 后当**普通 thin 数据盘**用。LVM 会按空闲 extent 分配，
  无法保证热数据在 SSD；混合 VG 的普通数据不是分层。
- 形态 B 的 SSD 必须与 HDD 同 VG（dm-cache 的限制），但只做 **cache-pool**，
  不承载普通 LV —— 这是受控的，不是把数据 LV 混放。
- 不要指望 `is_nonrot=1` 让 HDD 变快；它只是告诉客户端「这是 SSD」。
- 不要把 dm-cache 当成「纯加速」；它是缓存。但在小 SSD + 大 HDD 的预算下，
  它是最经济的服务端选项。

### 5.2 Linux 落地

#### 5.2.1 预算足够：整库放置

1. 确认 SSD 是独立块设备（非系统盘、非已挂载、无文件系统）。
2. `POST /v1/system/pools` 创建 SSD VG + thin pool（`hdd_devices` 字段传 SSD 设备；
   命名如 `vgssd/poolssd`）。**不要**把 SSD 加到现有 HDD VG。
3. `POST /v1/storages` 创建 SSD 存储，`mode=thin`，`pool_ref=vgssd/poolssd`，路径挂载点。
4. 新库创建时传 `storage_id`；旧库 `POST /v1/repos/{id}/move`（先确保无租约、未挂载；
   复制期间源和目标同时占空间）。
5. 验证：`lvs -o lv_name,vg_name,pool_lv,devices`、`dmsetup table`、`iostat -x`；
   确认新 LV 在 SSD PV 上。
6. 可选调优：`hw_block_size`、`max_sectors_kb`、`scheduler`、`read_ahead_kb`、`fstrim`；
   先在单库 A/B。

#### 5.2.2 预算有限：给现有 HDD 池挂 dm-cache

**现状限制**：dm-cache 目前只在 `InitializePool` 建池时挂载
（`internal/platform/linuxlvm/pool.go:136-276`），已有 HDD 池没有「在线挂 cache」的 API。
两条落地路径：

1. **不改代码（先验证）**：维护窗口内按 `cache.go` 的同一组命令手动挂：
   - 先把 SSD 建成 PV 并加入**同一个 VG**（dm-cache 要求 cache 与 origin 同 VG）：
     `pvcreate <ssd-dev>`、`vgextend <vg> <ssd-dev>`。
   - `lvcreate --type cache-pool -n <pool>_cache -l 100%FREE --chunksize 256K --cachepolicy smq <vg> <ssd-dev>`
   - `lvconvert --type cache --zero y --cachepool <vg>/<pool>_cache <vg>/<pool>`
   - `lvchange --cachemode writethrough <vg>/<pool>_tdata`
   - 验证：`lvs -o+cache_mode,chunksize,cache_policy,cache_dirty_blocks,cache_read_hits,cache_read_misses`。
   - 风险：在线挂 cache 可能触发 suspend；必须先备份 / 快照，低峰操作。
2. **产品化（推荐）**：加 `POST /v1/system/pools/{vg}/{pool}/cache`（attach / detach / expand），
   复用 `ensureCachePool` / `attachCache` / `setCacheMode`；需要处理在线挂载、幂等、失败回滚。
   这是「小 SSD 放大到整个 HDD 池」最值得补的一个接口。

**参数建议：**

- 容量：先 1~2 TB；按 `cache_read_hits / (hits + misses)` 观察 1~2 周；不达标再扩。
- 模式：`writethrough` 起步（安全）；写压力大、且 SSD 有 PLP + UPS，再评估 `writeback`。
- chunk：256K 默认；随机 4K 多、SSD 容量小时，可试 64K / 128K（粒度更细、元数据更多）。
- policy：`smq` 默认；顺序扫描多时观察是否污染。
- 池：dm-cache 是**池级**，所有库共享；热库会互相驱逐。库多且差异大时，
  可拆多个 VG / pool，各自配 cache。

**容量与命中率：**

- 小 SSD 的收益完全取决于访问倾斜。游戏库通常「少数游戏占多数启动」，
  1 TB cache 对 20 TB 库可能有不错命中率；但首次安装 / 更新仍全走 HDD。
- 若命中率长期低，说明热集大于 cache 或访问太随机；此时静态放置 top-N 更划算。
- 若写入是主要痛点，writethrough 不会加速写；需要 writeback（风险）或 dm-writecache（更复杂）。

**不要做的事：**

- 不要把 SSD 加到 HDD VG 后当普通 thin 数据盘用；只做 cache-pool。
- 不要在没有 PLP / UPS 时开 writeback。
- 不要在业务高峰在线挂 cache；先备份。

### 5.3 Windows 落地

#### 5.3.1 预算足够：整库放置

1. 确认 SSD 卷（独立于系统卷；NTFS）。
2. 新建 storage 指向 SSD 卷上的目录；新库选该 storage；旧库 move（复制 VHDX）。
3. 可选：母盘改 fixed VHDX（`CreateFixed` 已存在但未用）；差异盘保持 dynamic / differencing。
4. 可选：VHDX 创建时设 4K logical / physical sector（需确认客户端 / OS 兼容；创建后不能改）。
5. NTFS：64K 分配单元（格式化时定，重建才生效）、关闭 last access / 8.3、电源高性能、
   SSD 写缓存开启、TRIM 开启、杀软 / 索引排除 VHDX 目录。
6. 验证：`Get-Counter '\PhysicalDisk(*)\*'`、`Get-IscsiServerTarget`、`Get-IscsiVirtualDisk`；
   客户端 CrystalDiskMark。

#### 5.3.2 预算有限：Storage Spaces 分层 / 静态放置

Windows 没有 dm-cache。小 SSD + 大 HDD 的可选路径：

1. **静态放置 top-N（无缓存语义，最稳）**：SSD 卷建 storage，只把最热的几个库 move 过去；
   其余库留在 HDD，靠客户端 L2。行为可预测，不涉及回写 / 淘汰。
2. **Storage Spaces 分层（缓存 / 分层语义）**：用 SSD + HDD 建 Storage Space，
   建 tiered virtual disk，NTFS 卷上放 storage 目录和 VHDX。
   - 这是主机层能力，不在本仓库代码里；需要 Windows Server 版本支持并实测。
   - 写缓存 / 分层行为由 Windows 管理，Vault 只看到 NTFS 目录。
   - 风险：分层 / 回写、SSD 寿命、故障恢复都比静态放置复杂。
3. **不推荐**：第三方块缓存（PrimoCache 等）上生产；驱动 / 兼容 / 支持成本高。

如果 Windows 是主平台，且不想引入 Storage Spaces，**静态放置 top-N + 客户端 L2**
是最可控的「避免买太多 SSD」方案。

### 5.4 迁移注意

- `Move` 要求：无活跃租约、所有盘未挂载、目标存储已挂载、目标剩余空间 ≥ 源数据
  （复制期间双份）。
- 迁移是整盘复制，不是 `pvmove` / `dm-clone`；大库耗时长，业务需停机窗口。
- Linux thin snapshot 同池约束：搬库是整库搬（母盘 + 所有差异盘），不能只搬母盘。
- 迁移后 iSCSI target 会重建；客户端需重新挂载（现有流程）。
- 建议先搬一个非关键库做 A/B，再批量。

### 5.5 客户端联动（产品化）

- 服务端在 repo / storage 元数据加 `tier: ssd|hdd|cached`（或 `storage.media` + `cache`）。
- 客户端对 `tier=ssd` 的库默认 `L2=0`（保留 L1），减少客户端 SSD 写放大和重复副本；
  `tier=cached` / `hdd` 的库保留 L2，作为服务端 cache 之外的第二层。
- 管理页显示「服务端介质」「是否有服务端缓存」和「建议缓存策略」。
- 这是纯展示 / 策略，不改变 iSCSI 语义。

### 5.6 调优清单（按收益 / 风险排序）

1. **先测再调**：服务端 `iostat -x 1`、客户端现有 cache stats、CrystalDiskMark、真实游戏。
2. Linux：SSD 调度器 `none`（NVMe）/ `mq-deadline`（SATA）、`read_ahead_kb` 128-1024、
   `fstrim` 周期、`issue_discards` 评估。
3. LIO：确认 `hw_block_size` 4K 是否兼容；`max_sectors_kb` 与客户端 MaxBurst 匹配；
   `emulate_write_cache` 仅在 SSD 有 PLP 时开。
4. Windows：电源高性能、写缓存、TRIM、NTFS 64K、关闭 last access / 8.3、杀软排除、
   固定 VHDX（可选）。
5. 网络：2.5G/10G、巨帧、MPIO；先测链路实际上限。
6. **不要调**：没有基线时不要改 `emulate_write_cache`、不要开 writeback；
   不要把 SSD 混作普通 thin 数据盘（dm-cache 的 SSD 只做 cache-pool）。

---

## 6. 实施计划

| 阶段 | 内容 | 产出 | 工作量 |
|---|---|---|---|
| **P0 基线 / 定容量** | 采集 HDD 无缓存 / 客户端缓存 / 本地 SSD；客户端 L1/L2 命中率、读写比、每次启动读量；服务端 iostat / Windows 计数器 | 基线 + SSD 容量建议 | 1-2 天 |
| **P1 零成本先做** | 客户端 L2 扩容（用现有客户端 SSD）、网络确认、HDD defrag / readahead、服务端 RAM | 低成本收益 | 1 天 |
| **P2 小 SSD 试点** | Linux：1~2 TB dm-cache writethrough（手动或新 API）；Windows：Storage Spaces 分层或静态 top-N；A/B | 命中率 / 延迟对比 | 2-4 天 |
| **P3 决策** | 命中率达标 → 扩容 / 产品化；不达标 → 静态放置 top-N 或加大 SSD | 选型结论 | 1 天 |
| **P4 产品化** | cache attach / expand API（Linux）、tier 字段、客户端 L2 联动、服务端指标 | 代码 / 文档 | 3-5 天 |
| P5 可选 | 元数据 SSD、fixed VHDX、4K sector、writeback | 实验报告 | 按需 |

验收指标（建议）：

- 冷 4K 随机读 IOPS ≥ 5× HDD；p99 延迟 ≤ 目标（按业务定，如 ≤ 5 ms）。
- 4K 随机写 IOPS ≥ 5× HDD（writethrough 只验收读；写要 writeback / 静态放置）。
- 多客户端（4 / 8）并发冷读，聚合吞吐不下降 / 尾延迟不爆炸。
- 顺序读达到链路上限的 ≥ 80%。
- **dm-cache 读命中率 ≥ 目标（先定 60%~70%，按业务）**；`cache_dirty_blocks` 可控；
  SSD SMART / 温度 / 寿命正常。
- 迁移后数据校验一致；thin pool 水位正常；SSD SMART / TRIM 正常。
- 客户端 L1 命中率不降；SSD 库 L2 关闭后无回归。
- **如果命中率不达标，明确切换到静态放置或加大 SSD，不继续盲目扩容。**

---

## 7. 风险与边界

| 风险 | 说明 | 缓解 |
|---|---|---|
| SSD 寿命 | 写入放大、QLC / SLC 缓存掉速、DWPD 不足 | 选企业级 / 高 DWPD；预留 OP；监控 SMART；先测持续写 |
| 掉电 | 开 write cache / 无 PLP 会丢数据 | 默认不开；只对带 PLP 的企业级 SSD 开 |
| 容量 | 热库增长、差异盘膨胀、快照占空间 | 水位告警；配额；定期 `fstrim`；不把系统盘当数据盘 |
| 混布 VG | SSD 作为普通 thin 数据盘加入 HDD VG，LVM 不保证热数据在 SSD | 只让 SSD 做 cache-pool；否则物理隔离 VG / pool |
| 迁移停机 | Move 是复制，需无租约 / 未挂载 | 维护窗口；先小库；预留双份空间 |
| Linux 同池 | 母盘 / 差异盘不能跨池 | 整库放置；不承诺「只母盘上 SSD」 |
| 网络瓶颈 | 1GbE 掩盖 SSD 顺序收益 | 先升级链路；用 4K / 随机指标验收 |
| 客户端缓存冗余 | 整库 SSD 库仍开 L2，重复副本 | tier 标记 + 整库 SSD 自动关 L2；dm-cache 库保留 L2 |
| 监控缺失 | 服务端没有 per-LUN 延迟 | 先 iostat / 计数器；后续加指标 |
| dm-cache 误用 | 池级、writethrough、非按库 | 小 SSD 时它是经济选项；明确是缓存，不是纯加速 |
| cache 命中率不达预期 | 热集大于 SSD 或访问太随机 | 先测工作集；1 TB 试点；不达标改静态 top-N |
| cache 设备故障 | writethrough 可回源；writeback 可能丢脏数据 | writethrough 起步；writeback 配 PLP + UPS；监控 dirty |
| 在线挂 cache | 现有池挂 cache 可能 suspend / 失败 | 维护窗口、备份、幂等 / 回滚；优先新池或产品化 API |
| 池级噪声 | 所有库共享一个 cache，热库互相驱逐 | 拆分 VG / pool；或静态放置 |
| cache 扩容 | 当前代码只在建池时挂，未实现在线扩容 | 先按 1~2 TB 规划；后续加 API |

---

## 8. 需要确认的问题

1. 服务端是 Windows 还是 Linux？SSD 是独立 NVMe / SATA 还是系统盘？容量、型号、DWPD？
2. **SSD 预算上限是多少？** 能接受 1 TB、2 TB 还是更多？是一次买满还是先试点再扩？
3. HDD 库总量、热库 / 活跃工作集多大？差异盘平均多大？总库数、并发客户端数？
4. 客户端 L1/L2 命中率现在是多少？客户端 SSD 还有多少空闲？加大 L2 是否可行？
5. 负载以只读游戏启动为主，还是 Steam 更新 / 存档写入也重？读 / 写比例？
6. 服务器到客户端链路是 1G / 2.5G / 10G？是否有 MPIO / 巨帧？
7. **「不做缓存」是硬要求，还是「不想买太多 SSD」的表述？**
   - 硬要求 → 只能静态放置 top-N + 客户端 L2，长尾库仍慢。
   - 预算优先 → 小 SSD dm-cache（Linux）/ Storage Spaces 分层（Windows）更划算。
8. 现有 HDD 池是否已有数据、能否停机备份？在线挂 cache 有风险；
   是否接受「新建带 cache 的池 + 逐步迁移」？
9. 能否先拿一个热库 / 一个 1 TB SSD 做 A/B，再决定是否批量？

---

## 附 A：代码位置

| 主题 | 位置 |
|---|---|
| Linux thin LV 创建 / 差异盘 / 激活 | `internal/platform/linuxlvm/thin.go:203-239`、`:266-295`、`:379-395` |
| Linux 磁盘引用 / VG 推断 | `internal/platform/linuxlvm/thin.go:67-147` |
| Linux 池初始化 / dm-cache 编排 | `internal/platform/linuxlvm/pool.go:136-276` |
| dm-cache 原语 | `internal/platform/linuxlvm/cache.go` |
| 一个 VG 一个 thin pool | `internal/platform/linuxlvm/poolcatalog.go:15-25` |
| LIO iblock / 属性 | `internal/platform/liotarget/configfs.go:64-68`、`target.go:502-573`、`runner.go:523-545` |
| Windows VHDX 创建 | `internal/platform/winvhd/manager.go:26-143` |
| Windows iSCSI target 顺序 | `internal/platform/winbackend/backend.go:297-371` |
| Windows PowerShell target 脚本 | `internal/platform/winps/scripts/iscsi_set_target.ps1:297-440` |
| 建库 / 放置 | `internal/app/repo.go:148-360`、`:2124-2240` |
| 存储 / pool_ref | `internal/app/storage.go:509-597` |
| 整库迁移 | `internal/app/repomove.go:71-...` |
| 客户端读缓存 | `internal/agent/cacheproxy.go:20-34`、`internal/iscsiproxy/target.go:48-89` |
| 客户端 L2 无缓冲 I/O | `internal/iscsicache/l2/store_windows.go:15-40`、`:146` |
| 服务端池 / 缓存状态 API | `internal/api/platform.go:36-95`、`internal/platform/linuxlvm/pool.go:55-120` |

## 附 B：验证命令（Linux）

```bash
# 确认 LV 落在哪块 PV 上
lvs -a -o lv_name,vg_name,pool_lv,devices,seg_type
pvs -o pv_name,vg_name,pv_size,pv_free,dev_size

# 确认 thin pool / dm-cache 状态
dmsetup status
dmsetup table

# I/O 基线（服务端）
iostat -x 1
iostat -x -d 1

# SSD 调度器 / 预读
cat /sys/block/nvme0n1/queue/scheduler
cat /sys/block/nvme0n1/queue/read_ahead_kb
blockdev --getra /dev/nvme0n1

# LIO 属性（以实际 targetcli 输出为准）
targetcli ls /backstores/iblock_0
```

## 附 C：验证命令（Windows）

```powershell
Get-Counter '\PhysicalDisk(*)\*' -SampleInterval 1 -MaxSamples 10
Get-Counter '\LogicalDisk(*)\*' -SampleInterval 1 -MaxSamples 10
Get-IscsiServerTarget
Get-IscsiVirtualDisk
Get-Volume
Get-PhysicalDisk | Format-Table FriendlyName,MediaType,BusType,Size
```

---

## 附 D：本文的边界

- 本文只做调研与方案，不修改源码。
- 文中性能数字来自 `README.zh-CN.md` 的一台开发机实测，不是产品承诺；
  服务端 SSD 的实际收益必须在目标机器 + 目标网络 + 真实游戏负载上复测。
- 「纯加速」的定义按 §1 的判定口径；如果产品定义不同，需要重新划分 A/C 类。
