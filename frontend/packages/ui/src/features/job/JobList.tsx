import { useCallback, useMemo, useState } from 'react'
import { Button, Modal, Popconfirm, Progress, Select, Space, Typography, message } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { JobDTO, JobState } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { usePermission } from '../../hooks/useAuth'
import { isJobTerminal } from '../../hooks/useJobProgress'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'
import { jobStateLabel, jobTypeLabel } from '../../utils/labels'

const JOB_STATES: JobState[] = ['pending', 'running', 'succeeded', 'failed', 'cancelled']

/**
 * 关联对象是磁盘 ID 的任务类型。
 *
 * 服务端各类任务的 ref_id 语义并不统一（磁盘 / 存储库 / 分配 / 上传…），这里按任务类型
 * 分类解析：能解析出存储库的磁盘任务，最终仍链到所属存储库详情页。
 */
const DISK_JOB_TYPES = new Set([
  'create_vhdx',
  'create_vhdx_from_dir',
  'copy_vhdx',
  'create_diff',
  'compact',
  'delete_disk',
  'prepare_repo',
  'reset_diff'
])

/** 关联对象直接是存储库 ID 的任务类型。 */
const REPO_JOB_TYPES = new Set([
  'move_repo',
  'cleanup_diffs',
  'parent_teardown',
  'verify_diff',
  'reconcile',
  'reclaim'
])

interface JobRefInfo {
  label: string
  /** 可跳转的路径；为空表示这个关联对象没有可进入的页面。 */
  to?: string
}

/** 取路径最后一段（磁盘路径用）；兼容 Windows 反斜杠与 POSIX 斜杠。 */
function baseName(path: string): string {
  const index = Math.max(path.lastIndexOf('\\'), path.lastIndexOf('/'))
  return index >= 0 ? path.slice(index + 1) : path
}

