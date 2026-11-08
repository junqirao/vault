import { useEffect, useMemo, useState } from 'react'
import { Button, Input, Modal, Popconfirm, Space, Typography, message } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'

import { agentApi } from '../../api/agentClient'
import type { AgentCacheFile, AgentCacheTargetStatus } from '../../api/agentTypes'
import { useApi } from '../../api/provider'
import { ErrorNotice } from '../../components/ErrorNotice'
import { UsageBar } from '../../components/UsageBar'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { cacheRowUsage, cacheTotals } from './cacheUsage'

export interface CacheManagementModalProps {
  open: boolean
  onClose: () => void
}

/** 弹窗里的一行：一个存储库，以及它在**本机**当前的挂载/缓存目标/落盘文件。 */
interface CacheManageRow {
  key: string
  repoId: string
  repoName: string
  target?: AgentCacheTargetStatus
  file?: AgentCacheFile
}

/**
 * 本地读缓存管理：搜索 + 瀑布流卡片列出所有存储库的缓存占用，支持单个 / 全部清除。
 *
 * 列表以服务端的库列表为准（当前活动服务端），再并上本机挂载记录与缓存目录里的
 * 文件级快照（`GET /agent/cache` 的 `files`）—— 后者覆盖多服务端场景下不属于当前
 * 活动服务端的库，以及**退出客户端/卸载后仍按库键保留的 L2 预热文件**。
 *
 * 清除只丢缓存内容（L1 + L2），门户目标、iSCSI 会话与挂载都保留，后续读照常重新填充
 * （见 POST /agent/cache/clear）。对没有活动目标的保留文件，清除会直接删除落盘文件。
 */
