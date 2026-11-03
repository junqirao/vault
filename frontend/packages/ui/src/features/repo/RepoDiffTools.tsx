import { useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert,
  Button,
  Checkbox,
  Divider,
  Input,
  Modal,
  Progress,
  Radio,
  Segmented,
  Select,
  Space,
  Table,
  Tag,
  Typography,
  message
} from 'antd'
import type { TableColumnsType } from 'antd'

import { agentApi, selectDirectory, supportsDirectoryPicker } from '../../api/agentClient'
import type { AgentDiffSyncStatus } from '../../api/agentTypes'
import { useApi } from '../../api/provider'
import type { DiffSyncEntryDTO, DiffSyncMode, DiffSyncResultDTO, DiskDTO, JobDTO, RepoDTO } from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { isJobTerminal, useJobProgress } from '../../hooks/useJobProgress'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'
import { ServerDirBrowser } from './SourceDirPicker'

const { Paragraph, Text, Title } = Typography

/** 差异盘工具：从目录创建差异盘 + 维护态合并差异盘更新母盘。 */
export function RepoDiffTools({
  repo,
  disks,
  canManage,
  isMaintenance,
  onRefresh,
  onBusyChange
}: {
  repo: RepoDTO
  disks: DiskDTO[]
  canManage: boolean
  isMaintenance: boolean
  onRefresh: () => void
  onBusyChange?: (busy: boolean) => void
}): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const { active } = useServerConfig()

  const [sourceKind, setSourceKind] = useState<'server' | 'client'>('server')
  const [sourceDir, setSourceDir] = useState('')
  const [clientDir, setClientDir] = useState('')
  const [browserOpen, setBrowserOpen] = useState(false)
  const [clientTaskID, setClientTaskID] = useState('')
  const [clientStatus, setClientStatus] = useState<AgentDiffSyncStatus | null>(null)
  const [mode, setMode] = useState<DiffSyncMode>('auto')
  const [deleteExtra, setDeleteExtra] = useState(true)
  const [createJob, setCreateJob] = useState<JobDTO | null>(null)
  const [createResult, setCreateResult] = useState<DiffSyncResultDTO | null>(null)
  // 最近一次从目录创建出的差异盘：打开合并弹窗时默认选中它，省掉一次手找。
  const [lastCreatedDiffID, setLastCreatedDiffID] = useState('')
  const [error, setError] = useState<unknown>(null)
  const resultFetchedFor = useRef('')

  const [mergeOpen, setMergeOpen] = useState(false)
  const [selectedDiff, setSelectedDiff] = useState('')
  const [mergeJob, setMergeJob] = useState<JobDTO | null>(null)
  // 从服务端恢复的、由其他页面/上次进入发起的管理任务（plan_client_diff 等）。
  const [activeJob, setActiveJob] = useState<JobDTO | null>(null)

  const diffs = useMemo(() => (disks ?? []).filter((disk) => disk.kind === 'diff'), [disks])
  const canCreate = repo.state === 'active' && isMaintenance

  const selected = diffs.find((disk) => disk.id === selectedDiff)
  const otherCount = Math.max(0, diffs.length - (selected ? 1 : 0))

  const createProgress = useJobProgress(createJob?.id, createJob)
  const clientRunning = clientTaskID !== '' && (!clientStatus || clientStatus.state === 'running')
  const createRunning = clientRunning || (createJob !== null && !isJobTerminal(createProgress?.state))
  const mergeProgress = useJobProgress(mergeJob?.id, mergeJob)
  const mergeRunning = mergeJob !== null && !isJobTerminal(mergeProgress?.state)
  const activeProgress = useJobProgress(activeJob?.id, activeJob)
  const activeJobRunning = activeJob !== null && !isJobTerminal(activeProgress?.state)
  const busy = clientRunning || createRunning || mergeRunning || activeJobRunning

  // 恢复：页面重进时从服务端找仍在执行的差异盘工具任务；代理侧则找本机任务。
  useEffect(() => {
    let cancelled = false
    void api
      .getActiveDiffOperation(repo.id)
      .then((res) => {
        if (cancelled || !res.job) return
        if (res.job.type === 'merge_diff') {
          setMergeJob((prev) => prev ?? res.job!)
        } else if (res.job.type === 'create_diff_from_dir') {
          setCreateJob((prev) => prev ?? res.job!)
        } else {
          setActiveJob((prev) => prev ?? res.job!)
        }
      })
      .catch(() => undefined)
    void agentApi
      .listDiffSync(repo.id)
      .then((res) => {
        if (cancelled) return
        const items = res.items ?? []
        const running = items.find((item) => item.state === 'running')
        const latest = running ?? items[0]
        if (!latest) return
        setSourceKind('client')
        setClientDir(latest.local_dir ?? '')
        if (latest.mode === 'auto' || latest.mode === 'hash' || latest.mode === 'size') {
          setMode(latest.mode)
        }
        setDeleteExtra(latest.delete_extra !== false)
        setClientStatus(latest)
        if (latest.state === 'running') {
          setClientTaskID(latest.diff_sync_id)
        } else if (latest.state === 'succeeded') {
          // 页面离开期间已经完成：列表只带摘要，按 ID 取一次完整结果。
          void agentApi
            .getDiffSync(latest.diff_sync_id)
            .then((full) => {
              if (cancelled || !full.result) return
              setClientStatus(full)
              setCreateResult({
                ...full.result,
                warnings: full.warnings?.length ? full.warnings : full.result.warnings
              })
              setLastCreatedDiffID(full.result.diff_disk_id)
            })
            .catch(() => undefined)
        }
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
    // 只在切换存储库时恢复一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo.id])

  // plan_client_diff 这类没有专用轮询的任务：终态后刷新并清掉。
  useEffect(() => {
    if (!activeJob || !isJobTerminal(activeProgress?.state)) return
    setActiveJob(null)
    onRefresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeJob, activeProgress?.state])

  useEffect(() => {
    onBusyChange?.(busy)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [busy])

  // 组件卸载（切走详情页/切页签）时解除父级锁定标记。
  useEffect(() => {
    return () => onBusyChange?.(false)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 客户端目录任务：代理侧是异步任务，这里每 800ms 取一次状态；终态再刷新磁盘列表。
  useEffect(() => {
    if (!clientTaskID) return
    let cancelled = false
    let timer: number | undefined
    const tick = async (): Promise<void> => {
      try {
        const status = await agentApi.getDiffSync(clientTaskID)
        if (cancelled) return
        setClientStatus(status)
        if (status.state === 'succeeded') {
          // 把代理侧扫描/哈希阶段的告警并进最终结果（服务端结果里只有它自己那部分）。
          const result = status.result
            ? { ...status.result, warnings: status.warnings?.length ? status.warnings : status.result.warnings }
            : null
          setCreateResult(result)
          setLastCreatedDiffID(status.result?.diff_disk_id ?? '')
          message.success(t('repo.diffTools.createDone'))
          onRefresh()
          setClientTaskID('')
          return
        }
        if (status.state === 'failed' || status.state === 'cancelled') {
          if (status.state === 'cancelled') {
            message.info(t('repo.diffTools.createCancelled'))
          } else if (status.error_code) {
            setError(new Error(status.error_message || status.error_code))
          } else {
            message.error(t('repo.diffTools.createFailed'))
          }
          setClientTaskID('')
          return
        }
        timer = window.setTimeout(() => void tick(), 800)
      } catch (err) {
        if (cancelled) return
        setError(err)
        setClientTaskID('')
      }
    }
    void tick()
    return () => {
      cancelled = true
      if (timer) window.clearTimeout(timer)
    }
    // 只在任务 ID 变化时启动轮询；t/onRefresh 是只读引用。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clientTaskID])

  // 创建任务结束：成功则取回改动清单，失败则提示。结果清单只在终态取一次。
  useEffect(() => {
    if (!createJob || !isJobTerminal(createProgress?.state)) return
    if (resultFetchedFor.current === createJob.id) return
    resultFetchedFor.current = createJob.id
    if (createProgress?.state !== 'succeeded') {
      message.error(t('repo.diffTools.createFailed'))
      setCreateJob(null)
      return
    }
    void api
      .getDiffFromDirResult(repo.id, createJob.id)
      .then((result) => {
        setCreateResult(result)
        setLastCreatedDiffID(result.diff_disk_id)
        message.success(t('repo.diffTools.createDone'))
        onRefresh()
      })
      .catch((err) => setError(err))
      .finally(() => setCreateJob(null))
    // api / repo.id / t / onRefresh 都是稳定或只读引用；这里只跟随任务终态。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [createJob, createProgress?.state])

  // 合并任务结束：成功会退出维护并 +1 版本，刷新详情；失败保留弹窗让用户看到原因。
  useEffect(() => {
    if (!mergeJob || !isJobTerminal(mergeProgress?.state)) return
    if (mergeProgress?.state === 'succeeded') {
      message.success(t('repo.diffTools.mergeDone'))
      setMergeOpen(false)
      setSelectedDiff('')
      onRefresh()
    } else {
      message.error(t('repo.diffTools.mergeFailed'))
    }
    setMergeJob(null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mergeJob, mergeProgress?.state])

  if (!canManage || repo.mode !== 'shared') return null

  const startCreate = (): void => {
    setError(null)
    setCreateResult(null)
    if (sourceKind === 'client') {
      if (!clientDir.trim()) {
        message.warning(t('repo.diffTools.needDir'))
        return
      }
      setClientStatus(null)
      void agentApi
        .startDiffSync({
          repo_id: repo.id,
          local_dir: clientDir.trim(),
          mode,
          delete_extra: deleteExtra,
          server_key: active?.key
        })
        .then((started) => {
          setClientStatus(started.status)
          setClientTaskID(started.diff_sync_id)
          message.info(t('repo.diffTools.createStarted'))
        })
        .catch((err) => setError(err))
      return
    }
    if (!sourceDir.trim()) {
      message.warning(t('repo.diffTools.needDir'))
      return
    }
    void api
      .startDiffFromDir(repo.id, { source_dir: sourceDir.trim(), mode, delete_extra: deleteExtra })
      .then((job) => {
        setCreateJob(job)
        message.info(t('repo.diffTools.createStarted'))
      })
      .catch((err) => setError(err))
  }

  const pickClientDir = (): void => {
    void selectDirectory()
      .then((selection) => {
        if (selection && !selection.canceled && selection.path) setClientDir(selection.path)
      })
      .catch((err) => setError(err))
  }

  const clientPercent = (() => {
    if (!clientStatus) return 0
    switch (clientStatus.phase) {
      case 'scan':
        return 8
      case 'hash':
        return 25
      case 'plan':
        return 55
      case 'upload':
        if (clientStatus.total_bytes > 0) {
          return 60 + Math.min(35, Math.round((clientStatus.bytes / clientStatus.total_bytes) * 35))
        }
        return 70
      default:
        return clientStatus.state === 'succeeded' ? 100 : 0
    }
  })()

  const busyJob = activeJob ?? mergeJob ?? createJob
  const busyPercent = clientTaskID ? clientPercent : (busyJob ? (busyJob.type === 'merge_diff' ? (mergeProgress?.progress ?? busyJob.progress) : busyJob.type === 'create_diff_from_dir' ? (createProgress?.progress ?? busyJob.progress) : (activeProgress?.progress ?? busyJob.progress)) : 0)
  const busyLabel = clientTaskID
    ? t(`repo.diffTools.phase.${clientStatus?.phase ?? 'scan'}`)
    : busyJob
      ? t(`state.jobType.${busyJob.type}`)
      : ''

  const cancelBusy = (): void => {
    if (clientTaskID) {
      // 代理侧停扫描/上传；服务端 plan 任务可能还在跑，也顺手取消掉（尽力而为）。
      void agentApi.cancelDiffSync(clientTaskID).catch((err) => setError(err))
      void api.cancelDiffOperation(repo.id).catch(() => undefined)
      return
    }
    void api
      .cancelDiffOperation(repo.id)
      .then(() => {
        setActiveJob(null)
        setCreateJob(null)
        setMergeJob(null)
        onRefresh()
      })
      .catch((err) => setError(err))
  }

  const startMerge = (): void => {
    setError(null)
    if (!selected) {
      message.warning(t('repo.diffTools.needDiff'))
      return
    }
    void api
      .mergeDiff(repo.id, selected.id)
      .then((job) => {
        setMergeJob(job)
        // 合并开始后收进任务面板：离开页面也能从服务端恢复，不再依赖弹窗。
        setMergeOpen(false)
      })
      .catch((err) => setError(err))
  }

  const columns: TableColumnsType<DiffSyncEntryDTO> = [
    {
      title: t('repo.diffTools.col.kind'),
      dataIndex: 'kind',
      width: 90,
      render: (kind: string) => (
        <Tag color={kind === 'add' ? 'green' : kind === 'delete' ? 'red' : 'gold'}>
          {t(`repo.diffTools.kind.${kind}`)}
        </Tag>
      )
    },
    { title: t('repo.diffTools.col.path'), dataIndex: 'path', ellipsis: true },
    {
      title: t('repo.diffTools.col.size'),
      dataIndex: 'size',
      width: 110,
      render: (size?: number) => (size ? formatBytes(size) : '-')
    },
    {
      title: t('repo.diffTools.col.reason'),
      dataIndex: 'reason',
      width: 120,
      render: (reason?: string) => (reason ? t(`repo.diffTools.reason.${reason}`) : '-')
    }
  ]

  return (
    <SectionCard title={t('repo.diffTools.title')}>
      <Paragraph type="secondary" style={{ marginTop: 0 }}>
        {t('repo.diffTools.desc')}
      </Paragraph>
      {error ? <ErrorNotice error={error} /> : null}

      {busy ? (
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <div>
            <Text strong>{t('repo.diffTools.busy.title')}</Text>
            <Paragraph type="secondary" style={{ margin: `${spacing.xs}px 0 0` }}>
              {t('repo.diffTools.busy.desc')}
            </Paragraph>
          </div>
          <Space wrap>
            <Tag color="processing">{busyLabel}</Tag>
            {clientStatus?.current ? (
              <Text type="secondary" ellipsis style={{ maxWidth: 460 }}>
                {clientStatus.current}
              </Text>
            ) : null}
          </Space>
          <Progress percent={busyPercent} status="active" />
          <Space wrap size={spacing.md}>
            {clientTaskID && clientStatus?.total ? (
              <Text type="secondary">
                {t('repo.diffTools.busy.files', { done: clientStatus.done, total: clientStatus.total })}
              </Text>
            ) : null}
            {clientTaskID && clientStatus?.total_bytes ? (
              <Text type="secondary">
                {t('repo.diffTools.busy.bytes', {
                  done: formatBytes(clientStatus.bytes),
                  total: formatBytes(clientStatus.total_bytes)
                })}
              </Text>
            ) : null}
            <Button size="small" onClick={cancelBusy}>
              {t('common.cancel')}
            </Button>
          </Space>
          {clientStatus?.warnings?.length ? (
            <Text type="warning">
              {t('repo.diffTools.create.warnings', { count: clientStatus.warnings.length })}
            </Text>
          ) : null}
        </Space>
      ) : (
        <>
      <Divider />
      <Title level={5} style={{ marginTop: 0 }}>
        {t('repo.diffTools.create.title')}
      </Title>
      <Paragraph type="secondary" style={{ marginTop: 0 }}>
        {t('repo.diffTools.create.desc')}
      </Paragraph>
      <Text type="secondary" style={{ display: 'block', marginBottom: spacing.sm }}>
        {t(isMaintenance ? 'repo.diffTools.create.maintenanceHint' : 'repo.diffTools.needMaintenance')}
      </Text>
      <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
        <Segmented
          value={sourceKind}
          onChange={(next) => setSourceKind(next as 'server' | 'client')}
          options={[
            { label: t('repo.diffTools.source.server'), value: 'server' },
            { label: t('repo.diffTools.source.client'), value: 'client' }
          ]}
        />
        {sourceKind === 'server' ? (
          <Space.Compact style={{ width: '100%' }}>
            <Input
              readOnly
              value={sourceDir}
              placeholder={t('repo.diffTools.create.dirPlaceholder')}
              onChange={(event) => setSourceDir(event.target.value)}
            />
            <Button onClick={() => setBrowserOpen(true)}>{t('sourceDir.browse')}</Button>
          </Space.Compact>
        ) : (
          <Space.Compact style={{ width: '100%' }}>
            <Input
              readOnly={supportsDirectoryPicker()}
              value={clientDir}
              placeholder={t('repo.diffTools.create.clientDirPlaceholder')}
              onChange={(event) => setClientDir(event.target.value)}
            />
            <Button onClick={pickClientDir}>{t('sourceDir.selectDir')}</Button>
          </Space.Compact>
        )}
        <Space wrap>
          <Select<DiffSyncMode>
            value={mode}
            style={{ width: 200 }}
            onChange={setMode}
            options={[
              { value: 'auto', label: t('repo.diffTools.mode.auto') },
              { value: 'hash', label: t('repo.diffTools.mode.hash') },
              { value: 'size', label: t('repo.diffTools.mode.size') }
            ]}
          />
          <Checkbox checked={deleteExtra} onChange={(event) => setDeleteExtra(event.target.checked)}>
            {t('repo.diffTools.create.deleteExtra')}
          </Checkbox>
          <Button type="primary" loading={createRunning} disabled={!canCreate} onClick={startCreate}>
            {t('repo.diffTools.create.start')}
          </Button>
        </Space>
        {createResult ? (
          <div
            style={{
              border: '1px solid rgba(128, 128, 128, 0.22)',
              borderRadius: 8,
              padding: spacing.md,
              width: '100%'
            }}
          >
            <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
              <Text strong>
                {t('repo.diffTools.create.resultSummary', {
                  added: createResult.added,
                  modified: createResult.modified,
                  deleted: createResult.deleted
                })}
              </Text>
              <Text type="secondary">
                {t('repo.diffTools.create.fileSummary', {
                  source: createResult.source_files,
                  disk: createResult.diff_files,
                  bytes: formatBytes(createResult.source_bytes)
                })}
              </Text>
              {createResult.warnings?.length ? (
                <Text type="warning">
                  {t('repo.diffTools.create.warnings', { count: createResult.warnings.length })}
                </Text>
              ) : null}
              {(createResult.entries ?? []).length ? (
                <Table<DiffSyncEntryDTO>
                  size="small"
                  rowKey={(row) => `${row.kind}:${row.path}`}
                  columns={columns}
                  dataSource={createResult.entries ?? []}
                  pagination={{ pageSize: 8, hideOnSinglePage: true }}
                  scroll={{ x: 560 }}
                />
              ) : null}
              {createResult.truncated ? (
                <Text type="secondary">{t('repo.diffTools.create.truncated')}</Text>
              ) : null}
            </Space>
          </div>
        ) : null}
      </Space>

      <Divider />
      <Title level={5} style={{ marginTop: 0 }}>
        {t('repo.diffTools.merge.title')}
      </Title>
      <Paragraph type="secondary" style={{ marginTop: 0 }}>
        {t('repo.diffTools.merge.desc')}
      </Paragraph>
      <Space wrap>
        <Button
          danger
          disabled={!isMaintenance || diffs.length === 0 || mergeRunning}
          onClick={() => {
            setSelectedDiff(diffs.some((disk) => disk.id === lastCreatedDiffID) ? lastCreatedDiffID : '')
            setMergeOpen(true)
          }}
        >
          {t('repo.diffTools.merge.open')}
        </Button>
        <Text type="secondary">{t('repo.diffTools.merge.available', { count: diffs.length })}</Text>
      </Space>
        </>
      )}

      <ServerDirBrowser
        open={browserOpen}
        initialPath={sourceDir}
        onCancel={() => setBrowserOpen(false)}
        onConfirm={(path) => {
          setSourceDir(path)
          setBrowserOpen(false)
        }}
      />

      <Modal
        open={mergeOpen}
        title={t('repo.diffTools.merge.modalTitle')}
        width={640}
        centered
        okText={t('repo.diffTools.merge.confirm')}
        cancelText={t('common.cancel')}
        okButtonProps={{ danger: true, disabled: !selected }}
        confirmLoading={mergeRunning}
        onCancel={() => {
          if (!mergeRunning) setMergeOpen(false)
        }}
        onOk={startMerge}
      >
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <Alert
            type="warning"
            showIcon
            message={t('repo.diffTools.merge.warning', { count: otherCount })}
            description={t('repo.diffTools.merge.warningDetail')}
          />
          {diffs.length === 0 ? (
            <Alert type="info" showIcon message={t('repo.diffTools.merge.noDiffs')} />
          ) : (
            <Radio.Group
              value={selectedDiff}
              onChange={(event) => setSelectedDiff(event.target.value as string)}
              style={{ width: '100%' }}
            >
              <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
                {diffs.map((disk) => (
                  <Radio key={disk.id} value={disk.id} style={{ width: '100%' }}>
                    <Space wrap>
                      <Text strong>{disk.id.slice(0, 8)}</Text>
                      <Tag>v{disk.parent_version}</Tag>
                      <Text type="secondary">{formatBytes(disk.physical_bytes || 0)}</Text>
                      <Text type="secondary">{formatTime(disk.created_at)}</Text>
                    </Space>
                  </Radio>
                ))}
              </Space>
            </Radio.Group>
          )}
          {selected ? (
            <Alert
              type="info"
              showIcon
              message={t('repo.diffTools.merge.selected', {
                id: selected.id.slice(0, 8),
                version: selected.parent_version
              })}
            />
          ) : null}
          {mergeRunning ? <Progress percent={mergeProgress?.progress ?? 0} size="small" status="active" /> : null}
        </Space>
      </Modal>
    </SectionCard>
  )
}
