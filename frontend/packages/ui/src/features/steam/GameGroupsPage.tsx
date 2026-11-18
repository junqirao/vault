import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  App,
  Button,
  Empty,
  Form,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Switch,
  Tag,
  TimePicker,
  Typography,
  type FormInstance
} from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import dayjs, { type Dayjs } from 'dayjs'

import { fetchLocalSteamLibrary } from '../../api/agentClient'
import { useApi } from '../../api/provider'
import type {
  CreateRepoGroupRequest,
  GameDTO,
  RepoGroupDTO,
  UpdateRepoGroupRequest,
  UserDTO
} from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'
import { SteamAccountsPage } from './SteamAccountsPage'
import { SteamAppIcon } from './SteamAppIcon'
import { SteamGamePicker } from './SteamGamePicker'
import { SteamUpdateMeter } from './SteamUpdateMeter'
import { steamUpdateProgress } from './steamUpdateProgress'

interface GroupFormValues {
  name: string
  description?: string
  steam_account_id?: string
  access_mode: 'all' | 'selected'
  member_ids?: string[]
  auto_update: boolean
  update_window?: [Dayjs, Dayjs] | null
}

/** 把服务端的 "HH:MM-HH:MM" 解析成时间选择器的值。 */
function parseUpdateWindow(value: string): [Dayjs, Dayjs] | undefined {
  const match = /^(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})$/.exec(value.trim())
  if (!match) return undefined
  const base = dayjs().second(0).millisecond(0)
  return [
    base.hour(Number(match[1])).minute(Number(match[2])),
    base.hour(Number(match[3])).minute(Number(match[4]))
  ]
}

function formatUpdateWindow(value: GroupFormValues['update_window']): string {
  const start = value?.[0]
  const end = value?.[1]
  if (!start || !end) return ''
  return `${start.format('HH:mm')}-${end.format('HH:mm')}`
}

/** 默认自动更新窗口：凌晨 03:00-06:00，避开白天使用高峰。 */
function defaultUpdateWindow(): [Dayjs, Dayjs] {
  const base = dayjs().second(0).millisecond(0)
  return [base.hour(3).minute(0), base.hour(6).minute(0)]
}

interface AccountOption {
  id: string
  username: string
  display_name: string
  status: string
}

function accessLabel(t: (key: string) => string, group: RepoGroupDTO): string {
  return t(`steam.access.${group.access_mode}`)
}

/** 非就绪状态的标签配色：实心标签用具体色值（preset 名会渲染成浅底描边）。 */
function gameStatusColor(status: GameDTO['status']): string {
  if (status === 'ready') return palette.success
  if (status === 'updating') return palette.accent
  if (status === 'paused') return palette.warning
  return palette.danger
}

