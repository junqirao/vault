import { useMemo, useState } from 'react'
import { Button, Modal, Popconfirm, Progress, Select, Typography, message } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { JobDTO, JobState } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { usePermission } from '../../hooks/useAuth'
import { isJobTerminal } from '../../hooks/useJobProgress'
import { useI18n } from '../../i18n'
import { formatTime } from '../../utils/format'
import { jobStateLabel, jobTypeLabel } from '../../utils/labels'

const JOB_STATES: JobState[] = ['pending', 'running', 'succeeded', 'failed', 'cancelled']

/** 任务中心：异步任务队列与进度。 */
export function JobList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { isSuperAdmin } = usePermission()
  const queryClient = useQueryClient()
  const agent = useAgent()
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
        render: (value: string) => <span style={{ fontSize: 12 }}>{value}</span>
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
    [t, isSuperAdmin, cancelMutation]
  )

  return (
    <PageShell
      title={t('page.jobs.title')}
      extra={
        <>
          <Select<JobState>
            allowClear
            placeholder={t('job.filter.state')}
            style={{ width: 180 }}
            value={state}
            onChange={setState}
            options={JOB_STATES.map((value) => ({ value, label: jobStateLabel(value) }))}
          />
          <Button onClick={() => void jobsQuery.refetch()}>{t('common.refresh')}</Button>
        </>
      }
    >
      {jobsQuery.error ? <ErrorNotice error={jobsQuery.error} /> : null}
      <DataTable<JobDTO>
        columns={columns}
        rows={rows}
        rowKey={(job) => job.id}
        loading={jobsQuery.isLoading}
        empty={<EmptyState title={t('job.empty')} />}
        scroll={{ x: 1340 }}
      />

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
