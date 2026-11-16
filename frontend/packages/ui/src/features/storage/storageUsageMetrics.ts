import type { StorageDTO } from '../../api/types'

/**
 * 存储用量的统一口径：有底层卷时看"已用 / 分配容量"，目录模式看"卷已用 / 卷容量"。
 *
 * 单独放在 .ts 里而不是跟卡片组件放一起：组件文件里混入非组件导出会让
 * Vite 的 Fast Refresh 整页刷新（见 StorageUsage.tsx）。
 */
export function storageUsage(storage: StorageDTO): { used: number; total: number; percent: number } {
  const total = storage.size_bytes > 0 ? storage.size_bytes : storage.total_bytes
  const used =
    storage.size_bytes > 0 ? storage.used_bytes : Math.max(0, storage.total_bytes - storage.free_bytes)
  const percent = total > 0 ? Math.min(100, (used / total) * 100) : 0
  return { used, total, percent }
}