/** 游戏库管理：分组卡片瀑布流 + 全页面游戏选择器。 */
export function GameGroupsPage(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { message } = App.useApp()
  const queryClient = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  const queryGroupID = searchParams.get('group')
  const [groupForm] = Form.useForm<GroupFormValues>()
  const [error, setError] = useState<unknown>(null)
  const [groupOpen, setGroupOpen] = useState(false)
  const [editingGroup, setEditingGroup] = useState<RepoGroupDTO | null>(null)
  const [accountsOpen, setAccountsOpen] = useState(false)
  const [detailGroupID, setDetailGroupID] = useState<string | null>(queryGroupID)
  const [pickerGroupID, setPickerGroupID] = useState<string | null>(null)

  useEffect(() => {
    if (queryGroupID) setDetailGroupID(queryGroupID)
  }, [queryGroupID])

  const groupsQuery = useQuery({
    queryKey: ['steam', 'groups'],
    queryFn: () => api.listRepoGroups({ kind: 'steam' })
  })
  const accountsQuery = useQuery({
    queryKey: ['steam', 'accounts', 'for-select'],
    queryFn: () => api.listSteamAccounts({ limit: 500 }),
    refetchInterval: 60_000
  })
  const usersQuery = useQuery({
    queryKey: ['users', 'options', 'steam-members'],
    queryFn: () => api.listUsers({ limit: 500 })
  })
  // 页面级扫描一次，组卡片/详情里的本机封面才有图可读。
  useQuery({
    queryKey: ['steam', 'local-library'],
    queryFn: () => fetchLocalSteamLibrary(),
    staleTime: 30_000,
    retry: false
  })

  const groups = groupsQuery.data?.items ?? []
  const accounts = useMemo<AccountOption[]>(
    () =>
      (accountsQuery.data?.items ?? []).map((account) => ({
        id: account.id,
        username: account.username,
        display_name: account.display_name,
        status: account.status
      })),
    [accountsQuery.data]
  )
  const users = usersQuery.data?.items ?? []
  const detailGroup = groups.find((group) => group.id === detailGroupID)
  const pickerGroup = groups.find((group) => group.id === pickerGroupID)
  const pickerAccount = accounts.find((account) => account.id === pickerGroup?.steam_account_id)

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['steam', 'groups'] })
    void queryClient.invalidateQueries({ queryKey: ['steam', 'games'] })
    void queryClient.invalidateQueries({ queryKey: ['steam', 'accounts'] })
  }

  const saveGroupMutation = useMutation({
    mutationFn: async (values: GroupFormValues) => {
      const base: CreateRepoGroupRequest = {
        name: values.name.trim(),
        kind: 'steam',
        description: values.description?.trim(),
        steam_account_id: values.steam_account_id,
        // 平台与并发仍由系统管理；自动更新开关和时间窗口暴露给管理员。
        target_platform: 'windows',
        auto_update: values.auto_update,
        update_window: formatUpdateWindow(values.update_window),
        max_concurrent: 1,
        access_mode: values.access_mode,
        member_ids: values.access_mode === 'selected' ? values.member_ids ?? [] : []
      }
      if (editingGroup) {
        const body: UpdateRepoGroupRequest = { ...base }
        return api.updateRepoGroup(editingGroup.id, body)
      }
      return api.createRepoGroup(base)
    },
    onSuccess: (group) => {
      setGroupOpen(false)
      setEditingGroup(null)
      groupForm.resetFields()
      invalidate()
      message.success(t('steam.groups.saved'))
      if (group?.id) setDetailGroupID(group.id)
    },
    onError: (err) => setError(err)
  })

  const deleteGroupMutation = useMutation({
    mutationFn: (id: string) => api.deleteRepoGroup(id),
    onSuccess: (_data, id) => {
      invalidate()
      if (detailGroupID === id) setDetailGroupID(null)
      if (pickerGroupID === id) setPickerGroupID(null)
      message.success(t('steam.groups.deleted'))
    },
    onError: (err) => setError(err)
  })

  const openCreateGroup = (): void => {
    setEditingGroup(null)
    setError(null)
    groupForm.resetFields()
    groupForm.setFieldsValue({
      access_mode: 'all',
      member_ids: [],
      auto_update: true,
      update_window: defaultUpdateWindow()
    })
    setGroupOpen(true)
  }

  const openEditGroup = (group: RepoGroupDTO): void => {
    setEditingGroup(group)
    setError(null)
    groupForm.resetFields()
    groupForm.setFieldsValue({
      name: group.name,
      description: group.description,
      steam_account_id: group.steam_account_id,
      access_mode: group.access_mode,
      member_ids: group.member_ids ?? [],
      auto_update: group.auto_update,
      update_window: parseUpdateWindow(group.update_window) ?? defaultUpdateWindow()
    })
    setGroupOpen(true)
  }

  if (pickerGroup) {
    return (
      <SteamGamePicker
        group={pickerGroup}
        account={pickerAccount}
        onBack={() => setPickerGroupID(null)}
        onDone={() => {
          setPickerGroupID(null)
          setDetailGroupID(pickerGroup.id)
        }}
      />
    )
  }

  if (detailGroup) {
    return (
      <>
        <GroupDetailPage
          group={detailGroup}
          account={accounts.find((account) => account.id === detailGroup.steam_account_id)}
          onBack={() => {
            setDetailGroupID(null)
            if (queryGroupID) {
              const next = new URLSearchParams(searchParams)
              next.delete('group')
              setSearchParams(next, { replace: true })
            }
          }}
          onAdd={() => setPickerGroupID(detailGroup.id)}
          onEdit={() => openEditGroup(detailGroup)}
          onDelete={() => deleteGroupMutation.mutate(detailGroup.id)}
        />
        <GroupFormModal
          open={groupOpen}
          editing={editingGroup}
          form={groupForm}
          accounts={accounts}
          users={users}
          loading={saveGroupMutation.isPending}
          error={error}
          onCancel={() => setGroupOpen(false)}
          onSubmit={(values) => saveGroupMutation.mutate(values)}
        />
      </>
    )
  }

  return (
    <PageShell
      title={t('steam.groups.title')}
      subtitle={t('steam.groups.subtitle')}
      extra={
        <Space>
          <Button onClick={() => void groupsQuery.refetch()}>{t('common.refresh')}</Button>
          <Button onClick={() => setAccountsOpen(true)}>{t('steam.accounts.manage')}</Button>
          <Button type="primary" onClick={openCreateGroup}>
            {t('steam.groups.create')}
          </Button>
        </Space>
      }
    >
      <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
        <ErrorNotice error={error} />
        {groups.length === 0 && !groupsQuery.isLoading ? (
          <Empty description={t('steam.groups.empty')}>
            <Button type="primary" onClick={openCreateGroup}>
              {t('steam.groups.create')}
            </Button>
          </Empty>
        ) : (
          <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
            {groups.map((group) => (
              <GroupRow
                key={group.id}
                group={group}
                account={accounts.find((account) => account.id === group.steam_account_id)}
                onOpen={() => setDetailGroupID(group.id)}
                onAdd={() => setPickerGroupID(group.id)}
                onEdit={() => openEditGroup(group)}
                onDelete={() => deleteGroupMutation.mutate(group.id)}
              />
            ))}
          </Space>
        )}
      </Space>

      <GroupFormModal
        open={groupOpen}
        editing={editingGroup}
        form={groupForm}
        accounts={accounts}
        users={users}
        loading={saveGroupMutation.isPending}
        error={error}
        onCancel={() => setGroupOpen(false)}
        onSubmit={(values) => saveGroupMutation.mutate(values)}
      />

      <Modal
        open={accountsOpen}
        title={t('steam.accounts.manageTitle')}
        onCancel={() => setAccountsOpen(false)}
        footer={null}
        width={1240}
        destroyOnClose
        style={{ top: 24 }}
        styles={{ body: { maxHeight: '80vh', overflow: 'auto' } }}
      >
        <SteamAccountsPage embedded />
      </Modal>
    </PageShell>
  )
}

