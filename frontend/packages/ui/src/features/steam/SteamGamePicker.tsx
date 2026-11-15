import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Alert, App, Button, Checkbox, Empty, Input, Modal, Space, Spin, Tag, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { fetchLocalSteamLibrary } from '../../api/agentClient'
import { errorCodeOf } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { RepoGroupDTO, SteamStoreGameDTO } from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { SteamAppIcon } from './SteamAppIcon'

const MAX_BATCH_GAMES = 50

function isWindowsGame(game: SteamStoreGameDTO): boolean {
  const platforms = game.platforms ?? []
  return platforms.length === 0 || platforms.includes('windows')
}

interface SteamGamePickerProps {
  group: RepoGroupDTO
  account?: { username: string; status: string }
  onBack: () => void
  onDone: () => void
}

/** 整页游戏选择器：替代原先拥挤的批量添加弹窗。 */
export function SteamGamePicker({ group, account, onBack, onDone }: SteamGamePickerProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { message } = App.useApp()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [storeQuery, setStoreQuery] = useState('')
  const [selectedGames, setSelectedGames] = useState<Record<number, SteamStoreGameDTO>>({})
  const [preferServerList, setPreferServerList] = useState(false)
  const [guardOpen, setGuardOpen] = useState(false)
  const [guardCode, setGuardCode] = useState('')

  const localLibraryQuery = useQuery({
    queryKey: ['steam', 'local-library'],
    queryFn: () => fetchLocalSteamLibrary(),
    staleTime: 30_000,
    retry: false
  })
  const localLibrary = localLibraryQuery.data
  const localAvailable = Boolean(localLibrary?.available && localLibrary.games.length > 0)
  const useLocalList = localAvailable && !preferServerList

  const ownedGamesQuery = useQuery({
    queryKey: ['steam', 'owned-games', group.steam_account_id],
    queryFn: () => api.listSteamOwnedGames(group.steam_account_id!, undefined, 'CN'),
    enabled: Boolean(group.steam_account_id) && localLibraryQuery.isFetched && !useLocalList,
    staleTime: 5 * 60_000,
    refetchInterval: 60_000,
    retry: false
  })

  const localGames = useMemo<SteamStoreGameDTO[]>(
    () =>
      (localLibrary?.games ?? []).map((game) => ({
        app_id: game.app_id,
        name: game.name,
        image: '',
        platforms: [],
        owned: true,
        installed: game.installed,
        has_local_image: game.has_image
      })),
    [localLibrary]
  )

  const localGamesByID = useMemo(
    () => new Map(localGames.map((game) => [game.app_id, game])),
    [localGames]
  )
  const serverGames = useMemo(
    () =>
      (ownedGamesQuery.data?.items ?? []).map((game) => {
        const local = localGamesByID.get(game.app_id)
        return /^App \d+$/.test(game.name) && local ? { ...game, name: local.name } : game
      }),
    [ownedGamesQuery.data, localGamesByID]
  )
  const games = useMemo(() => {
    const source = useLocalList ? localGames : serverGames
    // 服务端/旧客户端可能仍返回 "App 123" 占位条目：没有商店信息，不进入选择器。
    return source.filter((game) => !/^App \d+$/.test(game.name.trim()))
  }, [useLocalList, localGames, serverGames])

  const displayGames = useMemo(() => {
    const keyword = storeQuery.trim().toLowerCase()
    if (!keyword) return games
    return games.filter(
      (game) => game.name.toLowerCase().includes(keyword) || String(game.app_id).includes(keyword)
    )
  }, [games, storeQuery])
  const selectableGames = useMemo(() => displayGames.filter(isWindowsGame), [displayGames])
  const unresolvedGames = useMemo(
    () => (useLocalList ? 0 : games.filter((game) => /^App \d+$/.test(game.name)).length),
    [useLocalList, games]
  )

  const localAccountMismatch = Boolean(
    localLibrary?.account_name &&
      account?.username &&
      localLibrary.account_name.toLowerCase() !== account.username.toLowerCase()
  )

  const importedRef = useRef('')
  useEffect(() => {
    const accountID = group.steam_account_id
    if (!accountID || !account || !localLibrary?.available || localLibrary.games.length === 0) return
    if (localAccountMismatch) return
    const key = `${accountID}:${localLibrary.games.length}:${localLibrary.steam_root ?? ''}`
    if (importedRef.current === key) return
    importedRef.current = key
    void api
      .importSteamClientGames(
        accountID,
        localLibrary.games.map((game) => ({
          app_id: game.app_id,
          name: game.name,
          installed: game.installed
        }))
      )
      .then(() => {
        void queryClient.invalidateQueries({ queryKey: ['steam', 'owned-games', accountID] })
      })
      .catch(() => {
        importedRef.current = ''
      })
  }, [localLibrary, localAccountMismatch, account, group.steam_account_id, api, queryClient])

  const rescanMutation = useMutation({
    mutationFn: () => fetchLocalSteamLibrary(true),
    onSuccess: (data) => {
      if (data) queryClient.setQueryData(['steam', 'local-library'], data)
    }
  })

  const licenseSyncMutation = useMutation({
    mutationFn: (code?: string) => api.syncSteamLicenses(group.steam_account_id!, code),
    onSuccess: (res) => {
      setError(null)
      setGuardOpen(false)
      setGuardCode('')
      void queryClient.invalidateQueries({ queryKey: ['steam', 'owned-games', group.steam_account_id] })
      void queryClient.invalidateQueries({ queryKey: ['steam', 'accounts'] })
      message.success(t('steam.accounts.licenseOk', { count: res.items.length }))
    },
    onError: (err) => {
      setError(err)
      const code = errorCodeOf(err)
      setGuardOpen(code === 'steam.account_needs_2fa' || code === 'steam.account_login_failed')
    }
  })

  const batchMutation = useMutation({
    mutationFn: () =>
      api.batchCreateGames(group.id, {
        size_bytes: 0,
        games: Object.values(selectedGames).map((game) => ({
          app_id: game.app_id,
          app_name: game.name
        }))
      }),
    onSuccess: (res) => {
      void queryClient.invalidateQueries({ queryKey: ['steam', 'games', group.id] })
      void queryClient.invalidateQueries({ queryKey: ['steam', 'groups'] })
      void queryClient.invalidateQueries({ queryKey: ['steam', 'accounts'] })
      if (res.failed.length === 0) {
        message.success(t('steam.games.batchCreated', { count: res.created.length }))
        onDone()
        return
      }
      const created = new Set(res.created.map((game) => game.app_id))
      setSelectedGames((previous) => {
        const next: Record<number, SteamStoreGameDTO> = {}
        for (const [key, game] of Object.entries(previous)) {
          if (!created.has(game.app_id)) next[Number(key)] = game
        }
        return next
      })
      message.warning(t('steam.games.batchPartial', { created: res.created.length, failed: res.failed.length }))
    },
    onError: (err) => setError(err)
  })

  const toggleGame = (game: SteamStoreGameDTO): void => {
    if (!isWindowsGame(game)) return
    setSelectedGames((previous) => {
      const next = { ...previous }
      if (next[game.app_id]) {
        delete next[game.app_id]
        return next
      }
      if (Object.keys(next).length >= MAX_BATCH_GAMES) {
        message.warning(t('steam.games.maxSelected', { max: MAX_BATCH_GAMES }))
        return previous
      }
      next[game.app_id] = game
      return next
    })
  }

  const allSelected =
    selectableGames.length > 0 && selectableGames.every((game) => Boolean(selectedGames[game.app_id]))

  const toggleAll = (): void => {
    if (allSelected) {
      setSelectedGames((previous) => {
        const next = { ...previous }
        for (const game of selectableGames) delete next[game.app_id]
        return next
      })
      return
    }
    const next = { ...selectedGames }
    let capped = false
    for (const game of selectableGames) {
      if (Object.keys(next).length >= MAX_BATCH_GAMES) {
        capped = true
        break
      }
      next[game.app_id] = game
    }
    if (capped) message.warning(t('steam.games.maxSelected', { max: MAX_BATCH_GAMES }))
    setSelectedGames(next)
  }

  const selectedCount = Object.keys(selectedGames).length
  const loading = localLibraryQuery.isLoading || (!useLocalList && ownedGamesQuery.isLoading)

  return (
    <PageShell
      title={t('steam.games.pickerTitle', { name: group.name })}
      leading={<Button onClick={onBack}>{t('common.back')}</Button>}
      extra={
        <Space wrap>
          <Tag color={selectedCount > 0 ? 'blue' : 'default'} style={{ margin: 0 }}>
            {t('steam.games.selectedCount', { count: selectedCount })}
          </Tag>
          <Button onClick={onBack}>{t('common.cancel')}</Button>
          <Button
            type="primary"
            loading={batchMutation.isPending}
            disabled={selectedCount === 0}
            onClick={() => batchMutation.mutate()}
          >
            {t('steam.games.addSelected', { count: selectedCount })}
          </Button>
        </Space>
      }
    >
      <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
        {localAvailable ? (
          <Space wrap>
            <Tag color={useLocalList ? 'green' : 'blue'} style={{ margin: 0 }}>
              {useLocalList
                ? t('steam.games.localSource', { count: localGames.length })
                : t('steam.games.usingServerList', { count: games.length })}
            </Tag>
            {useLocalList && localLibrary?.account_name ? (
              <Typography.Text type="secondary">{localLibrary.account_name}</Typography.Text>
            ) : null}
            <Button size="small" loading={rescanMutation.isPending} onClick={() => rescanMutation.mutate()}>
              {t('steam.games.rescanLocal')}
            </Button>
            <Button size="small" onClick={() => setPreferServerList(!preferServerList)}>
              {useLocalList ? t('steam.games.useServerList') : t('steam.games.useLocalList')}
            </Button>
          </Space>
        ) : localLibraryQuery.isFetched ? (
          <Space wrap>
            <Tag style={{ margin: 0 }}>{t('steam.games.localUnavailable')}</Tag>
            <Button size="small" loading={rescanMutation.isPending} onClick={() => rescanMutation.mutate()}>
              {t('steam.games.rescanLocal')}
            </Button>
          </Space>
        ) : null}

        {localAccountMismatch ? (
          <Alert
            type="warning"
            showIcon
            message={t('steam.games.localAccountMismatch', {
              local: localLibrary?.account_name || '-',
              server: account?.username || '-'
            })}
          />
        ) : null}

        {account && (account.status === 'needs_2fa' || account.status === 'error') ? (
          <Alert
            type="warning"
            showIcon
            message={t('steam.games.accountNeedsLogin', { username: account.username })}
            action={
              <Button size="small" loading={licenseSyncMutation.isPending} onClick={() => licenseSyncMutation.mutate(undefined)}>
                {t('steam.games.syncLicenses')}
              </Button>
            }
          />
        ) : null}

        {unresolvedGames > 0 ? (
          <Alert
            type="warning"
            showIcon
            message={t('steam.games.metadataIncomplete', { count: unresolvedGames })}
            action={
              <Button size="small" loading={ownedGamesQuery.isFetching} onClick={() => void ownedGamesQuery.refetch()}>
                {t('common.retry')}
              </Button>
            }
          />
        ) : null}

        <ErrorNotice error={error} />
        <ErrorNotice error={ownedGamesQuery.error} />

        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: spacing.md,
            flexWrap: 'wrap'
          }}
        >
          <Space wrap>
            <Input.Search
              allowClear
              value={storeQuery}
              placeholder={t('steam.games.filterPlaceholder')}
              onChange={(event) => setStoreQuery(event.target.value)}
              style={{ width: 360 }}
            />
            <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
              {t('steam.games.ownedCount', { count: games.length })}
            </Typography.Text>
            <Button
              size="small"
              loading={useLocalList ? rescanMutation.isPending : ownedGamesQuery.isFetching}
              onClick={() => {
                if (useLocalList) rescanMutation.mutate()
                else void ownedGamesQuery.refetch()
              }}
            >
              {t('common.refresh')}
            </Button>
            <Button size="small" loading={licenseSyncMutation.isPending} onClick={() => licenseSyncMutation.mutate(undefined)}>
              {t('steam.games.syncLicenses')}
            </Button>
          </Space>
          <Checkbox checked={allSelected} onChange={toggleAll}>
            {t('steam.games.selectAll')}
          </Checkbox>
        </div>

        {loading ? (
          <div style={{ padding: spacing.xl, textAlign: 'center' }}>
            <Spin />
          </div>
        ) : displayGames.length === 0 ? (
          <Empty description={t('steam.games.searchEmpty')} />
        ) : (
          <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
            {displayGames.map((game) => (
              <GamePickRow
                key={game.app_id}
                game={game}
                selected={Boolean(selectedGames[game.app_id])}
                onToggle={() => toggleGame(game)}
              />
            ))}
          </Space>
        )}
      </Space>

      <ModalShellGuard
        open={guardOpen}
        onCancel={() => setGuardOpen(false)}
        onOk={() => licenseSyncMutation.mutate(guardCode.trim())}
        loading={licenseSyncMutation.isPending}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Typography.Text type="secondary">{t('steam.accounts.guardCodeHint')}</Typography.Text>
          <Input value={guardCode} onChange={(event) => setGuardCode(event.target.value)} autoFocus />
        </Space>
      </ModalShellGuard>
    </PageShell>
  )
}

