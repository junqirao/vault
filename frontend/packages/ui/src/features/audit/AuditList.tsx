import { useCallback, useMemo, useState } from 'react'
import { Button, Input, Space, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { AuditDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'
import { auditActionLabel } from '../../utils/labels'

interface AuditRefInfo {
  label: string
  /** 可跳转路径；为空表示没有可进入的对象页。 */
  to?: string
}

/** 取路径最后一段（磁盘路径用）；兼容 Windows 反斜杠与 POSIX 斜杠。 */
function baseName(path: string): string {
  const index = Math.max(path.lastIndexOf('\\'), path.lastIndexOf('/'))
  return index >= 0 ? path.slice(index + 1) : path
}

/** 审计日志（仅超级管理员）。 */
export interface AuditListProps {
  /** 嵌入合并后的「服务日志」Tabs 时只渲染卡片，不再套 PageShell。 */
  embedded?: boolean
}

export function AuditList({ embedded = false }: AuditListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const navigate = useNavigate()
  const [action, setAction] = useState('')
  const [userId, setUserId] = useState('')

  const auditQuery = useQuery({
    queryKey: ['audit', action, userId],
    queryFn: () => api.auditLogs({ action: action || undefined, user_id: userId || undefined, limit: 200 })
  })

  // 用户列显示用户名而不是 UUID；资源列的 repo/storage/disk 也解析成名称。
  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 500 })
  })
  const reposQuery = useQuery({
    queryKey: ['repos', 'all'],
    queryFn: () => api.listRepos({ limit: 500 })
  })
  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages()
  })
  // 全量磁盘一次拉取：审计资源里的 "disk:<id>" 很多早已删除，逐条 GET 会刷 404 警告。
  const disksQuery = useQuery({
    queryKey: ['disks', 'all'],
    queryFn: () => api.listDisks()
  })

  const userNameByID = useMemo(() => {
    const map = new Map<string, string>()
    for (const user of usersQuery.data?.items ?? []) map.set(user.id, user.username)
    return map
  }, [usersQuery.data])

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

  const resolveUser = useCallback(
    (id: string): AuditRefInfo => {
      const raw = (id ?? '').trim()
      if (!raw) return { label: '-' }
      // system / 空是系统动作，不指向任何真实用户，不做链接。
      if (raw === 'system') return { label: raw }
      const name = userNameByID.get(raw)
      return name ? { label: name, to: '/admin/users' } : { label: raw }
    },
    [userNameByID]
  )

  /**
   * 审计资源 → 「类型-名称」+ 可跳转页面。
   *
   * 服务端资源串是 `<type>:<id>`（部分类型没有 id，如 session / uploads）。只有能关联到
   * 现有对象的才给链接；target、file 等没有管理页的资源保持纯文本，避免造出一个假链接。
   */
  const resolveResource = useCallback(
    (resource: string): AuditRefInfo => {
      const raw = (resource ?? '').trim()
      if (!raw) return { label: '-' }
      const separator = raw.indexOf(':')
      const kind = separator >= 0 ? raw.slice(0, separator) : raw
      const value = separator >= 0 ? raw.slice(separator + 1) : ''
      const shortId = value.length > 12 ? `${value.slice(0, 8)}…` : value

      switch (kind) {
        case 'repo': {
          const name = repoNameByID.get(value)
          return name
            ? { label: t('job.ref.repo', { name }), to: `/repos/${value}` }
            : { label: t('job.ref.repo', { name: shortId }) }
        }
        case 'storage': {
          const name = storageNameByID.get(value)
          return name
            ? { label: t('job.ref.storage', { name }), to: '/admin/storages' }
            : { label: t('job.ref.storage', { name: shortId }) }
        }
        case 'disk': {
          const disk = diskByID.get(value)
          if (disk) {
            const repoName = repoNameByID.get(disk.repoID)
            return {
              label: t('job.ref.disk', { id: disk.name || shortId }),
              to: repoName ? `/repos/${disk.repoID}` : undefined
            }
          }
          return { label: t('job.ref.disk', { id: shortId }) }
        }
        case 'job':
          return { label: t('audit.ref.job', { id: shortId }), to: '/admin/jobs' }
        case 'allocation':
          return { label: t('audit.ref.allocation', { id: shortId }) }
        case 'lease':
          return { label: t('audit.ref.lease', { id: shortId }) }
        case 'upload':
          return { label: t('job.ref.upload', { id: shortId }) }
        case 'target':
          return { label: t('job.ref.target', { id: value || shortId }) }
        case 'user': {
          const name = userNameByID.get(value)
          return name ? { label: name, to: '/admin/users' } : { label: value || shortId }
        }
        case 'key':
          return { label: t('audit.ref.key', { name: value }), to: '/admin/system' }
        case 'file':
        case 'path':
          return { label: t('audit.ref.file', { path: value || shortId }) }
        default:
          return { label: raw }
      }
    },
    [diskByID, repoNameByID, storageNameByID, t, userNameByID]
  )

  const columns = useMemo<TableColumnsType<AuditDTO>>(
    () => [
      { title: t('common.createdAt'), dataIndex: 'created_at', key: 'created_at', width: 170, render: (value: number) => formatTime(value) },
      {
        title: t('field.user'),
        dataIndex: 'user_id',
        key: 'user_id',
        width: 220,
        render: (value: string) => {
          const info = resolveUser(value)
          return info.to ? (
            <Typography.Link style={{ fontSize: 12 }} onClick={() => navigate(info.to!)}>
              {info.label}
            </Typography.Link>
          ) : (
            <span style={{ fontSize: 12 }}>{info.label}</span>
          )
        }
      },
      {
        title: t('field.action'),
        dataIndex: 'action',
        key: 'action',
        width: 200,
        render: (value: string) => auditActionLabel(value)
      },
      {
        title: t('field.resource'),
        dataIndex: 'resource',
        key: 'resource',
        width: 260,
        ellipsis: true,
        render: (value: string) => {
          const info = resolveResource(value)
          return info.to ? (
            <Typography.Link style={{ fontSize: 12 }} onClick={() => navigate(info.to!)}>
              {info.label}
            </Typography.Link>
          ) : (
            <span style={{ fontSize: 12 }}>{info.label}</span>
          )
        }
      },
      { title: t('field.detail'), dataIndex: 'detail', key: 'detail', render: (value: string) => <span style={{ fontSize: 12 }}>{value || '-'}</span> },
      { title: t('field.ip'), dataIndex: 'ip', key: 'ip', width: 140 },
      {
        title: t('field.result'),
        dataIndex: 'result',
        key: 'result',
        width: 110,
        render: (value: string) => <StatusTag group="audit" value={value} />
      }
    ],
    [t, resolveUser, resolveResource, navigate]
  )

  const content = (
    <>
      {auditQuery.error ? <ErrorNotice error={auditQuery.error} /> : null}
      <SectionCard
        title={t('common.filters')}
        extra={
          <Button icon={<ReloadOutlined />} onClick={() => void auditQuery.refetch()}>
            {t('common.refresh')}
          </Button>
        }
      >
        <Space wrap size={spacing.sm}>
          <Input
            allowClear
            value={action}
            placeholder={t('audit.filter.action')}
            style={{ width: 200 }}
            onChange={(event) => setAction(event.target.value)}
          />
          <Input
            allowClear
            value={userId}
            placeholder={t('audit.filter.userId')}
            style={{ width: 220 }}
            onChange={(event) => setUserId(event.target.value)}
          />
          <Button
            onClick={() => {
              setAction('')
              setUserId('')
            }}
          >
            {t('common.reset')}
          </Button>
        </Space>
      </SectionCard>
      <SectionCard
        title={t('audit.list.title')}
        extra={
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('common.total', { total: auditQuery.data?.items.length ?? 0 })}
          </Typography.Text>
        }
      >
        <DataTable<AuditDTO>
          columns={columns}
          rows={auditQuery.data?.items ?? []}
          rowKey={(log) => log.id}
          loading={auditQuery.isLoading}
          empty={<EmptyState title={t('audit.empty')} />}
          scroll={{ x: 1200 }}
        />
      </SectionCard>
    </>
  )

  if (embedded) return content

  return (
    <PageShell title={t('page.audit.title')} subtitle={t('page.audit.subtitle')}>
      {content}
    </PageShell>
  )
}