function GroupRow({
  group,
  account,
  onOpen,
  onAdd,
  onEdit,
  onDelete
}: {
  group: RepoGroupDTO
  account?: AccountOption
  onOpen: () => void
  onAdd: () => void
  onEdit: () => void
  onDelete: () => void
}): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const gamesQuery = useQuery({
    queryKey: ['steam', 'games', group.id],
    queryFn: () => api.listGames(group.id),
    staleTime: 60_000
  })
  const games = gamesQuery.data?.items ?? []
  const first = games[0]

  return (
    <div
      onClick={onOpen}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: spacing.md,
        border: `1px solid ${palette.border}`,
        borderRadius: 12,
        padding: `${spacing.sm}px ${spacing.md}px`,
        background: palette.bgContainer,
        cursor: 'pointer',
        minHeight: 64
      }}
    >
      <SteamAppIcon
        appID={first?.app_id ?? 0}
        name={group.name}
        size={48}
        radius={10}
      />
      <div style={{ flex: '1 1 260px', minWidth: 0 }}>
        <Space size={4} wrap>
          <Typography.Text strong ellipsis={{ tooltip: group.name }}>
            {group.name}
          </Typography.Text>
          <Tag color="geekblue" style={{ marginInlineEnd: 0 }}>
            {accessLabel(t, group)}
          </Tag>
          <Tag style={{ marginInlineEnd: 0 }}>{account?.username || t('steam.groups.noAccount')}</Tag>
          {account && account.status !== 'active' ? (
            <Tag color="orange" style={{ marginInlineEnd: 0 }}>
              {t(`steam.status.${account.status}`)}
            </Tag>
          ) : null}
          <Tag style={{ marginInlineEnd: 0 }}>
            {t('steam.groups.gameCountValue', {
              count: gamesQuery.isFetched ? games.length : group.game_count
            })}
          </Tag>
          <Tag color={group.auto_update ? 'green' : 'default'} style={{ marginInlineEnd: 0 }}>
            {group.auto_update
              ? t('steam.groups.autoUpdateTag', {
                  window: group.update_window || t('steam.groups.anyTime')
                })
              : t('steam.groups.autoUpdateOff')}
          </Tag>
        </Space>
        <Typography.Text
          type="secondary"
          ellipsis={{ tooltip: group.description }}
          style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}
        >
          {group.description || '-'}
        </Typography.Text>
      </div>
      <Space wrap onClick={(event) => event.stopPropagation()} style={{ flexShrink: 0 }}>
        <Button size="small" type="primary" onClick={onAdd}>
          {t('steam.games.add')}
        </Button>
        <Button size="small" onClick={onOpen}>
          {t('steam.groups.open')}
        </Button>
        <Button size="small" onClick={onEdit}>
          {t('common.edit')}
        </Button>
        <Popconfirm
          title={t('steam.groups.deleteConfirm')}
          okText={t('common.delete')}
          cancelText={t('common.cancel')}
          onConfirm={onDelete}
        >
          <Button size="small" danger>
            {t('common.delete')}
          </Button>
        </Popconfirm>
      </Space>
    </div>
  )
}

