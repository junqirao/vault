import type { AgentCacheFile, AgentCacheStatus, AgentCacheTargetStatus, AgentMountState } from '../../api/agentTypes'

export interface CacheTotals {
  /** L2 磁盘缓存占用：活动目标用逻辑槽位用量，退出后保留的文件用落盘大小。 */
  usedBytes: number
  /**
   * 本地缓存合计上限；0 表示**无限**（界面显示 ∞，进度条空轨道）。
   */
  limitBytes: number
  /** 是否有任何缓存目标或落盘文件。 */
  hasCache: boolean
  /** used / limit，0–100；limit 为 0 时为 0。 */
  percent: number
}

/**
 * 汇总「本地缓存」的总体用量（L2：磁盘缓存）。
 *
 * L1 是进程内存，不占"本地缓存大小"，所以概览进度条只统计 L2；活动目标用代理给出的
 * 逻辑槽位用量，退出/卸载后保留的文件没有活动目标，按落盘大小估算。两者不能混在一起
 * 重复计算，所以先用活动目标占掉库键，剩下的文件再计入。
 *
 * 上限只认「本地缓存上限」：没配（0）就是**无限**，不拿各库上限之和冒充总量，
 * 否则界面会显示一个"看起来有上限"的数字。
 */
export function cacheTotals(cache: AgentCacheStatus | null, mounts: AgentMountState[]): CacheTotals {
  const targets = cache?.targets ?? []
  const files = cache?.files ?? []
  const mountByAllocation = new Map(mounts.map((mount) => [mount.allocation_id, mount]))
  const activeKeys = new Set<string>()
  let usedBytes = 0

  for (const target of targets) {
    const mount = mountByAllocation.get(target.allocation_id)
    activeKeys.add(mount?.repo_id || target.allocation_id)
    usedBytes += target.l2_used_bytes
  }

  for (const file of files) {
    if (activeKeys.has(file.key)) continue
    activeKeys.add(file.key)
    usedBytes += file.l2_bytes
  }

  // 0 = 无限：没有分母，界面上用 ∞ 表示，进度条画空轨道。
  const limitBytes = cache?.local_limit_bytes ?? 0
  return {
    usedBytes,
    limitBytes,
    hasCache: usedBytes > 0 || limitBytes > 0,
    percent: limitBytes > 0 ? Math.min(100, (usedBytes / limitBytes) * 100) : 0
  }
}

/**
 * 单行（一个存储库）的用量口径，与管理弹窗、仓库卡片保持一致：
 * 活动目标用 L1+L2 的实际配额与逻辑占用；只有落盘文件时用 L2 文件大小 / 每库 L2 上限。
 */
export function cacheRowUsage(
  target: AgentCacheTargetStatus | undefined,
  file: AgentCacheFile | undefined,
  l2LimitBytes: number
): { usedBytes: number; limitBytes: number } {
  if (target) {
    return {
      usedBytes: target.l1_used_bytes + target.l2_used_bytes,
      limitBytes: target.l1_limit_bytes + target.l2_limit_bytes
    }
  }
  if (file) {
    return { usedBytes: file.l2_bytes, limitBytes: l2LimitBytes }
  }
  return { usedBytes: 0, limitBytes: 0 }
}
