import { Card, Skeleton, Space, Tooltip, Typography } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { SectionTitleLink } from '../../components/SectionTitleLink'
import { UsageBar } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { StorageDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { storageUsage } from './storageUsageMetrics'

/**
 * 把所有存储合成一个概览。
 *
 * 同一卷上的多个目录存储不能重复计容量：按 `volume_name`（没有则按路径）去重，
 * 每卷只取一份"已用 / 总量"。这样首页那一条总览与存储详情里的逐条数据不会对不上。
 */
function aggregateStorageUsage(storages: StorageDTO[]): { used: number; total: number; percent: number } {
  const seen = new Set<string>()
  let used = 0
  let total = 0
  for (const storage of storages) {
    const key = storage.volume_name || storage.path
    if (seen.has(key)) continue
    seen.add(key)
    const usage = storageUsage(storage)
    used += usage.used
    total += usage.total
  }
  const percent = total > 0 ? Math.min(100, (used / total) * 100) : 0
  return { used, total, percent }
}

/**
 * 存储库首页的「存储」总览卡片。
 *
 * 与「我的配额」模块同款排版：一行总量 + 一条进度条 + 底部"剩余 / 存储数量"。
 * 不在首页罗列每个存储：入口是**标题 + 箭头合成的 text 按钮**，直接跳到管理端「存储」
 * 页面看逐块明细（不再单独维护一份只读页）。普通用户没有管理权限时由跳转方给提示。
 */
export function StorageUsageCard({
  onOpen,
  embedded = false
}: {
  onOpen: () => void
  /** 嵌在「用量」大模块里时不再套一层 Card，由外层负责边框与留白。 */
  embedded?: boolean
}): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages()
  })
  const storages = storagesQuery.data?.items ?? []
  const aggregate = aggregateStorageUsage(storages)

  const content = storagesQuery.isLoading ? (
    <Skeleton active title={false} paragraph={{ rows: 2 }} />
  ) : storagesQuery.error ? (
    <Typography.Text type="danger">{t('storage.loadFailed')}</Typography.Text>
  ) : storages.length === 0 ? (
    <Typography.Text type="secondary">{t('storage.empty')}</Typography.Text>
  ) : (
    <>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: spacing.xs, flexWrap: 'wrap' }}>
        {/*
          入口统一用 SectionTitleLink（文字 + 居中右箭头 + paddingInline: 0），保证与
          下面「我的配额」「本地缓存」「性能」的标题左边缘和字号完全一致。
        */}
        <Tooltip title={t('storage.usage.detail')}>
          <SectionTitleLink title={t('storage.usage.detail')} onClick={onOpen}>
            {t('storage.title')}
          </SectionTitleLink>
        </Tooltip>
        <Typography.Text style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {`${t('storage.usage.used')} ${formatBytes(aggregate.used)} / ${formatBytes(aggregate.total)}`}
        </Typography.Text>
      </div>
      {/* 与存储库卡片同一条用量条（UsageBar）：百分数叠在条正中，颜色按占比分档。 */}
      <UsageBar percent={aggregate.percent} />
      <Space size={spacing.md} wrap style={{ marginTop: spacing.xxs }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('storage.usage.remaining')} {formatBytes(Math.max(0, aggregate.total - aggregate.used))}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('storage.usage.count', { count: storages.length })}
        </Typography.Text>
      </Space>
    </>
  )

  if (embedded) {
    return (
      <div
        style={{ height: '100%', display: 'flex', flexDirection: 'column', justifyContent: 'center' }}
      >
        {content}
      </div>
    )
  }

  return (
    <Card styles={{ body: { padding: spacing.md } }} style={{ height: '100%' }}>
      {content}
    </Card>
  )
}

