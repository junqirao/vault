import { useState } from 'react'
import { Card, Tooltip, Typography } from 'antd'

import { SectionTitleLink } from '../../components/SectionTitleLink'
import { UsageBar } from '../../components/UsageBar'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { CacheManagementModal } from '../settings/CacheManagementModal'
import { cacheTotals } from '../settings/cacheUsage'

export interface CacheOverviewProps {
  /** 嵌在「用量」大模块里时不再套一层 Card。 */
  embedded?: boolean
}

/**
 * 首页「用量」模块里的本地缓存概览。
 *
 * 展示的是 **L2 磁盘缓存**的最终大小（活动目标用逻辑槽位、退出后保留的文件用落盘大小），
 * 分母优先用用户在设置里配的「本地缓存上限」；没配（0 = 不额外限制）时显示 ∞。
 * 标题「本地缓存 →」可点击打开缓存管理弹窗；设置页里也有独立的「缓存管理器」入口。
 */
export function CacheOverview({ embedded = false }: CacheOverviewProps): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const [open, setOpen] = useState(false)
  const totals = cacheTotals(agent.cache, agent.state?.mounts ?? [])
  const limitText = totals.limitBytes > 0 ? formatBytes(totals.limitBytes) : t('common.unlimited')

  const content = (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 4, flexWrap: 'wrap' }}>
        <Tooltip title={t('settings.cache.manageTitle')}>
          <SectionTitleLink title={t('settings.cache.manageTitle')} onClick={() => setOpen(true)}>
            {t('repo.cache.title')}
          </SectionTitleLink>
        </Tooltip>
        <Typography.Text style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {t('settings.cache.headerUsage', { used: formatBytes(totals.usedBytes), limit: limitText })}
        </Typography.Text>
      </div>
      <UsageBar
        percent={totals.percent}
        label={totals.limitBytes > 0 ? undefined : t('common.unlimited')}
        style={{ marginTop: 2 }}
      />
      <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
        {agent.available
          ? t('repo.cache.disk', { size: formatBytes(agent.cache?.disk_l2_bytes ?? 0) })
          : t('repo.cache.unavailable')}
      </Typography.Text>
      <CacheManagementModal open={open} onClose={() => setOpen(false)} />
    </>
  )

  if (embedded) {
    return (
      <div style={{ height: '100%', display: 'flex', flexDirection: 'column', justifyContent: 'center' }}>
        {content}
      </div>
    )
  }

  return (
    <Card styles={{ body: { padding: 16 } }} style={{ height: '100%' }}>
      {content}
    </Card>
  )
}