function GamePickRow({
  game,
  selected,
  onToggle
}: {
  game: SteamStoreGameDTO
  selected: boolean
  onToggle: () => void
}): JSX.Element {
  const { t } = useI18n()
  const windows = isWindowsGame(game)
  return (
    <div
      onClick={windows ? onToggle : undefined}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: spacing.sm,
        border: selected ? `2px solid ${palette.accent}` : `1px solid ${palette.border}`,
        borderRadius: 10,
        padding: `${spacing.xs}px ${spacing.md}px`,
        background: palette.bgContainer,
        cursor: windows ? 'pointer' : 'not-allowed',
        opacity: windows ? 1 : 0.55,
        transition: 'border-color 0.15s ease'
      }}
    >
      <Checkbox
        checked={selected}
        disabled={!windows}
        aria-label={t('steam.games.selectGame')}
        onClick={(event) => event.stopPropagation()}
        onChange={() => {
          if (windows) onToggle()
        }}
      />
      <SteamAppIcon
        appID={game.app_id}
        image={game.image || undefined}
        name={game.name}
        size={40}
      />
      <div style={{ flex: 1, minWidth: 0 }}>
        <Typography.Text strong ellipsis={{ tooltip: game.name }} style={{ display: 'block' }}>
          {game.name}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          App {game.app_id}
        </Typography.Text>
      </div>
      {game.installed ? <Tag color="green">{t('steam.games.installed')}</Tag> : null}
      {windows ? null : <Tag>{t('steam.games.notWindows')}</Tag>}
    </div>
  )
}

/** 验证码弹窗的小外壳（只有真正需要时出现）。 */
function ModalShellGuard({
  open,
  onCancel,
  onOk,
  loading,
  children
}: {
  open: boolean
  onCancel: () => void
  onOk: () => void
  loading: boolean
  children: ReactNode
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Modal open={open} title={t('steam.accounts.guardCodeTitle')} onCancel={onCancel} onOk={onOk} confirmLoading={loading}>
      {children}
    </Modal>
  )
}
