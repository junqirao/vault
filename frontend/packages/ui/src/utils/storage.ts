import type { StorageDTO } from '../api/types'

/**
 * 存储用量的统一口径：有分配容量（thin 卷）时看"已用 / 分配容量"，
 * 目录模式看"卷已用 / 卷容量"。
 *
 * 建库、迁移等所有"选存储"的下拉都必须用这同一个口径，
 * 否则同一块存储在列表页和下拉里会显示成两个不同的占用。
 */
export function storageUsage(storage: StorageDTO): { usedBytes: number; totalBytes: number } {
  if (storage.size_bytes > 0) {
    return { usedBytes: storage.used_bytes, totalBytes: storage.size_bytes }
  }
  const total = storage.total_bytes
  return { usedBytes: Math.max(0, total - storage.free_bytes), totalBytes: total }
}
