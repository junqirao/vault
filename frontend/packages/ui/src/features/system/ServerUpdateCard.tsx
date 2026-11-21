import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Descriptions, Modal, Progress, Space, Tag, Typography } from 'antd'
import { CheckCircleOutlined, CloudDownloadOutlined, ReloadOutlined } from '@ant-design/icons'
import { useQueryClient } from '@tanstack/react-query'

import { useApi } from '../../api/provider'
import type { ServerUpdateStatus } from '../../api/types'
import { isApiError } from '../../api/errors'
import { hasKey, useI18n } from '../../i18n'
import { SectionCard } from '../../components/SectionCard'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'

/** 轮询间隔：下载/重启期间每秒拉一次；其余状态不轮询。 */
const ACTIVE_POLL_MS = 1000
/** 服务端重启后的探测间隔：慢一点，避免每次失败都刷错误。 */
const RESTART_POLL_MS = 1500
/**
 * 「已更新到 x」的成功提示只在**这次更新刚完成**时展示：
 * 页面停留期间看到更新结束，或结果发生在最近 10 分钟内。旧结果不再每次进页面都弹一遍。
 * 失败结果仍保留（需要管理员处理），但可以手动关闭。
 */
const RECENT_SUCCESS_MS = 10 * 60 * 1000

/**
 * 服务端自更新卡片（仅超级管理员可见）。
 *
 * 更新由服务端执行：浏览器/客户端只调用状态、检查、下载、应用接口；真正替换与重启
 * 由服务端的独立 helper 完成。这里不假设服务端一定会在线——应用更新后它会短暂断开，
 * 因此重启阶段用轮询等待恢复，而不是把网络错误当成失败。
 */
