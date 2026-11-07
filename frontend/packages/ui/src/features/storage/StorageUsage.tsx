import { ArrowLeftOutlined, RightOutlined } from '@ant-design/icons'
import { Button, Card, Col, Progress, Row, Skeleton, Space, Tag, Tooltip, Typography } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { EmptyState } from '../../components/EmptyState'
import { PageShell } from '../../components/PageShell'
import { usageColor } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { StorageDTO } from '../../api/types'
import { usePermission } from '../../hooks/useAuth'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

/** 存储用量的统一口径：有底层卷时看"已用 / 分配容量"，目录模式看"卷已用 / 卷容量"。 */
function storageUsage(storage: StorageDTO): { used: number; total: number; percent: number } {
  const total = storage.size_bytes > 0 ? storage.size_bytes : storage.total_bytes
  const used =
    storage.size_bytes > 0 ? storage.used_bytes : Math.max(0, storage.total_bytes - storage.free_bytes)
  const percent = total > 0 ? Math.min(100, (used / total) * 100) : 0
  return { used, total, percent }
}

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
 * 存储详情卡：每块存储一张小卡片（详情页一行两张）。
 *
 * 排版与首页「存储」卡片保持一致：标题行右侧报已用/总量，下面是同一条进度条；
 * 进度条的颜色统一走 usageColor（与库卡片、配额条同一套分档），占用越高越偏橙/红。
 * 这里只比首页多一行挂载路径，方便管理员核对是哪块盘。
 */
function StorageUsageTile({ storage }: { storage: StorageDTO }): JSX.Element {
  const { t } = useI18n()
  const { used, total, percent } = storageUsage(storage)
  return (
    <Card
      style={{ height: '100%' }}
      styles={{
        body: {
          padding: spacing.md,
          display: 'flex',
          flexDirection: 'column',
          gap: spacing.xs
        }
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'baseline',
          justifyContent: 'space-between',
          gap: spacing.sm,
          flexWrap: 'wrap'
        }}
      >
        <Space size={spacing.xs} style={{ minWidth: 0 }}>
          <Typography.Text strong ellipsis={{ tooltip: storage.name }} style={{ minWidth: 0 }}>
            {storage.name}
          </Typography.Text>
          {!storage.enabled ? <Tag>{t('storage.usage.disabled')}</Tag> : null}
          <Tag>{t(`storage.kind.${storage.kind || 'dir'}`)}</Tag>
          {storage.media_type && storage.media_type !== 'unknown' ? (
            <Tag color={storage.media_type === 'ssd' ? 'green' : 'orange'}>
              {t(`storage.media.${storage.media_type}`)}
            </Tag>
          ) : null}
        </Space>
        <Typography.Text
          type="secondary"
          style={{ fontSize: fontSize.sm, fontVariantNumeric: 'tabular-nums' }}
        >
          {`${formatBytes(used)} / ${formatBytes(total)} · ${Math.round(percent)}%`}
        </Typography.Text>
      </div>
      <Progress
        percent={Math.round(percent)}
        showInfo={false}
        size="small"
        strokeColor={usageColor(percent)}
        trailColor={palette.neutralFill}
        style={{ marginTop: spacing.xxs, marginBottom: 0 }}
      />
      <Typography.Text
        type="secondary"
        ellipsis={{ tooltip: storage.path }}
        style={{ display: 'block', fontSize: fontSize.xs }}
      >
        {storage.path}
      </Typography.Text>
    </Card>
  )
}

/**
 * 存储库首页的「存储」总览卡片。
 *
 * 与「我的配额」模块同款排版：一行总量 + 一条进度条 + 底部"剩余 / 存储数量"。
 * 不在首页罗列每个存储，明细只在只读存储页看；入口是**标题 + 箭头合成的 text 按钮**
 * （一级文字 + 组件自带的尾置图标，"存储 →" 整体可点，点击面积也比单独一个小箭头大），
 * 卡片本身不再整块可点（避免与卡片内其他区域的可点语义混淆、也不再给整张卡片
 * 加上悬停变色）。管理入口在只读存储页右上角（见 StorageUsagePage）。
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
  const percent = Math.round(aggregate.percent)

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
          入口就是「存储 →」这一颗按钮：箭头用 Button 自带的 icon + iconPosition="end"，
          不手写 span/SVG —— 手写的话箭头和文字各自占一行内位置，字号、行高、间距全要自己
          调，结果就是箭头离标题很远、基线也对不齐（真实反馈："这个按钮要对齐，不要离太远"）。
          paddingInline: 0 保证标题左边缘与下面「我的配额」标题对齐（两张卡上下叠在同一模块里）。
        */}
        <Tooltip title={t('storage.usage.detail')}>
          <Button
            type="text"
            size="small"
            aria-label={t('storage.usage.detail')}
            icon={<RightOutlined style={{ fontSize: 10 }} />}
            iconPosition="end"
            onClick={onOpen}
            style={{ paddingInline: 0, height: 'auto', fontWeight: 600 }}
          >
            {t('storage.title')}
          </Button>
        </Tooltip>
        <Typography.Text style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {`${t('storage.usage.used')} ${formatBytes(aggregate.used)} / ${formatBytes(aggregate.total)}`}
        </Typography.Text>
        <Typography.Text strong style={{ fontSize: fontSize.sm }}>
          {`${percent}%`}
        </Typography.Text>
      </div>
      <Progress
        percent={percent}
        showInfo={false}
        size="small"
        strokeColor={usageColor(aggregate.percent)}
        trailColor={palette.neutralFill}
        style={{ marginTop: spacing.xxs, marginBottom: 0 }}
      />
      <Space size={spacing.md} wrap style={{ marginTop: spacing.sm }}>
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

/**
 * 客户端只读存储页：每个存储的用量进度条。
 *
 * 没有任何管理动作（启停/挂载/扩容/删除都不在这里）：普通用户只需要知道"空间用了多少"。
 * 管理入口在本页右上角的「管理」按钮，且仅超级管理员可点。
 */
export function StorageUsagePage({
  onBack,
  onManage
}: {
  onBack: () => void
  onManage: () => void
}): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { isSuperAdmin } = usePermission()
  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages()
  })
  const storages = storagesQuery.data?.items ?? []

  return (
    <PageShell
      title={t('storage.title')}
      leading={
        <Button icon={<ArrowLeftOutlined />} onClick={onBack}>
          {t('common.back')}
        </Button>
      }
      extra={
        <Tooltip title={isSuperAdmin ? t('storage.usage.manage') : t('storage.usage.noPermission')}>
          <span style={{ display: 'inline-block' }}>
            <Button type="primary" disabled={!isSuperAdmin} onClick={onManage}>
              {t('storage.usage.manageButton')}
            </Button>
          </span>
        </Tooltip>
      }
    >
      {storagesQuery.isLoading ? (
        <Skeleton active paragraph={{ rows: 4 }} />
      ) : storagesQuery.error ? (
        <Typography.Text type="danger">{t('storage.loadFailed')}</Typography.Text>
      ) : storages.length === 0 ? (
        <EmptyState title={t('storage.empty')} />
      ) : (
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <Typography.Title level={5} style={{ margin: 0 }}>
            {t('storage.usage.title')}
          </Typography.Title>
          {/* 每块存储一张小卡片，一行两个：信息量比原来的单行更大，扫一眼仍能横向对比。 */}
          <Row gutter={[spacing.md, spacing.md]}>
            {storages.map((storage) => (
              <Col key={storage.id} span={12}>
                <StorageUsageTile storage={storage} />
              </Col>
            ))}
          </Row>
        </Space>
      )}
    </PageShell>
  )
}
