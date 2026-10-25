import dayjs from 'dayjs'

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'] as const

/** 以 1024 进制格式化字节数。 */
export function formatBytes(bytes: number | undefined | null): string {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) return '-'
  if (bytes === 0) return '0 B'
  const negative = bytes < 0
  let value = Math.abs(bytes)
  let unitIndex = 0
  while (value >= 1024 && unitIndex < BYTE_UNITS.length - 1) {
    value /= 1024
    unitIndex += 1
  }
  const digits = value >= 100 || unitIndex === 0 ? 0 : value >= 10 ? 1 : 2
  const unit = BYTE_UNITS[unitIndex] ?? 'B'
  return `${negative ? '-' : ''}${value.toFixed(digits)} ${unit}`
}

/**
 * 传输速率文案（如 `12.3 MB/s`）。
 *
 * 量级换算复用 formatBytes，只补一个 `/s` 后缀：速率与容量必须是同一套进位，
 * 否则一屏里会同时出现"已传 1.2 GB"和"速度 1150 MB/s"这种自相矛盾的组合。
 * 速率未知（0 / 缺失 / 负数）返回 `-` —— 与"速度为 0"（卡住不动）不是一回事。
 */
export function formatSpeed(bytesPerSecond: number | undefined | null): string {
  if (bytesPerSecond === undefined || bytesPerSecond === null || Number.isNaN(bytesPerSecond)) return '-'
  if (bytesPerSecond <= 0) return '-'
  return `${formatBytes(bytesPerSecond)}/s`
}

/**
 * 存储库用量：进度条百分比 + 「已用 / 上限」文案。
 *
 * 卡片、详情页概览共用一个口径（同一台机器上这两处必须显示同一个数）；管理端表格走服务端
 * 口径 —— 运维看的是"这块库被分配了多少容量"，与客户端"这块库我用了多少"本来就不是一回事。
 *
 * 分母取**库容量**（建库时设定的容量 = 那块盘的 VHDX 标称容量，服务端读时派生）。
 * 不能拿 `quota_bytes` 当分母：库没有"配额"概念，新建库根本不写这个字段（恒为 0），
 * 于是百分比恒为 0、进度条永远空着 —— 界面上就是"看不出用了多少"。
 * 存量老库里若还残留库级配额（>0），回退用它当分母，免得老库退化成没有分母。
 *
 * 分子取本机**实测**的卷用量（客户端挂载着这个库时才有，见 AgentMountVolumeUsage）：
 * 服务端的 used_bytes 是**已分配磁盘总量**（按盘的标称容量计费，见 implementation.md 的
 * "配额统计"），它等于库容量本身，不是用户眼里的用量。有实测值时以它为准（分母同样用卷容量：
 * 那就是这块盘的标称容量）。
 *
 * 第三个参数 `localOnly` 是客户端的规则（真实反馈："未挂载的情况下不展示已用存储的量，
 * 那个进度条置灰就行了"、"已用那里只展示一个上限就可以了"）：没有实测值时**不拿服务端数字
 * 兜底**，用量写 `-`（不知道），上限照报 —— 文案就是 `- / 1000 MB`。`-` 占住"已用"那个
 * 位置：用户既不会把"已分配容量"读成自己的用量，也不会把两个数看反。管理端不传（false），
 * 它要看的正是"这个库分配出去多大"那个绝对数。
 *
 * 别拿 `formatBytes(0)` 当"未知"：那是"确实一个字节没用"，两件事。
 *
 * 返回值给的是**零件**加整句：`text`（"已用 / 上限"）给管理端表格这类只要一句话的地方，
 * `usedText` / `totalText` 两半给客户端 —— 刚挂上、用量还在取数时，「已用」那半边要换成
 * 转圈（见 components/UsageBar 的 UsageValueText），整句没法局部替换。
 *
 * `measured` = 用量是不是本机实测值：置灰进度条、给「已用」标签挂悬浮说明都用它
 * （见 RepoCards 的 RepoUsage），免得每个调用方各自判一遍。
 */
export function repoUsage(
  repo?: {
    used_bytes?: number
    capacity_bytes?: number
    quota_bytes?: number
  },
  local?: { used_bytes?: number; total_bytes?: number },
  localOnly = false
): { percent: number; usedText: string; totalText: string; text: string; measured: boolean } {
  const localTotal = local?.total_bytes && local.total_bytes > 0 ? local.total_bytes : 0
  const measured = localTotal > 0
  // 用量未知用 undefined 表示（客户端没实测到），与"用了 0 字节"区分开。
  const used = measured ? (local?.used_bytes ?? 0) : localOnly ? undefined : (repo?.used_bytes ?? 0)
  const capacity = repo?.capacity_bytes && repo.capacity_bytes > 0 ? repo.capacity_bytes : 0
  const total = measured ? localTotal : capacity > 0 ? capacity : (repo?.quota_bytes ?? 0)
  const usedText = used === undefined ? '-' : formatBytes(used)
  // 分母为 0 = 上限根本不知道（不是"上限 0"）：只报已知的那半边，不写"上限 0"。
  if (total <= 0) return { percent: 0, usedText, totalText: '', text: usedText, measured }
  // 超 100% 是真的会发生的：用量按"每个差异盘按容量预留"计，共享库多位用户同时在建盘时
  // 会短暂超过单份容量。钳到 100 只影响条形长度，文字仍显示真实数值。
  const totalText = formatBytes(total)
  return {
    percent: used === undefined ? 0 : Math.min(100, Math.round((used / total) * 100)),
    usedText,
    totalText,
    text: `${usedText} / ${totalText}`,
    measured
  }
}