function GroupDetailPage({
  group,
  account,
  onBack,
  onAdd,
  onEdit,
  onDelete
}: {
  group: RepoGroupDTO
  account?: AccountOption
  onBack: () => void
  onAdd: () => void
  onEdit: () => void
  onDelete: () => void
}): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { message } = App.useApp()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)

  const gamesQuery = useQuery({
    queryKey: ['steam', 'games', group.id],
    queryFn: () => api.listGames(group.id),
    refetchInterval: (query) => {
      const items = (query.state.data as { items?: GameDTO[] } | undefined)?.items ?? []
      return items.some((game) => game.status === 'updating') ? 2000 : false
    }
  })

  const checkMutation = useMutation({
    mutationFn: () => api.checkGroupUpdates(group.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['steam', 'games', group.id] })
      message.success(t('steam.games.checked'))
    },
    onError: (err) => setError(err)
  })

  const updateMutation = useMutation({
    mutationFn: (gameId: string) => api.updateGameNow(gameId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['steam', 'games', group.id] })
      message.success(t('steam.games.updateQueued'))
    },
    onError: (err) => setError(err)
  })

  const controlMutation = useMutation({
    mutationFn: ({ gameId, action }: { gameId: string; action: 'pause' | 'stop' }) =>
      action === 'pause' ? api.pauseGameUpdate(gameId) : api.stopGameUpdate(gameId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['steam', 'games', group.id] })
      void queryClient.invalidateQueries({ queryKey: ['steam', 'groups'] })
    },
    onError: (err) => setError(err)
  })

  const deleteMutation = useMutation({
    mutationFn: (gameId: string) => api.deleteGame(gameId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['steam', 'games', group.id] })
      void queryClient.invalidateQueries({ queryKey: ['steam', 'groups'] })
      message.success(t('steam.games.deleted'))
    },
    onError: (err) => setError(err)
  })

  const games = gamesQuery.data?.items ?? []

  return (
    <PageShell
      title={group.name}
      titleAfter={
        <Space size={4} wrap>
          <Tag color="geekblue">{accessLabel(t, group)}</Tag>
          <Tag>{account?.username || t('steam.groups.noAccount')}</Tag>
          <Tag>{t('steam.groups.gameCountValue', { count: group.game_count })}</Tag>
          <Tag color={group.auto_update ? 'green' : 'default'}>
            {group.auto_update
              ? t('steam.groups.autoUpdateTag', {
                  window: group.update_window || t('steam.groups.anyTime')
                })
              : t('steam.groups.autoUpdateOff')}
          </Tag>
        </Space>
      }
      subtitle={group.description || undefined}
      leading={<Button onClick={onBack}>{t('common.back')}</Button>}
      extra={
        <Space wrap>
          <Button loading={checkMutation.isPending} onClick={() => checkMutation.mutate()}>
            {t('steam.games.check')}
          </Button>
          <Button type="primary" onClick={onAdd}>
            {t('steam.games.add')}
          </Button>
          <Button onClick={onEdit}>{t('common.edit')}</Button>
          <Popconfirm title={t('steam.groups.deleteConfirm')} okText={t('common.delete')} cancelText={t('common.cancel')} onConfirm={onDelete}>
            <Button danger>{t('common.delete')}</Button>
          </Popconfirm>
        </Space>
      }
    >
      <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
        <ErrorNotice error={error} />
        <ErrorNotice error={gamesQuery.error} />
        {games.length === 0 && !gamesQuery.isLoading ? (
          <Empty description={t('steam.groups.noGames')}>
            <Button type="primary" onClick={onAdd}>
              {t('steam.groups.addFirstGame')}
            </Button>
          </Empty>
        ) : (
          <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
            {games.map((game) => (
              <GameLibraryRow
                key={game.id}
                game={game}
                onUpdate={() => updateMutation.mutate(game.id)}
                onPause={() => controlMutation.mutate({ gameId: game.id, action: 'pause' })}
                onStop={() => controlMutation.mutate({ gameId: game.id, action: 'stop' })}
                onDelete={() => deleteMutation.mutate(game.id)}
                updating={updateMutation.isPending && updateMutation.variables === game.id}
                controlling={controlMutation.isPending && controlMutation.variables?.gameId === game.id}
              />
            ))}
          </Space>
        )}
      </Space>
    </PageShell>
  )
}