export function CacheManagementModal({ open, onClose }: CacheManagementModalProps): JSX.Element {
  const { t } = useI18n()
  const api = useApi()
  const agent = useAgent()
  const [error, setError] = useState<unknown>(null)
  const [query, setQuery] = useState('')
  const [clearingAll, setClearingAll] = useState(false)
  const [clearingId, setClearingId] = useState<string | null>(null)

  // 打开弹窗时拉一次存储库列表；关着的时候不必打扰服务端。
  const reposQuery = useQuery({
    queryKey: ['repos', 'cache-management'],
    queryFn: () => api.listRepos({ limit: 200 }),
    enabled: open,
    staleTime: 60_000
  })

  // 缓存用量会随读写持续变化：弹窗打开期间按 10s 轮询，关闭即停。
  const refreshCache = agent.refreshCache
  useEffect(() => {
    if (!open || !agent.available) return
    setError(null)
    setQuery('')
    void refreshCache()
    const timer = window.setInterval(() => void refreshCache(), 10000)
    return () => window.clearInterval(timer)
  }, [open, agent.available, refreshCache])

  const rows = useMemo<CacheManageRow[]>(() => {
    const mounts = agent.state?.mounts ?? []
    const targets = agent.cache?.targets ?? []
    const files = agent.cache?.files ?? []
    const targetByAllocation = new Map(targets.map((item) => [item.allocation_id, item]))
    const fileByKey = new Map(files.map((item) => [item.key, item]))
    const mountByRepo = new Map<string, (typeof mounts)[number]>()
    for (const mount of mounts) {
      if (!mountByRepo.has(mount.repo_id)) mountByRepo.set(mount.repo_id, mount)
    }

    const out: CacheManageRow[] = []
    const seen = new Set<string>()
    for (const repo of reposQuery.data?.items ?? []) {
      const mount = mountByRepo.get(repo.id)
      seen.add(repo.id)
      out.push({
        key: repo.id,
        repoId: repo.id,
        repoName: repo.name,
        target: mount ? targetByAllocation.get(mount.allocation_id) : undefined,
        file: fileByKey.get(repo.id)
      })
    }
    // 本机挂载但不在当前服务端列表里的库（多服务端 / 列表尚未刷新）：补一行，别把它漏掉。
    for (const mount of mounts) {
      if (seen.has(mount.repo_id)) continue
      seen.add(mount.repo_id)
      out.push({
        key: mount.repo_id,
        repoId: mount.repo_id,
        repoName: mount.repo_name || mount.repo_id,
        target: targetByAllocation.get(mount.allocation_id),
        file: fileByKey.get(mount.repo_id)
      })
    }
    // 只有落盘文件、既不在库列表也没有挂载记录的键（退出后保留的缓存、旧版本遗留）：
    // 也要列出来，否则用户既看不到它、也没法清除。
    for (const file of files) {
      if (seen.has(file.key)) continue
      seen.add(file.key)
      out.push({
        key: file.key,
        repoId: file.key,
        repoName: file.key,
        file
      })
    }
    return out
  }, [agent.state?.mounts, agent.cache?.targets, agent.cache?.files, reposQuery.data?.items])

  const filteredRows = useMemo(() => {
    const keyword = query.trim().toLowerCase()
    if (!keyword) return rows
    return rows.filter((row) =>
      [row.repoName, row.repoId, row.target?.target_iqn ?? '', row.file?.key ?? ''].some((value) =>
        value.toLowerCase().includes(keyword)
      )
    )
  }, [rows, query])

  const clearableRows = rows.filter((row) => row.target?.cache_enabled || row.file)
  const hasError = Boolean(error) || (reposQuery.isError && rows.length === 0)
  const diskL2Bytes = agent.cache?.disk_l2_bytes ?? 0
  const diskL1Bytes = agent.cache?.disk_l1_bytes ?? 0
  const l2LimitBytes = agent.cache?.l2_limit_bytes ?? 0
  const totals = cacheTotals(agent.cache, agent.state?.mounts ?? [])
  const totalLimitText = totals.limitBytes > 0 ? formatBytes(totals.limitBytes) : t('common.unlimited')

  const clearOne = async (row: CacheManageRow): Promise<void> => {
    setError(null)
    setClearingId(row.repoId)
    try {
      const result = await agentApi.clearCache({ repo_id: row.repoId })
      await agent.refreshCache()
      if (result.cleared) message.success(t('settings.cache.clearDone', { repo: row.repoName }))
      else message.info(t('settings.cache.clearNone', { repo: row.repoName }))
    } catch (err) {
      setError(err)
    } finally {
      setClearingId(null)
    }
  }

  const clearAll = async (): Promise<void> => {
    if (clearableRows.length === 0) return
    setError(null)
    setClearingAll(true)
    try {
      const result = await agentApi.clearCache({ all: true })
      await agent.refreshCache()
      if (result.cleared) message.success(t('settings.cache.clearAllDone', { count: clearableRows.length }))
      else message.info(t('settings.cache.clearAllNone'))
    } catch (err) {
      setError(err)
    } finally {
      setClearingAll(false)
    }
  }

  return (
    <Modal
      open={open}
      title={t('settings.cache.manageTitle')}
      width={860}
      centered
      maskClosable={false}
      keyboard={false}
      onCancel={onClose}
      footer={
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <Popconfirm
            title={t('settings.cache.clearAllConfirm')}
            description={t('settings.cache.clearConfirmHint')}
            okText={t('common.confirm')}
            cancelText={t('common.cancel')}
            disabled={clearableRows.length === 0}
            onConfirm={() => void clearAll()}
          >
            <Button danger loading={clearingAll} disabled={clearableRows.length === 0}>
              {t('settings.cache.clearAll')}
            </Button>
          </Popconfirm>
          <Button onClick={onClose}>{t('common.close')}</Button>
        </div>
      }
    >
      <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('settings.cache.manageHint')}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('settings.cache.diskUsage', { l2: formatBytes(diskL2Bytes), l1: formatBytes(diskL1Bytes) })}
          {' · '}
          {t('settings.cache.manageOnlyMounted')}
        </Typography.Text>
        {hasError ? <ErrorNotice error={error ?? reposQuery.error} /> : null}
        <div style={{ display: 'flex', alignItems: 'center', gap: spacing.sm }}>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs, whiteSpace: 'nowrap' }}>
            {t('settings.cache.headerUsage', {
              used: formatBytes(totals.usedBytes),
              limit: totalLimitText
            })}
          </Typography.Text>
          <UsageBar
            percent={totals.percent}
            label={totals.limitBytes > 0 ? undefined : t('common.unlimited')}
            height={10}
            style={{ flex: 1, marginTop: 0 }}
          />
        </div>
        <Input
          allowClear
          prefix={<SearchOutlined />}
          placeholder={t('settings.cache.searchPlaceholder')}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
        />
        <div
          style={{
            maxHeight: 'calc(100vh - 360px)',
            overflowY: 'auto',
            paddingRight: spacing.xxs,
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fill, minmax(250px, 1fr))',
            alignContent: 'start',
            gap: spacing.sm
          }}
        >
          {filteredRows.map((row) => {
            const canClear = Boolean(row.target?.cache_enabled || row.file)
            const usage = cacheRowUsage(row.target, row.file, l2LimitBytes)
            const percent = usage.limitBytes > 0 ? Math.min(100, (usage.usedBytes / usage.limitBytes) * 100) : 0
            return (
              <div
                key={row.key}
                style={{
                  minWidth: 0,
                  border: '1px solid #f0f0f0',
                  borderRadius: 8,
                  padding: spacing.sm,
                  display: 'flex',
                  flexDirection: 'column',
                  gap: spacing.xs
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs, minWidth: 0 }}>
                  <Typography.Text strong ellipsis={{ tooltip: row.repoName }} style={{ flex: 1, minWidth: 0 }}>
                    {row.repoName}
                  </Typography.Text>
                </div>
                <div
                  title={
                    row.target
                      ? `L1 ${formatBytes(row.target.l1_used_bytes)} / ${formatBytes(row.target.l1_limit_bytes)} · L2 ${formatBytes(row.target.l2_used_bytes)} / ${formatBytes(row.target.l2_limit_bytes)}`
                      : `L2 ${formatBytes(row.file?.l2_bytes)} / ${formatBytes(l2LimitBytes)}`
                  }
                >
                  <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                    {usage.limitBytes > 0 || usage.usedBytes > 0
                      ? `${formatBytes(usage.usedBytes)}${usage.limitBytes > 0 ? ` / ${formatBytes(usage.limitBytes)}` : ''}`
                      : '-'}
                  </Typography.Text>
                  <UsageBar
                    percent={percent}
                    muted={!canClear && !row.file}
                    height={8}
                    showPercent={false}
                    style={{ marginTop: 2 }}
                  />
                </div>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: spacing.xs }}>
                  <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                    {row.target && row.target.reads > 0
                      ? `${t('settings.cache.colHitRate')} ${(row.target.hit_rate * 100).toFixed(1)}%`
                      : ''}
                  </Typography.Text>
                  <Popconfirm
                    title={t('settings.cache.clearOneConfirm', { repo: row.repoName })}
                    description={t('settings.cache.clearConfirmHint')}
                    okText={t('common.confirm')}
                    cancelText={t('common.cancel')}
                    disabled={!canClear}
                    onConfirm={() => void clearOne(row)}
                  >
                    <Button
                      type="link"
                      size="small"
                      danger
                      loading={clearingId === row.repoId}
                      disabled={!canClear || clearingAll}
                      style={{ paddingInline: 0 }}
                    >
                      {t('settings.cache.clearOne')}
                    </Button>
                  </Popconfirm>
                </div>
              </div>
            )
          })}
        </div>
        {!reposQuery.isLoading && filteredRows.length === 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {query.trim() ? t('settings.cache.searchEmpty') : t('settings.cache.empty')}
          </Typography.Text>
        ) : null}
      </Space>
    </Modal>
  )
}
