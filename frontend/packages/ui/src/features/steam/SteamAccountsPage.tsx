import { useMemo, useState } from 'react'
import { Alert, App, Button, Dropdown, Form, Input, InputNumber, Modal, Progress, Select, Space, Switch, Table, Tag, Typography } from 'antd'
import { MoreOutlined } from '@ant-design/icons'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { useApi } from '../../api/provider'
import { errorCodeOf } from '../../api/errors'
import type {
  CreateSteamAccountRequest,
  SteamAccountDTO,
  SteamAccountStatus,
  SteamNetworkCheckDTO,
  UpdateSteamAccountRequest
} from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'

const STATUS_COLOR: Record<SteamAccountStatus, string> = {
  active: 'green',
  needs_2fa: 'orange',
  disabled: 'default',
  error: 'red'
}

function proxyLabel(
  t: (key: string, args?: Record<string, unknown>) => string,
  proxy: string,
  source: string,
  hint: string
): string {
  switch (source) {
    case 'config':
      return t('steam.runtime.proxy.config', { proxy })
    case 'system':
      return t('steam.runtime.proxy.system', { proxy })
    case 'env':
      return t('steam.runtime.proxy.env', { proxy })
    case 'pac':
      return t('steam.runtime.proxy.pac', { hint })
    default:
      return t('steam.runtime.proxy.none')
  }
}

interface AccountFormValues {
  username: string
  display_name?: string
  credential?: string
  guard_type: 'none' | 'totp' | 'email'
  guard_secret?: string
  max_concurrent: number
  enabled?: boolean
}

/**
 * Steam 内容账号管理（独立页面）。
 *
 * 该模块只用于存储库内容下载/更新；页面不提供玩家登录、Steam 客户端登录或任意命令执行。
 * 凭据只写不读：编辑时留空表示不修改，接口永远不会返回明文。
 */
export function SteamAccountsPage({ embedded = false }: { embedded?: boolean }): JSX.Element {
  return <SteamAccountsPanel embedded={embedded} />
}