/**
 * 共享库的差异盘用量：把磁盘列表里 `kind=diff` 的盘按**物理占用**聚合。
 *
 * 为什么不能拿 `repo.used_bytes` 当"用了多少"：那是配额口径——已分配差异盘按**标称容量**
 * 计费，一块 1000 MB 的差异盘哪怕只写了 10 MB 也记 1000 MB。进度条用它就会直接顶到 100%，
 * 用户看到"这块盘快满了"会莫名焦虑（真实反馈），而实际上还空得很。
 * 差异盘真正吃掉多少空间，只有 `physical_bytes` 知道（thin 差异盘刚派生时接近 0）。
 *
 * 没有差异盘（独享库、还没建盘、查询未回）返回 undefined，调用方退回原口径。
 */
export function diffDiskUsage(
  disks?: { kind: string; physical_bytes?: number; size_bytes?: number }[]
): { used: number; total: number } | undefined {
  let used = 0
  let total = 0
  for (const disk of disks ?? []) {
    if (disk.kind !== 'diff') continue
    used += disk.physical_bytes ?? 0
    total += disk.size_bytes ?? 0
  }
  return total > 0 ? { used, total } : undefined
}

/**
 * **分给我的那块**差异盘用量：先用分配记录找到 `user_id = 我` 的那条，再按它的 `disk_id`
 * 定位差异盘。
 *
 * 为什么不能把库里的差异盘加总（真实反馈：库里两块差异盘时显示成了 `40 MB / 49.9 GB`）：
 * 共享库的每块差异盘属于**一个用户**，分母相加会变成"2 份容量"（49.9 GB = 2 × 24.9 GB），
 * 用户根本对不上自己那块盘（磁盘页签里写的是 36 MB / 24.9 GB）。
 * 服务端 `physical_bytes` 每 5 分钟采样一次，前端按 60s 轮询即可跟上。
 */
export function userDiffUsage(
  disks?: { id: string; kind: string; physical_bytes?: number; size_bytes?: number }[],
  allocations?: { disk_id: string; user_id: string; state: string }[],
  userId?: string
): { used: number; total: number } | undefined {
  if (!userId) return undefined
  const mine = (allocations ?? []).find((alloc) => alloc.user_id === userId && alloc.state !== 'released')
  if (!mine) return undefined
  const disk = (disks ?? []).find((item) => item.id === mine.disk_id)
  if (!disk || disk.kind !== 'diff') return undefined
  return { used: disk.physical_bytes ?? 0, total: disk.size_bytes ?? 0 }
}

/** 容量输入框统一使用的单位后缀（存量值仍是字节，只在表单里换算成 M 展示/录入）。 */
export const CAPACITY_UNIT = 'M'

/**
 * 母盘 VHDX 的最小容量：1 GB。
 *
 * 与服务端 sizing 规则一致（`CalculateVHDXSize`：容量留空或过小都取最小值，并向上取整到 1 GB）。
 * 用途：新建存储库时在客户端就能判断"库配额是否连母盘都装不下"——否则会等服务端返回
 * `repo.quota_exceeded`（"已用 0 B"，新建时很难懂，真实工单）。
 */
export const MIN_PARENT_BYTES = 1 << 30

/** 字节 → MB（1024 进制，保留 2 位小数；用于表单回填）。 */
export function bytesToMegabytes(bytes: number | undefined | null): number | undefined {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) return undefined
  return Math.round((bytes / (1024 * 1024)) * 100) / 100
}

/** MB → 字节（四舍五入到整数；用于表单提交）。 */
export function megabytesToBytes(megabytes: number | undefined | null): number | undefined {
  if (megabytes === undefined || megabytes === null || Number.isNaN(megabytes)) return undefined
  return Math.round(megabytes * 1024 * 1024)
}

/**
 * antd Form.Item 的容量字段配套属性。
 *
 * 用法：`<Form.Item name="quota_bytes" getValueProps={capacityValueProps} normalize={capacityNormalize}>`
 * —— 表单里**存储的仍是字节**（接口无需改动），但界面按 M 展示与录入。
 */
export const capacityValueProps = (bytes?: number): { value?: number } => ({
  value: bytesToMegabytes(bytes)
})

export const capacityNormalize = (megabytes?: number): number | undefined => megabytesToBytes(megabytes)

/** 格式化毫秒时间戳；0/空值显示为 '-'。 */
export function formatTime(ms: number | undefined | null): string {
  if (!ms) return '-'
  return dayjs(ms).format('YYYY-MM-DD HH:mm:ss')
}

/** 计算距今剩余秒数（不小于 0）。 */
export function remainingSeconds(expiresAtMs: number): number {
  return Math.max(0, Math.ceil((expiresAtMs - Date.now()) / 1000))
}

/**
 * 脱敏展示敏感值（token / 密钥）。
 *
 * 只保留头尾少量字符，绝不完整展示。
 */
export function maskSecret(value: string | undefined | null): string {
  if (!value) return '-'
  if (value.length <= 8) return '••••'
  return `${value.slice(0, 4)}••••${value.slice(-4)}`
}

/** 取实例 ID 的短前缀，便于用户核对机器。 */
export function shortId(id: string | undefined | null, length = 8): string {
  if (!id) return '-'
  return id.slice(0, length)
}