export function ServerUpdateCard(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [status, setStatus] = useState<ServerUpdateStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [unsupported, setUnsupported] = useState(false)
  const [restarting, setRestarting] = useState(false)
  const restartingRef = useRef(false)
  const lastReleaseRef = useRef<string>('')
  // 页面停留期间是否亲眼看到一次更新结束（服务端重启回来 / applying → idle）。
  const [completedDuringSession, setCompletedDuringSession] = useState(false)
  const applyingSeenRef = useRef(false)
  // 本次会话手动关闭过的结果（按结果指纹区分），关掉后不再反复弹出。
  const [dismissedResult, setDismissedResult] = useState('')

  const refresh = useCallback(async (silent = false): Promise<ServerUpdateStatus | null> => {
    try {
      const next = await api.serverUpdateStatus()
      setStatus(next)
      setUnsupported(false)
      setError(null)
      const wasRestarting = restartingRef.current
      if (wasRestarting && next.state !== 'applying' && next.state !== 'restarting') {
        // 服务端回来后，last_result/current version 会告诉我们结果。
        restartingRef.current = false
        setRestarting(false)
        setCompletedDuringSession(true)
      }
      // 别的管理员发起 / 页面打开时正好在更新：同样认作"本次看到了结束"。
      if (next.state === 'applying' || next.state === 'restarting') {
        applyingSeenRef.current = true
      } else if (applyingSeenRef.current) {
        applyingSeenRef.current = false
        setCompletedDuringSession(true)
      }
      const release = next.current.release_version
      if (release && release !== lastReleaseRef.current) {
        lastReleaseRef.current = release
        // 服务端更新/重启后，“版本”卡片里的发布版本必须立刻跟着变，不能等下一次
        // 手动刷新或页面重载。这里复用 React Query 缓存，只做一次主动失效。
        void queryClient.invalidateQueries({ queryKey: ['system-info'] })
        void queryClient.invalidateQueries({ queryKey: ['system-settings'] })
        void queryClient.invalidateQueries({ queryKey: ['system-health'] })
      } else if (wasRestarting && next.state !== 'applying' && next.state !== 'restarting') {
        // 极端情况下 release_version 没变（例如同版本重装），仍然要刷新版本卡片。
        void queryClient.invalidateQueries({ queryKey: ['system-info'] })
      }
      return next
    } catch (err) {
      if (!silent) {
        if (isApiError(err) && (err.status === 404 || err.status === 501)) {
          setUnsupported(true)
        } else {
          setError(err)
        }
      }
      return null
    } finally {
      setLoading(false)
    }
  }, [api, queryClient])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const activeState = status?.state === 'downloading' || status?.state === 'applying' || restarting
  useEffect(() => {
    if (!activeState) return
    const interval = window.setInterval(
      () => void refresh(true),
      restarting ? RESTART_POLL_MS : ACTIVE_POLL_MS
    )
    return () => window.clearInterval(interval)
  }, [activeState, restarting, refresh])

  const run = async (action: () => Promise<ServerUpdateStatus>): Promise<void> => {
    setBusy(true)
    setError(null)
    try {
      setStatus(await action())
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  const apply = (): void => {
    Modal.confirm({
      title: t('system.serverUpdate.applyConfirmTitle'),
      content: t('system.serverUpdate.applyConfirm'),
      okText: t('system.serverUpdate.apply'),
      cancelText: t('common.cancel'),
      onOk: async () => {
        setBusy(true)
        setError(null)
        try {
          const next = await api.applyServerUpdate()
          setStatus(next)
          restartingRef.current = true
          setRestarting(true)
        } catch (err) {
          setError(err)
          throw err
        } finally {
          setBusy(false)
        }
      }
    })
  }

  const codeText = (code?: string, fallback?: string, args?: Record<string, unknown>): string => {
    if (!code) return fallback || '-'
    const key = `err.${code}`
    return hasKey(key) ? t(key, args) : fallback || code
  }

  const progress = status?.progress
  const percent = progress && progress.percent >= 0 ? progress.percent : undefined
  const latest = status?.latest
  const manual = status?.restart_mode === 'manual'
  const lastResult = status?.last_result
  const resultKey = lastResult
    ? `${lastResult.finished_at ?? 0}:${lastResult.to_version ?? ''}:${lastResult.ok}`
    : ''
  const recentSuccess = Boolean(
    lastResult?.ok && lastResult.finished_at && Date.now() - lastResult.finished_at < RECENT_SUCCESS_MS
  )
  // 成功结果：只展示"页面期间刚完成"或"最近 10 分钟内完成"的那一次；
  // 失败结果保留给管理员处理。两种情况都可以手动关闭，关掉后本次会话不再重复弹。
  const showLastResult = Boolean(
    lastResult &&
      resultKey !== dismissedResult &&
      (!lastResult.ok || completedDuringSession || recentSuccess)
  )

  const checkButton = (
    <Button
      size="small"
      icon={<ReloadOutlined />}
      loading={busy && status?.state !== 'downloading' && status?.state !== 'applying'}
      disabled={busy || status?.state === 'downloading' || status?.state === 'applying' || status?.state === 'downloaded' || restarting}
      onClick={() => void run(() => api.checkServerUpdate())}
    >
      {status?.state === 'checking' ? t('system.serverUpdate.checking') : t('system.serverUpdate.check')}
    </Button>
  )

  return (
    <SectionCard
      title={t('system.serverUpdate.title')}
      loading={loading && !status}
      extra={unsupported ? undefined : checkButton}
    >
      {unsupported ? (
        <Alert
          type="info"
          showIcon
          message={t('system.serverUpdate.unsupported')}
          description={t('system.serverUpdate.unsupportedHint')}
        />
      ) : (
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          {error ? (
            <Alert
              type="error"
              showIcon
              message={t('system.serverUpdate.failed')}
              description={codeText(
                isApiError(error) ? error.code : undefined,
                isApiError(error) ? error.message : String(error),
                isApiError(error) ? error.args : undefined
              )}
            />
          ) : null}

          <Descriptions size="small" column={3} bordered>
            <Descriptions.Item label={t('system.serverUpdate.current')}>
              {status?.current.release_version ? `v${status.current.release_version}` : '-'}
              {status?.current.release_channel && status.current.release_channel !== 'stable' ? (
                <Tag style={{ marginInlineStart: spacing.xs }}>{status.current.release_channel.toUpperCase()}</Tag>
              ) : null}
            </Descriptions.Item>
            <Descriptions.Item label={t('system.serverUpdate.internal')}>
              {status?.current.internal_version || '-'}
            </Descriptions.Item>
            <Descriptions.Item label={t('system.serverUpdate.platform')}>
              {status ? `${status.current.platform}/${status.current.arch}` : '-'}
            </Descriptions.Item>
          </Descriptions>

          {!status?.current.release_version ? (
            <Alert type="warning" showIcon message={t('system.serverUpdate.noVersion')} />
          ) : null}

          {status?.error ? (
            <Alert
              type="error"
              showIcon
              message={t('system.serverUpdate.failed')}
              description={codeText(status.error.code, status.error.message, status.error.args)}
            />
          ) : null}

          {lastResult && showLastResult ? (
            <Alert
              type={lastResult.ok ? 'success' : 'warning'}
              showIcon
              closable
              icon={lastResult.ok ? <CheckCircleOutlined /> : undefined}
              onClose={() => setDismissedResult(resultKey)}
              message={
                lastResult.ok
                  ? t('system.serverUpdate.lastSuccess', { version: lastResult.to_version || '-' })
                  : t('system.serverUpdate.lastFailed', {
                      error: lastResult.error || t('common.failed'),
                      time: lastResult.finished_at ? formatTime(lastResult.finished_at) : '-'
                    })
              }
            />
          ) : null}

          {latest ? (
            <div>
              <Space size={spacing.xs} wrap align="center">
                <Typography.Text strong>
                  {t('system.serverUpdate.available', { version: latest.version })}
                </Typography.Text>
                {latest.channel && latest.channel !== 'stable' ? <Tag>{latest.channel.toUpperCase()}</Tag> : null}
                <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                  {t('system.serverUpdate.size')}: {formatBytes(latest.asset_size)}
                </Typography.Text>
              </Space>
              {latest.notes ? (
                <Typography.Paragraph
                  type="secondary"
                  style={{ fontSize: fontSize.xs, marginTop: spacing.xs, maxHeight: 120, overflow: 'auto', whiteSpace: 'pre-wrap' }}
                >
                  {latest.notes}
                </Typography.Paragraph>
              ) : null}
            </div>
          ) : status && status.state === 'idle' && status.checked_at ? (
            <Typography.Text type="success">
              <CheckCircleOutlined /> {t('system.serverUpdate.upToDate')}
            </Typography.Text>
          ) : null}

          {status?.state === 'downloading' || restarting ? (
            <div>
              <Progress
                percent={percent}
                status={percent === undefined ? 'active' : undefined}
                size="small"
              />
              <Space size={spacing.sm} wrap>
                {progress ? (
                  <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                    {t('system.serverUpdate.progress', {
                      received: formatBytes(progress.received_bytes),
                      total: formatBytes(progress.total_bytes)
                    })}
                  </Typography.Text>
                ) : null}
                {progress && progress.speed_bytes_per_sec > 0 ? (
                  <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                    {formatBytes(progress.speed_bytes_per_sec)}/s
                  </Typography.Text>
                ) : null}
              </Space>
            </div>
          ) : null}

          {status?.state === 'applying' || restarting ? (
            <Alert
              type="info"
              showIcon
              message={t('system.serverUpdate.restarting')}
              description={t('system.serverUpdate.restartingHint')}
            />
          ) : null}

          {manual && latest ? (
            <Alert type="warning" showIcon message={t('system.serverUpdate.manual')} />
          ) : null}

          {(latest && (status?.state === 'available' || status?.state === 'failed')) ||
          status?.state === 'downloading' ||
          status?.state === 'downloaded' ? (
            <Space size={spacing.sm} wrap>
              {latest && (status?.state === 'available' || status?.state === 'failed') ? (
                <Button
                  type="primary"
                  icon={<CloudDownloadOutlined />}
                  loading={busy}
                  disabled={busy}
                  onClick={() => void run(() => api.downloadServerUpdate())}
                >
                  {t('system.serverUpdate.download')}
                </Button>
              ) : null}

              {status?.state === 'downloading' ? (
                <Button danger disabled={busy} onClick={() => void run(() => api.cancelServerUpdateDownload())}>
                  {t('system.serverUpdate.cancel')}
                </Button>
              ) : null}

              {status?.state === 'downloaded' ? (
                <Button type="primary" danger loading={busy} disabled={busy} onClick={apply}>
                  {t('system.serverUpdate.apply')}
                </Button>
              ) : null}
            </Space>
          ) : null}
        </Space>
      )}
    </SectionCard>
  )
}