function SteamAccountsPanel({ embedded }: { embedded: boolean }): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { message } = App.useApp()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<AccountFormValues>()
  const [error, setError] = useState<unknown>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<SteamAccountDTO | null>(null)
  const [guardTarget, setGuardTarget] = useState<{ account: SteamAccountDTO; action: 'login' | 'licenses' } | null>(null)
  const [guardCode, setGuardCode] = useState('')
  const [networkCheck, setNetworkCheck] = useState<SteamNetworkCheckDTO | null>(null)
  const [outputTarget, setOutputTarget] = useState<SteamAccountDTO | null>(null)

  const accountsQuery = useQuery({
    queryKey: ['steam', 'accounts'],
    queryFn: () => api.listSteamAccounts({ limit: 500 }),
    // 后台会自动刷新许可/会话状态；轮询让界面及时提示需要验证，而不必手动刷新。
    refetchInterval: 30_000
  })
  const runtimeQuery = useQuery({
    queryKey: ['steam', 'runtime'],
    queryFn: () => api.steamRuntimeStatus(),
    refetchInterval: (query) => (query.state.data?.installing ? 1000 : false)
  })
  const installMutation = useMutation({
    mutationFn: () => api.installSteamCmd(),
    onSuccess: (status) => {
      setError(null)
      void queryClient.invalidateQueries({ queryKey: ['steam', 'runtime'] })
      if (status.installing) message.info(t('steam.runtime.installStarted'))
      else if (status.last_error) message.warning(t('steam.runtime.bootstrapWarning'))
      else message.success(t('steam.runtime.installOk'))
    },
    onError: (err) => setError(err)
  })
  const networkCheckMutation = useMutation({
    mutationFn: () => api.checkSteamNetwork(),
    onSuccess: (result) => {
      setError(null)
      setNetworkCheck(result)
    },
    onError: (err) => setError(err)
  })

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['steam', 'accounts'] })
    void queryClient.invalidateQueries({ queryKey: ['steam', 'available'] })
    void queryClient.invalidateQueries({ queryKey: ['steam', 'groups'] })
    void queryClient.invalidateQueries({ queryKey: ['steam', 'owned-games'] })
  }

  const saveMutation = useMutation({
    mutationFn: async (values: AccountFormValues) => {
      if (editing) {
        const body: UpdateSteamAccountRequest = {
          display_name: values.display_name,
          guard_type: values.guard_type,
          max_concurrent: values.max_concurrent,
          enabled: values.enabled
        }
        if (values.credential?.trim()) body.credential = values.credential.trim()
        if (values.guard_secret?.trim()) body.guard_secret = values.guard_secret.trim()
        return api.updateSteamAccount(editing.id, body)
      }
      const body: CreateSteamAccountRequest = {
        username: values.username.trim(),
        display_name: values.display_name?.trim(),
        credential: values.credential?.trim() ?? '',
        guard_type: values.guard_type,
        guard_secret: values.guard_secret?.trim(),
        max_concurrent: values.max_concurrent
      }
      return api.createSteamAccount(body)
    },
    onSuccess: () => {
      setFormOpen(false)
      setEditing(null)
      form.resetFields()
      setError(null)
      invalidate()
      message.success(t('steam.accounts.saved'))
    },
    onError: (err) => setError(err)
  })

  const testMutation = useMutation({
    mutationFn: (args: { account: SteamAccountDTO; code?: string }) => api.testSteamLogin(args.account.id, args.code),
    onSuccess: () => {
      setError(null)
      invalidate()
      message.success(t('steam.accounts.testOk'))
      setGuardTarget(null)
      setGuardCode('')
    },
    onError: (err, variables) => {
      setError(err)
      invalidate()
      // 缓存会话失效时自动弹出验证码输入，而不是只显示一条错误。
      if (errorCodeOf(err) === 'steam.account_needs_2fa') {
        setGuardCode('')
        setGuardTarget({ account: variables.account, action: 'login' })
      }
    }
  })

  const licenseMutation = useMutation({
    mutationFn: (args: { account: SteamAccountDTO; code?: string }) => api.syncSteamLicenses(args.account.id, args.code),
    onSuccess: (res) => {
      setError(null)
      invalidate()
      message.success(t('steam.accounts.licenseOk', { count: res.items.length }))
      setGuardTarget(null)
      setGuardCode('')
    },
    onError: (err, variables) => {
      setError(err)
      invalidate()
      if (errorCodeOf(err) === 'steam.account_needs_2fa') {
        setGuardCode('')
        setGuardTarget({ account: variables.account, action: 'licenses' })
      }
    }
  })

  const disableMutation = useMutation({
    mutationFn: (id: string) => api.disableSteamAccount(id),
    onSuccess: () => {
      invalidate()
      message.success(t('steam.accounts.disabledDone'))
    },
    onError: (err) => setError(err)
  })

  const releaseLeasesMutation = useMutation({
    mutationFn: (id: string) => api.releaseSteamLeases(id),
    onSuccess: (res) => {
      invalidate()
      message.success(t('steam.accounts.releaseLeasesDone', { count: res.released }))
    },
    onError: (err) => setError(err)
  })

  const openCreate = (): void => {
    setEditing(null)
    setError(null)
    form.resetFields()
    form.setFieldsValue({
      guard_type: 'none',
      max_concurrent: 1,
      enabled: true
    })
    setFormOpen(true)
  }

  const openEdit = (account: SteamAccountDTO): void => {
    setEditing(account)
    setError(null)
    form.resetFields()
    form.setFieldsValue({
      username: account.username,
      display_name: account.display_name,
      guard_type: account.guard_type,
      max_concurrent: account.max_concurrent,
      enabled: account.status !== 'disabled'
    })
    setFormOpen(true)
  }

  const runAction = (account: SteamAccountDTO, action: 'login' | 'licenses'): void => {
    // TOTP 在账号尚未验证/需要重新验证时弹一次输入框；已 active 的 TOTP 由服务端自动生成。
    // 邮箱 Guard 无法自动读取，始终弹框。
    const needsCode = account.guard_type === 'email' || (account.guard_type === 'totp' && account.status !== 'active')
    if (!needsCode) {
      if (action === 'login') testMutation.mutate({ account })
      else licenseMutation.mutate({ account })
      return
    }
    setGuardCode('')
    setGuardTarget({ account, action })
  }

  const columns = useMemo<TableColumnsType<SteamAccountDTO>>(
    () => [
      {
        title: t('steam.accounts.username'),
        dataIndex: 'username',
        key: 'username',
        width: 180,
        render: (_v, account) => (
          <Space direction="vertical" size={0} style={{ maxWidth: 160, minWidth: 0 }}>
            <Typography.Text strong ellipsis={{ tooltip: account.username }}>
              {account.username}
            </Typography.Text>
            <Typography.Text type="secondary" ellipsis={{ tooltip: account.display_name }} style={{ fontSize: fontSize.xs }}>
              {account.display_name || '-'}
            </Typography.Text>
          </Space>
        )
      },
      {
        title: t('common.status'),
        dataIndex: 'status',
        key: 'status',
        width: 100,
        render: (status: SteamAccountStatus, account) => (
          <Space size={4} wrap>
            <Tag color={STATUS_COLOR[status]}>{t(`steam.status.${status}`)}</Tag>
            {account.active_leases > 0 ? (
              <Tag color="orange">{t('steam.accounts.busy', { count: account.active_leases })}</Tag>
            ) : null}
          </Space>
        )
      },
      {
        title: t('steam.accounts.guard'),
        dataIndex: 'guard_type',
        key: 'guard_type',
        width: 100,
        render: (value: string) => t(`steam.guard.${value}`)
      },
      {
        title: t('steam.accounts.credential'),
        dataIndex: 'has_credential',
        key: 'has_credential',
        width: 90,
        render: (value: boolean) => (value ? t('common.yes') : t('common.no'))
      },
      {
        title: t('steam.accounts.maxConcurrent'),
        dataIndex: 'max_concurrent',
        key: 'max_concurrent',
        width: 90
      },
      {
        title: t('steam.accounts.lastLogin'),
        dataIndex: 'last_login_at',
        key: 'last_login_at',
        width: 160,
        render: (value: number) => (value ? formatTime(value) : '-')
      },
      {
        title: t('steam.accounts.lastChecked'),
        dataIndex: 'last_checked_at',
        key: 'last_checked_at',
        width: 160,
        render: (value: number) => (value ? formatTime(value) : '-')
      },
      {
        title: t('steam.accounts.lastError'),
        dataIndex: 'last_error',
        key: 'last_error',
        width: 200,
        render: (value: string) =>
          value ? (
            <Typography.Text type="danger" ellipsis={{ tooltip: value }} style={{ maxWidth: 180, display: 'inline-block' }}>
              {value}
            </Typography.Text>
          ) : (
            '-'
          )
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 260,
        fixed: 'right',
        render: (_v, account) => {
          const menuItems = [
            ...(account.active_leases > 0
              ? [{ key: 'release', label: t('steam.accounts.releaseLeases') }]
              : []),
            ...(account.last_output ? [{ key: 'output', label: t('steam.accounts.viewOutput') }] : []),
            { type: 'divider' as const },
            {
              key: 'disable',
              label: t('common.disabled'),
              danger: true,
              disabled: account.status === 'disabled'
            }
          ]
          return (
            <Space wrap size={4}>
              <Button size="small" onClick={() => runAction(account, 'login')} loading={testMutation.isPending}>
                {t('steam.accounts.test')}
              </Button>
              <Button size="small" onClick={() => runAction(account, 'licenses')} loading={licenseMutation.isPending}>
                {t('steam.accounts.sync')}
              </Button>
              <Button size="small" onClick={() => openEdit(account)}>
                {t('common.edit')}
              </Button>
              <Dropdown
                menu={{
                  items: menuItems,
                  onClick: ({ key }) => {
                    if (key === 'release') releaseLeasesMutation.mutate(account.id)
                    else if (key === 'output') setOutputTarget(account)
                    else if (key === 'disable') disableMutation.mutate(account.id)
                  }
                }}
              >
                <Button size="small" icon={<MoreOutlined />} />
              </Dropdown>
            </Space>
          )
        }
      }
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [t, testMutation.isPending, licenseMutation.isPending, disableMutation.isPending, releaseLeasesMutation.isPending]
  )

  return (
    <>
      <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: spacing.sm }}>
          {embedded ? (
            <span />
          ) : (
            <Typography.Title level={5} style={{ margin: 0 }}>
              {t('steam.accounts.title')}
            </Typography.Title>
          )}
          <Space>
            <Button
              onClick={() => {
                void accountsQuery.refetch()
                void runtimeQuery.refetch()
              }}
            >
              {t('common.refresh')}
            </Button>
            <Button loading={networkCheckMutation.isPending} onClick={() => networkCheckMutation.mutate()}>
              {t('steam.runtime.checkNetwork')}
            </Button>
            <Button type="primary" onClick={openCreate}>
              {t('steam.accounts.add')}
            </Button>
          </Space>
        </div>

        {runtimeQuery.data ? (
          runtimeQuery.data.installing ? (
            <Alert
              type="info"
              showIcon
              message={t('steam.runtime.installing', {
                phase: t(`steam.runtime.phase.${runtimeQuery.data.phase || 'preparing'}`)
              })}
              description={
                <Space direction="vertical" size={4} style={{ width: '100%' }}>
                  <Progress percent={runtimeQuery.data.progress} status="active" />
                  {runtimeQuery.data.message ? (
                    <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                      {runtimeQuery.data.message}
                    </Typography.Text>
                  ) : null}
                  <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                    {proxyLabel(
                      t,
                      runtimeQuery.data.proxy,
                      runtimeQuery.data.proxy_source,
                      runtimeQuery.data.proxy_hint
                    )}
                  </Typography.Text>
                </Space>
              }
            />
          ) : (
            <Alert
              type={runtimeQuery.data.installed ? (runtimeQuery.data.last_error ? 'warning' : 'success') : 'warning'}
              showIcon
              message={
                runtimeQuery.data.installed
                  ? t('steam.runtime.ready', { path: runtimeQuery.data.path })
                  : t('steam.runtime.missing')
              }
              description={
                <Space direction="vertical" size={2}>
                  {runtimeQuery.data.last_error ? (
                    <Typography.Text type="danger" style={{ fontSize: fontSize.xs }}>
                      {runtimeQuery.data.last_error}
                    </Typography.Text>
                  ) : runtimeQuery.data.installed ? null : (
                    <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                      {t('steam.runtime.missingHint')}
                    </Typography.Text>
                  )}
                  <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                    {proxyLabel(
                      t,
                      runtimeQuery.data.proxy,
                      runtimeQuery.data.proxy_source,
                      runtimeQuery.data.proxy_hint
                    )}
                  </Typography.Text>
                </Space>
              }
              action={
                !runtimeQuery.data.installed ? (
                  <Button size="small" type="primary" loading={installMutation.isPending} onClick={() => installMutation.mutate()}>
                    {t('steam.runtime.install')}
                  </Button>
                ) : undefined
              }
            />
          )
        ) : null}
        {networkCheck ? (
          <Alert
            type={networkCheck.ok ? 'success' : 'error'}
            showIcon
            closable
            onClose={() => setNetworkCheck(null)}
            message={
              networkCheck.ok
                ? t('steam.runtime.networkOk', { status: networkCheck.status, ms: networkCheck.latency_ms })
                : t('steam.runtime.networkFailed')
            }
            description={
              <Space direction="vertical" size={2}>
                <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                  {t('steam.runtime.networkUrl', { url: networkCheck.url })}
                </Typography.Text>
                <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                  {proxyLabel(t, networkCheck.proxy, networkCheck.proxy_source, networkCheck.proxy_hint)}
                </Typography.Text>
                {networkCheck.error ? (
                  <Typography.Text type="danger" style={{ fontSize: fontSize.xs }}>
                    {networkCheck.error}
                  </Typography.Text>
                ) : null}
              </Space>
            }
          />
        ) : null}
        {error && errorCodeOf(error) !== 'steam.steamcmd_unavailable' ? <ErrorNotice error={error} /> : null}
        <SectionCard title={t('steam.accounts.listTitle')}>
          <Table<SteamAccountDTO>
            rowKey="id"
            size="small"
            loading={accountsQuery.isLoading}
            columns={columns}
            dataSource={accountsQuery.data?.items ?? []}
            pagination={{ pageSize: 10, showSizeChanger: true }}
            scroll={{ x: 1200 }}
            locale={{ emptyText: t('steam.accounts.empty') }}
          />
        </SectionCard>
      </Space>

      <Modal
        open={formOpen}
        title={editing ? t('steam.accounts.editTitle') : t('steam.accounts.addTitle')}
        onCancel={() => setFormOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={saveMutation.isPending}
        destroyOnClose
        width={640}
      >
        <Form<AccountFormValues> form={form} layout="vertical" onFinish={(values) => saveMutation.mutate(values)}>
          <Form.Item name="username" label={t('steam.accounts.username')} rules={[{ required: true }]}>
            <Input disabled={Boolean(editing)} autoComplete="off" />
          </Form.Item>
          <Form.Item name="display_name" label={t('steam.accounts.displayName')}>
            <Input />
          </Form.Item>
          <Form.Item
            name="credential"
            label={t('steam.accounts.credential')}
            rules={editing ? [] : [{ required: true }]}
            extra={editing ? t('steam.accounts.credentialEditHint') : undefined}
          >
            <Input.Password autoComplete="new-password" placeholder={editing ? t('steam.accounts.keepEmpty') : ''} />
          </Form.Item>
          <Form.Item name="guard_type" label={t('steam.accounts.guard')} rules={[{ required: true }]}>
            <Select
              options={[
                { value: 'none', label: t('steam.guard.none') },
                { value: 'totp', label: t('steam.guard.totp') },
                { value: 'email', label: t('steam.guard.email') }
              ]}
            />
          </Form.Item>
          <Form.Item name="guard_secret" label={t('steam.accounts.guardSecret')} extra={t('steam.accounts.guardSecretHint')}>
            <Input.Password autoComplete="new-password" placeholder={editing ? t('steam.accounts.keepEmpty') : ''} />
          </Form.Item>
          <Form.Item name="max_concurrent" label={t('steam.accounts.maxConcurrent')} rules={[{ required: true }]}>
            <InputNumber min={1} max={8} style={{ width: '100%' }} />
          </Form.Item>
          {editing ? (
            <Form.Item name="enabled" label={t('steam.accounts.enabled')} valuePropName="checked">
              <Switch />
            </Form.Item>
          ) : null}
        </Form>
      </Modal>

      <Modal
        open={guardTarget !== null}
        title={t('steam.accounts.guardCodeTitle')}
        onCancel={() => setGuardTarget(null)}
        onOk={() => {
          if (!guardTarget) return
          const args = { account: guardTarget.account, code: guardCode.trim() }
          if (guardTarget.action === 'login') testMutation.mutate(args)
          else licenseMutation.mutate(args)
        }}
        confirmLoading={testMutation.isPending || licenseMutation.isPending}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Typography.Text type="secondary">{t('steam.accounts.guardCodeHint')}</Typography.Text>
          <Input value={guardCode} onChange={(e) => setGuardCode(e.target.value)} autoFocus />
        </Space>
      </Modal>
      <Modal
        open={outputTarget !== null}
        title={t('steam.accounts.outputTitle')}
        onCancel={() => setOutputTarget(null)}
        footer={null}
        width={900}
        styles={{ body: { maxHeight: '65vh', overflow: 'auto' } }}
      >
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('steam.accounts.outputHint')}
        </Typography.Text>
        {outputTarget?.cmd_root ? (
          <Typography.Paragraph
            type="secondary"
            copyable
            style={{ fontSize: fontSize.xs, margin: `${spacing.xs} 0 0` }}
          >
            {t('steam.accounts.cmdRoot')}: {outputTarget.cmd_root}
          </Typography.Paragraph>
        ) : null}
        <pre
          className="selectable"
          style={{
            marginTop: spacing.sm,
            padding: spacing.sm,
            background: 'rgba(0,0,0,0.04)',
            borderRadius: 6,
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-all',
            fontSize: 12
          }}
        >
          {outputTarget?.last_output || ''}
        </pre>
      </Modal>
    </>
  )
}