function GameLibraryRow({
  game,
  onUpdate,
  onPause,
  onStop,
  onDelete,
  updating,
  controlling
}: {
  game: GameDTO
  onUpdate: () => void
  onPause: () => void
  onStop: () => void
  onDelete: () => void
  updating: boolean
  controlling: boolean
}): JSX.Element {
  const { t } = useI18n()
  const [outputOpen, setOutputOpen] = useState(false)
  const updatingGame = game.status === 'updating'
  const progress = steamUpdateProgress(game, t)

  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: spacing.md,
        flexWrap: 'wrap',
        border: `1px solid ${palette.border}`,
        borderRadius: 12,
        padding: `${spacing.sm}px ${spacing.md}px`,
        background: palette.bgContainer
      }}
    >
      <SteamAppIcon appID={game.app_id} name={game.app_name} size={44} radius={9} />

      <div style={{ flex: '1 1 240px', minWidth: 0 }}>
        <Space size={4} wrap>
          <Typography.Text strong ellipsis={{ tooltip: game.app_name }}>
            {game.app_name}
          </Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            App {game.app_id}
          </Typography.Text>
          {/* 状态标签放最后：实心、小一号，不再抢在名字和 AppID 之间打断阅读。 */}
          {game.status !== 'ready' ? (
            <Tag
              color={gameStatusColor(game.status)}
              bordered={false}
              style={{
                marginInlineEnd: 0,
                fontSize: 11,
                lineHeight: '16px',
                padding: '0 6px',
                borderRadius: 4
              }}
            >
              {t(`steam.gameStatus.${game.status}`)}
            </Tag>
          ) : null}
        </Space>
        <Typography.Text
          type="secondary"
          ellipsis={{
            tooltip: `Build: ${game.build_id || '-'}${
              game.available_build_id ? ` → ${game.available_build_id}` : ''
            } · ${t('steam.games.lastUpdated')}: ${
              game.last_updated_at ? formatTime(game.last_updated_at) : '-'
            }${game.last_error ? ` · ${game.last_error}` : ''}`
          }}
          style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}
        >
          Build: {game.build_id || '-'}
          {game.available_build_id ? ` → ${game.available_build_id}` : ''}
          {' · '}
          {t('steam.games.lastUpdated')}: {game.last_updated_at ? formatTime(game.last_updated_at) : '-'}
          {game.last_error ? (
            <>
              {' · '}
              <span style={{ color: palette.danger }}>{game.last_error}</span>
            </>
          ) : null}
        </Typography.Text>
      </div>

      {updatingGame ? (
        <SteamUpdateMeter progress={progress} style={{ flex: '0 1 320px', minWidth: 220 }} />
      ) : null}

      <Space wrap style={{ flexShrink: 0 }}>
        {updatingGame ? (
          game.can_stop ? (
            <>
              <Button size="small" loading={controlling} onClick={onPause}>
                {t('steam.games.pauseUpdate')}
              </Button>
              <Button size="small" danger loading={controlling} onClick={onStop}>
                {t('steam.games.stopUpdate')}
              </Button>
            </>
          ) : null
        ) : (
          <Button size="small" type="primary" loading={updating} onClick={onUpdate}>
            {game.status === 'paused' ? t('steam.games.resumeUpdate') : t('steam.games.updateNow')}
          </Button>
        )}
        <Button size="small" onClick={() => setOutputOpen(true)}>
          {t('steam.games.viewOutput')}
        </Button>
        <Popconfirm
          title={t('steam.games.deleteConfirm')}
          okText={t('common.delete')}
          cancelText={t('common.cancel')}
          onConfirm={onDelete}
        >
          <Button size="small" danger disabled={updatingGame}>
            {t('common.delete')}
          </Button>
        </Popconfirm>
      </Space>

      <GameOutputModal open={outputOpen} game={game} onClose={() => setOutputOpen(false)} />
    </div>
  )
}

