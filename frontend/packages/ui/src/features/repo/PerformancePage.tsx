import { useEffect, useMemo, useState } from 'react'
import { Button, Col, Collapse, Empty, Row, Typography } from 'antd'
import { ArrowLeftOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'

import { agentApi } from '../../api/agentClient'
import type { AgentCacheHistory, AgentMountState } from '../../api/agentTypes'
import { useApi } from '../../api/provider'
import { PageShell } from '../../components/PageShell'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { PerformanceCharts, usePerformanceHistory } from './PerformanceMonitor'
import { RepoPerformanceView } from './RepoPerformanceView'

export interface PerformancePageProps {
  onBack: () => void
}

/**
 * 按库拉取缓存/IOPS 采样。
 *
 * 一次取全量、再按分配 ID 分发：存储库数量多时，逐库各开一个 5 秒轮询会把本地代理打满。
 * 后端采样间隔也是 5 秒，所以这里与它对齐即可。
 */
function useCacheHistory(): AgentCacheHistory | null {
  const agent = useAgent()
  const [history, setHistory] = useState<AgentCacheHistory | null>(null)

  useEffect(() => {
    if (!agent.available) {
      setHistory(null)
      return
    }
    let stopped = false
    const load = async (): Promise<void> => {
      try {
        const next = await agentApi.getCacheHistory()
        if (!stopped) setHistory(next)
      } catch {
        // 保留上一份数据；性能页不该因为一次轮询失败整页抖动。
      }
    }
    void load()
    const timer = window.setInterval(() => void load(), 5000)
    return () => {
      stopped = true
      window.clearInterval(timer)
    }
  }, [agent.available])

  return history
}

/**
 * 性能详情页：三个可折叠模块。
 *
 * 1. 总览：原来的磁盘 / 缓存两张全局曲线；
 * 2. 已挂载的存储库：每个库一张独立视图；
 * 3. 未挂载的存储库：默认收起，展开后同样是每库一张视图（没有数据时说明原因）。
 */
export function PerformancePage({ onBack }: PerformancePageProps): JSX.Element {
  const { t } = useI18n()
  const api = useApi()
  const agent = useAgent()
  const performance = usePerformanceHistory()
  const cacheHistory = useCacheHistory()

  // 性能页看的是"本机实际挂载情况"：同一个库只取一条挂载记录（正常也只有一条）。
  const mountByRepo = useMemo(() => {
    const out = new Map<string, AgentMountState>()
    for (const mount of agent.state?.mounts ?? []) {
      if (!out.has(mount.repo_id)) out.set(mount.repo_id, mount)
    }
    return out
  }, [agent.state?.mounts])

  const reposQuery = useQuery({
    queryKey: ['repos', 'performance'],
    queryFn: () => api.listRepos({ limit: 200 })
  })

  const repos = reposQuery.data?.items ?? []
  const mounted = repos.filter((repo) => mountByRepo.has(repo.id))
  const unmounted = repos.filter((repo) => !mountByRepo.has(repo.id))

  const renderRepo = (repoId: string): JSX.Element => {
    const repo = repos.find((item) => item.id === repoId)
    const mount = mountByRepo.get(repoId)
    const samples = mount ? (cacheHistory?.targets[mount.allocation_id] ?? []) : []
    return (
      <RepoPerformanceView
        key={repoId}
        repoName={repo?.name ?? mount?.repo_name ?? repoId}
        detail={mount?.mount_path}
        allocationId={mount?.allocation_id}
        cacheEnabled={Boolean(mount?.cache_enabled)}
        directMode={Boolean(mount?.direct_mode)}
        samples={samples}
      />
    )
  }

  const renderRepoGrid = (entries: string[], emptyText: string): JSX.Element => {
    if (reposQuery.isLoading) {
      return (
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('common.loading')}
        </Typography.Text>
      )
    }
    if (entries.length === 0) {
      return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={emptyText} />
    }
    return (
      <Row gutter={[spacing.md, spacing.md]}>
        {entries.map((repoId) => (
          <Col key={repoId} xs={24} xl={12}>
            {renderRepo(repoId)}
          </Col>
        ))}
      </Row>
    )
  }

  const sections = [
    {
      key: 'overview',
      label: t('repo.performance.detail.overview'),
      children: <PerformanceCharts data={performance} height={220} />
    },
    {
      key: 'mounted',
      label: `${t('repo.performance.detail.mounted')} (${mounted.length})`,
      children: renderRepoGrid(
        mounted.map((repo) => repo.id),
        t('repo.performance.detail.mountedEmpty')
      )
    },
    {
      key: 'unmounted',
      label: `${t('repo.performance.detail.unmounted')} (${unmounted.length})`,
      children: renderRepoGrid(
        unmounted.map((repo) => repo.id),
        t('repo.performance.detail.unmountedEmpty')
      )
    }
  ]

  return (
    <PageShell
      title={t('repo.performance.detail.title')}
      leading={
        <Button icon={<ArrowLeftOutlined />} onClick={onBack}>
          {t('repo.detail.back')}
        </Button>
      }
      subtitle={t('repo.performance.proxyHint')}
    >
      <Collapse defaultActiveKey={['overview', 'mounted']} items={sections} />
    </PageShell>
  )
}