/** 任务中心：异步任务队列与进度。 */
export function JobList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { isSuperAdmin } = usePermission()
  const queryClient = useQueryClient()
  const agent = useAgent()
  const navigate = useNavigate()
  const [state, setState] = useState<JobState | undefined>(undefined)
  // 错误信息详情：表格里只展示截断后的一行，点击后弹窗看完整文本。
  const [errorDetail, setErrorDetail] = useState<{ job: JobDTO; text: string } | null>(null)

  const jobsQuery = useQuery({
    queryKey: ['jobs', state ?? 'all'],
    queryFn: () => api.listJobs({ state, limit: 200 })
    // 不再 refetchInterval：任务状态/进度由 SSE `job` 事件持续推来（见下面的 rows 合并）。
  })

  // 把 SSE 的最新状态合并进列表：进度条与状态标签随事件走，列表本身只在进入页面/手点刷新时查一次。
  const rows = useMemo(
    () =>
      (jobsQuery.data?.items ?? []).map((job) => {
        // 列表里的终态比事件旧快照可信：事件可能丢终态、也可能晚到。已终态的任务不再被
        // 旧 running 事件覆盖；非终态才用 SSE 的实时进度。
        if (isJobTerminal(job.state)) return job
        const live = agent.jobs[job.id]
        if (!live) return job
        return {
          ...job,
          state: live.state,
          progress: live.progress,
          failed: live.state === 'failed' || job.failed,
          last_error: live.last_error ?? job.last_error
        }
      }),
    [jobsQuery.data, agent.jobs]
  )

  // 关联对象名称：存储库 / 存储 / 磁盘各拉一次全量做 ID → 名称映射。
  // 磁盘不能逐条 GET /v1/disks/{id}：历史任务里的磁盘可能早已删除，会每次 404 并在
  // 服务端日志刷一条 WARN（真实反馈的刷屏来源），因此管理端走全量列表接口。
  const reposQuery = useQuery({
    queryKey: ['repos', 'all'],
    queryFn: () => api.listRepos({ limit: 500 })
  })
  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages()
  })
  const disksQuery = useQuery({
    queryKey: ['disks', 'all'],
    queryFn: () => api.listDisks()
  })

  const repoNameByID = useMemo(() => {
    const map = new Map<string, string>()
    for (const repo of reposQuery.data?.items ?? []) map.set(repo.id, repo.name)
    return map
  }, [reposQuery.data])

  const storageNameByID = useMemo(() => {
    const map = new Map<string, string>()
    for (const storage of storagesQuery.data?.items ?? []) map.set(storage.id, storage.name)
    return map
  }, [storagesQuery.data])

  const diskByID = useMemo(() => {
    const map = new Map<string, { repoID: string; name: string }>()
    for (const disk of disksQuery.data?.items ?? []) {
      map.set(disk.id, { repoID: disk.repo_id, name: baseName(disk.vhdx_path) })
    }
    return map
  }, [disksQuery.data])

  /**
   * 任务关联对象 → 「类型-名称」+ 可跳转路径。
   *
   * 名称优先用服务端真实名称（存储库名 / 存储名 / 磁盘所属库名），解析不到时才退回短 ID：
   * 只显示 ID 的话，用户根本不知道这条任务在动哪个库（真实反馈）。
   */
  const resolveJobRef = useCallback(
    (job: JobDTO): JobRefInfo => {
      const ref = job.ref_id
      if (!ref) return { label: '-' }
      const shortId = ref.length > 12 ? `${ref.slice(0, 8)}…` : ref

      // 磁盘任务：解析出所属存储库后仍然链到存储库详情（磁盘没有独立详情页）。
      if (DISK_JOB_TYPES.has(job.type)) {
        const disk = diskByID.get(ref)
        const repoID = disk?.repoID
        const repoName = repoID ? repoNameByID.get(repoID) : undefined
        if (repoID && repoName) {
          return { label: t('job.ref.repo', { name: repoName }), to: `/repos/${repoID}` }
        }
        return { label: t('job.ref.disk', { id: disk?.name || shortId }) }
      }

      // 存储库任务：任务中心里最常见的对象，直接显示库名并可进入详情。
      if (REPO_JOB_TYPES.has(job.type)) {
        const name = repoNameByID.get(ref)
        return name
          ? { label: t('job.ref.repo', { name }), to: `/repos/${ref}` }
          : { label: t('job.ref.repo', { name: shortId }) }
      }

      if (job.type === 'publish' || job.type === 'unpublish') {
        return { label: t('job.ref.allocation', { id: shortId }) }
      }
      if (job.type === 'repair_target') {
        return { label: t('job.ref.target', { id: shortId }) }
      }
      if (job.type === 'upload_complete') {
        return { label: t('job.ref.upload', { id: shortId }) }
      }
      if (job.type === 'install_deps') {
        return { label: t('job.ref.deps', { name: ref }), to: '/admin/system' }
      }

      // 兜底：按已知 ID 集合认一认，认不出再显示短 ID，绝不把裸 ID 当唯一信息。
      const repoName = repoNameByID.get(ref)
      if (repoName) return { label: t('job.ref.repo', { name: repoName }), to: `/repos/${ref}` }
      const storageName = storageNameByID.get(ref)
      if (storageName) return { label: t('job.ref.storage', { name: storageName }), to: '/admin/storages' }
      return { label: t('job.ref.unknown', { id: shortId }) }
    },
    [diskByID, repoNameByID, storageNameByID, t]
  )

  // 取消任务：卡住的发布/拆除任务没有别的出口（真实诉求："任务一直卡着，给个取消按钮"）。
  const cancelMutation = useMutation({
    mutationFn: (id: string) => api.cancelJob(id),
    onSuccess: () => {
      message.success(t('job.cancelDone'))
      void queryClient.invalidateQueries({ queryKey: ['jobs'] })
    },
    onError: (err) => message.error(translateError(err) || t('job.cancelFailed'))
  })

  const columns = useMemo<TableColumnsType<JobDTO>>(
    () => [
      { title: t('field.type'), dataIndex: 'type', key: 'type', width: 200, render: (value: string) => jobTypeLabel(value) },
      {
        title: t('field.refId'),
        dataIndex: 'ref_id',
        key: 'ref_id',
        ellipsis: true,
        render: (_value, job) => {
          const info = resolveJobRef(job)
          return info.to ? (
            <Typography.Link
              style={{ fontSize: 12 }}
              onClick={() => navigate(info.to!, { state: { from: '/admin/jobs' } })}
            >
              {info.label}
            </Typography.Link>
          ) : (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {info.label}
            </Typography.Text>
          )
        }
      },
      {
        title: t('common.status'),
        dataIndex: 'state',
        key: 'state',
        width: 120,
        render: (value: string) => <StatusTag group="job" value={value} />
      },
      {
        title: t('field.progress'),
        dataIndex: 'progress',
        key: 'progress',
        width: 180,
        render: (value: number, job) => (
          <Progress percent={value} size="small" status={job.failed ? 'exception' : job.state === 'succeeded' ? 'success' : 'active'} />
        )
      },
      { title: t('job.field.attempt'), dataIndex: 'attempt', key: 'attempt', width: 100 },
      { title: t('common.createdAt'), dataIndex: 'created_at', key: 'created_at', width: 170, render: (value: number) => formatTime(value) },
      { title: t('common.updatedAt'), dataIndex: 'finished_at', key: 'finished_at', width: 170, render: (value: number) => formatTime(value) },
      {
        // 失败原因（稳定错误码）直接列出来：否则用户只能看到"失败"却不知道卡在哪，
        // 又没法翻服务端日志（真实反馈："看不到任何日志"）。
        //
        // 列宽必须锁死、正文必须自己截断：错误码/底层报错可能很长，不截断会把前面的列挤变形。
        // 点击整段文字打开详情弹窗，完整内容可滚动、可复制。
        title: t('job.field.error'),
        dataIndex: 'last_error',
        key: 'last_error',
        width: 220,
        ellipsis: true,
        render: (value: string | undefined, job) =>
          value ? (
            <Typography.Link
              type="danger"
              onClick={() => setErrorDetail({ job, text: value })}
              style={{
                display: 'block',
                maxWidth: '100%',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
                fontSize: 12
              }}
            >
              {value}
            </Typography.Link>
          ) : null
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 110,
        render: (_, job) =>
          // 只有超管能取消（服务端也仅超管放行）；终态任务无需取消。
          isSuperAdmin && !isJobTerminal(job.state) ? (
            <Popconfirm
              title={t('job.cancelConfirm')}
              okText={t('common.confirm')}
              cancelText={t('common.cancel')}
              onConfirm={() => cancelMutation.mutate(job.id)}
            >
              <Button size="small" danger loading={cancelMutation.isPending}>
                {t('job.cancel')}
              </Button>
            </Popconfirm>
          ) : null
      }
    ],
    [t, isSuperAdmin, cancelMutation, resolveJobRef, navigate]
  )

  return (
    <PageShell
      title={t('page.jobs.title')}
      subtitle={t('page.jobs.subtitle')}
      extra={<Button onClick={() => void jobsQuery.refetch()}>{t('common.refresh')}</Button>}
    >
      {jobsQuery.error ? <ErrorNotice error={jobsQuery.error} /> : null}
      <SectionCard title={t('common.filters')}>
        <Space wrap size={spacing.sm}>
          <Select<JobState>
            allowClear
            placeholder={t('job.filter.state')}
            style={{ width: 180 }}
            value={state}
            onChange={setState}
            options={JOB_STATES.map((value) => ({ value, label: jobStateLabel(value) }))}
          />
          <Button onClick={() => setState(undefined)}>{t('common.reset')}</Button>
        </Space>
      </SectionCard>
      <SectionCard
        title={t('job.list.title')}
        extra={
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('common.total', { total: rows.length })}
          </Typography.Text>
        }
      >
        <DataTable<JobDTO>
          columns={columns}
          rows={rows}
          rowKey={(job) => job.id}
          loading={jobsQuery.isLoading}
          empty={<EmptyState title={t('job.empty')} />}
          scroll={{ x: 1340 }}
        />
      </SectionCard>

      <Modal
        open={errorDetail !== null}
        title={t('job.errorDetail.title')}
        width={760}
        centered
        onCancel={() => setErrorDetail(null)}
        footer={<Button onClick={() => setErrorDetail(null)}>{t('common.close')}</Button>}
      >
        {errorDetail ? (
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {`${jobTypeLabel(errorDetail.job.type)} · ${errorDetail.job.id}`}
            </Typography.Text>
            <Typography.Paragraph
              copyable={{ text: errorDetail.text }}
              style={{
                marginTop: 8,
                marginBottom: 0,
                maxHeight: '50vh',
                overflow: 'auto',
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-word'
              }}
            >
              {errorDetail.text}
            </Typography.Paragraph>
          </div>
        ) : null}
      </Modal>
    </PageShell>
  )
}
