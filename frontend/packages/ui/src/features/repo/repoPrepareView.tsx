import { Progress, Space, Typography } from 'antd'
import type { CSSProperties } from 'react'
import { useQuery } from '@tanstack/react-query'

import { useApi } from '../../api/provider'
import type { RepoDTO, RepoPrepareDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes, formatSpeed } from '../../utils/format'
import { uploadEtaText } from './uploadView'

/**
 * 建库/导入进度百分比。
 *
 * 母盘阶段如果有字节总量（从服务端目录导入），按字节算 —— 阶段计数永远是 0/1，
 * 拿它画进度条会让 100GB 的拷贝几小时不动。其余阶段（派生差异盘 / 下发目标）
 * 没有字节概念，仍按 Done/Total 算。
 */
export function preparePercent(prepare: RepoPrepareDTO | undefined): number {
  if (!prepare) return 0
  const totalBytes = prepare.bytes_total ?? 0
  if (prepare.phase === 'parent' && totalBytes > 0) {
    return Math.min(100, Math.round(((prepare.bytes_done ?? 0) / totalBytes) * 100))
  }
  if (prepare.total > 0) {
    return Math.min(100, Math.round((prepare.done / prepare.total) * 100))
  }
  return 0
}

/**
 * 进度条右侧的数值：拷贝阶段是「已写入 / 总量」，其余阶段是「已完成 / 总数」。
 *
 * 卡片与详情页共用：同一份快照在两处必须显示同一句话，否则用户会以为其中一处卡住了。
 */
export function prepareValue(prepare: RepoPrepareDTO): string {
  const totalBytes = prepare.bytes_total ?? 0
  if (prepare.phase === 'parent' && totalBytes > 0) {
    return `${formatBytes(prepare.bytes_done ?? 0)} / ${formatBytes(totalBytes)}`
  }
  return `${prepare.done}/${prepare.total}`
}

/**
 * 建库进度的明细（进度条 + 阶段 + 字节/计数 + 速度 + 预计剩余）。
 *
 * 详情页横幅与新建页的导入面板共用。速度与剩余只在"母盘拷贝且尚未失败"时显示：
 * 派生差异盘/下发目标没有连续的字节流，报速度只会是噪音。
 */
export function RepoPrepareDetail({
  prepare,
  style
}: {
  prepare?: RepoPrepareDTO
  style?: CSSProperties
}): JSX.Element | null {
  const { t } = useI18n()
  if (!prepare) return null
  const copying = prepare.phase === 'parent' && (prepare.bytes_total ?? 0) > 0
  const failed = Boolean(prepare.error)

  return (
    <Space direction="vertical" size={spacing.xs} style={{ width: '100%', ...style }}>
      <Progress percent={preparePercent(prepare)} status={failed ? 'exception' : 'active'} />
      <Space size={spacing.md} wrap>
        <Typography.Text type="secondary">{t(`repo.prepare.${prepare.phase}`)}</Typography.Text>
        <Typography.Text type="secondary">{prepareValue(prepare)}</Typography.Text>
        {copying && !failed ? (
          <Typography.Text type="secondary">
            {`${t('upload.speed')} ${formatSpeed(prepare.speed_bytes_per_sec)}`}
          </Typography.Text>
        ) : null}
        {copying && !failed ? (
          <Typography.Text type="secondary">
            {`${t('upload.eta')} ${uploadEtaText(t, prepare.eta_seconds)}`}
          </Typography.Text>
        ) : null}
      </Space>
    </Space>
  )
}

/**
 * 跟踪一个正在建/导入的存储库：轮询详情，并把本地事件流里的 prepare 快照合进来。
 *
 * 为什么轮询而不是只靠事件：SSE 可能丢事件/重连，而建库是分钟到小时级的长活；
 * 2 秒一次的详情查询成本很低，却能让离开页面再回来的人立刻接上进度。
 * 库一旦离开 creating/importing，轮询自动停止。
 */
export function useRepoPrepare(
  repoId: string | null | undefined,
  initial?: RepoDTO
): RepoDTO | undefined {
  const api = useApi()
  const agent = useAgent()
  const query = useQuery({
    queryKey: ['repo', repoId],
    queryFn: () => api.getRepo(repoId as string),
    enabled: Boolean(repoId),
    initialData: initial,
    refetchInterval: (current) => {
      const state = current.state.data?.state
      return state === 'creating' || state === 'importing' ? 2000 : false
    }
  })

  const repo = query.data
  if (!repo || !repoId) return repo
  const live = agent.repoEvents[repoId]
  if (live?.action === 'prepare_progress' && live.prepare) {
    return {
      ...repo,
      state: (live.state as RepoDTO['state'] | undefined) ?? repo.state,
      prepare: live.prepare
    }
  }
  return repo
}