/** SteamCMD 原始输出：更新中每秒刷新，更新结束后可回看最后一次输出。 */
function GameOutputModal({
  open,
  game,
  onClose
}: {
  open: boolean
  game: GameDTO
  onClose: () => void
}): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const preRef = useRef<HTMLPreElement>(null)
  const outputQuery = useQuery({
    queryKey: ['steam', 'game-output', game.id],
    queryFn: () => api.getGameOutput(game.id),
    enabled: open,
    refetchInterval: open && game.status === 'updating' ? 1000 : false
  })
  const lines = outputQuery.data?.items ?? []

  useEffect(() => {
    if (!open) return
    const node = preRef.current
    if (node) node.scrollTop = node.scrollHeight
  }, [open, lines])

  return (
    <Modal
      open={open}
      title={`${t('steam.games.outputTitle')} · ${game.app_name}`}
      onCancel={onClose}
      footer={null}
      width={900}
      destroyOnClose
      styles={{ body: { maxHeight: '70vh', overflow: 'auto' } }}
    >
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('steam.games.outputHint')}
      </Typography.Text>
      <pre
        ref={preRef}
        className="selectable"
        style={{
          marginTop: spacing.sm,
          padding: spacing.sm,
          background: 'rgba(0,0,0,0.04)',
          borderRadius: 6,
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          fontSize: 12,
          maxHeight: '55vh',
          overflow: 'auto'
        }}
      >
        {lines.length > 0 ? lines.join('\n') : t('steam.games.outputEmpty')}
      </pre>
    </Modal>
  )
}

function GroupFormModal({
  open,
  editing,
  form,
  accounts,
  users,
  loading,
  error,
  onCancel,
  onSubmit
}: {
  open: boolean
  editing: RepoGroupDTO | null
  form: FormInstance<GroupFormValues>
  accounts: AccountOption[]
  users: UserDTO[]
  loading: boolean
  error: unknown
  onCancel: () => void
  onSubmit: (values: GroupFormValues) => void
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Modal
      open={open}
      title={editing ? t('steam.groups.editTitle') : t('steam.groups.createTitle')}
      onCancel={onCancel}
      onOk={() => form.submit()}
      confirmLoading={loading}
      destroyOnClose
      width={640}
    >
      <Form<GroupFormValues> form={form} layout="vertical" onFinish={onSubmit}>
        <Form.Item name="name" label={t('steam.groups.name')} rules={[{ required: true }]}>
          <Input />
        </Form.Item>
        <Form.Item name="description" label={t('steam.groups.description')}>
          <Input.TextArea rows={2} />
        </Form.Item>
        <Form.Item name="steam_account_id" label={t('steam.groups.account')} rules={[{ required: true }]}>
          <Select
            showSearch
            optionFilterProp="label"
            options={accounts.map((account) => ({
              value: account.id,
              label: `${account.username}${account.display_name ? ` (${account.display_name})` : ''} · ${t(`steam.status.${account.status}`)}`
            }))}
          />
        </Form.Item>
        <Form.Item name="access_mode" label={t('steam.groups.access')} rules={[{ required: true }]}>
          <Select
            options={[
              { value: 'all', label: t('steam.access.all') },
              { value: 'selected', label: t('steam.access.selected') }
            ]}
          />
        </Form.Item>
        <Form.Item noStyle shouldUpdate={(prev, next) => prev.access_mode !== next.access_mode}>
          {({ getFieldValue }) =>
            getFieldValue('access_mode') === 'selected' ? (
              <Form.Item
                name="member_ids"
                label={t('steam.groups.members')}
                rules={[{ required: true, message: t('steam.groups.membersRequired') }]}
              >
                <Select
                  mode="multiple"
                  showSearch
                  optionFilterProp="label"
                  placeholder={t('steam.groups.membersHint')}
                  options={users.map((user) => ({
                    value: user.id,
                    label: `${user.username}${user.remark ? ` (${user.remark})` : ''}`
                  }))}
                />
              </Form.Item>
            ) : null
          }
        </Form.Item>
        <Form.Item
          name="auto_update"
          label={t('steam.groups.autoUpdate')}
          valuePropName="checked"
          extra={t('steam.groups.autoUpdateHint')}
        >
          <Switch />
        </Form.Item>
        <Form.Item noStyle shouldUpdate={(prev, next) => prev.auto_update !== next.auto_update}>
          {({ getFieldValue }) =>
            getFieldValue('auto_update') ? (
              <Form.Item
                name="update_window"
                label={t('steam.groups.updateWindow')}
                extra={t('steam.groups.updateWindowHint')}
              >
                <TimePicker.RangePicker format="HH:mm" minuteStep={5} style={{ width: '100%' }} />
              </Form.Item>
            ) : null
          }
        </Form.Item>
        <ErrorNotice error={error} />
      </Form>
    </Modal>
  )
}
