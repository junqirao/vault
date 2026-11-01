import { useMemo, useState } from 'react'
import { Button, Modal, Space, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { LeaseDTO, UserDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'

export interface UserLeasesModalProps {
  /** 目标用户；null 表示弹窗关闭。 */
  user: UserDTO | null
  onClose: () => void
}

/**
 * 单个用户的在线租约（原「在线租约」页并入用户管理后的详情入口）。
 *
 * 租约接口只支持按存储库筛选，不支持按用户筛选，因此这里取一次全量再按 user_id
 * 过滤：管理端的租约总量通常不大，而并入用户详情后用户只关心"这个人当前挂了什么"。
 * 强制下线仍只对 active 租约开放，与合并前的行为一致。
 */
export function UserLeasesModal({ user, onClose }: UserLeasesModalProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [pendingRevoke, setPendingRevoke] = useState<LeaseDTO | null>(null)

  const leasesQuery = useQuery({
    queryKey: ['leases', 'user', user?.id ?? ''],
    queryFn: () => api.listLeases(),
    enabled: user !== null,
    refetchInterval: 15000
  })

  const leases = useMemo(
    () => (leasesQuery.data?.items ?? []).filter((lease) => lease.user_id === user?.id),
    [leasesQuery.data, user?.id]
  )

  const revokeMutation = useMutation({
    mutationFn: (leaseId: string) => api.revokeLease(leaseId),
    onSuccess: () => {
      setPendingRevoke(null)
      void queryClient.invalidateQueries({ queryKey: ['leases'] })
    },
    onError: (err) => setError(err)
  })

  const columns: TableColumnsType<LeaseDTO> = [
    { title: t('lease.field.target'), dataIndex: 'target_name', key: 'target_name' },
    {
      title: t('field.mountPoint'),
      dataIndex: 'mount_point',
      key: 'mount_point',
      render: (value?: string) => value || '-'
    },
    {
      title: t('field.clientId'),
      dataIndex: 'client_id',
      key: 'client_id',
      render: (value: string) => <Typography.Text style={{ fontSize: 12 }}>{value}</Typography.Text>
    },
    {
      title: t('common.status'),
      dataIndex: 'state',
      key: 'state',
      width: 130,
      render: (value: string) => <StatusTag group="lease" value={value} />
    },
    {
      title: t('lease.field.lastSeen'),
      dataIndex: 'last_seen_at',
      key: 'last_seen_at',
      width: 170,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('common.expiresAt'),
      dataIndex: 'expires_at',
      key: 'expires_at',
      width: 170,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 110,
      render: (_value, lease) => (
        <Button
          type="link"
          size="small"
          danger
          disabled={lease.state !== 'active'}
          onClick={() => setPendingRevoke(lease)}
        >
          {t('action.revoke')}
        </Button>
      )
    }
  ]

  return (
    <Modal
      open={user !== null}
      title={t('user.leases.title', { username: user?.username ?? '' })}
      width={960}
      centered
      onCancel={onClose}
      footer={<Button onClick={onClose}>{t('common.close')}</Button>}
      destroyOnClose
    >
      <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
        {error ? <ErrorNotice error={error} /> : null}
        {leasesQuery.error ? <ErrorNotice error={leasesQuery.error} /> : null}
        <DataTable<LeaseDTO>
          columns={columns}
          rows={leases}
          rowKey={(lease) => lease.id}
          loading={leasesQuery.isLoading}
          empty={<EmptyState title={t('user.leases.empty')} />}
          scroll={{ x: 820 }}
        />
      </Space>

      <ConfirmDialog
        open={pendingRevoke !== null}
        danger
        title={t('action.revoke')}
        content={t('lease.revoke.confirm')}
        loading={revokeMutation.isPending}
        onConfirm={() => pendingRevoke && revokeMutation.mutate(pendingRevoke.id)}
        onCancel={() => setPendingRevoke(null)}
      />
    </Modal>
  )
}
