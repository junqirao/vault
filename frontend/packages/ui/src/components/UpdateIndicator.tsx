import { useCallback, useEffect, useRef, useState } from 'react'
import { App, Modal, Progress, Space, Typography, theme } from 'antd'
import { ArrowDownOutlined } from '@ant-design/icons'

import { agentApi, getAgentVersionInfo, relaunchClient } from '../api/agentClient'
import type { UpdateInfo } from '../api/agentTypes'
import { translateError } from '../api/errors'
import { useAgent } from '../hooks/useAgent'
import { useI18n } from '../i18n'
import { fontSize, spacing } from '../tokens/palette'
import { formatBytes } from '../utils/format'

/**
 * 运行期自动检查更新的间隔。
 *
 * 启动期的整包更新只在启动时跑一次，运行中新发布的版本不会自己出现；这里按固定间隔
 * 静默检查，让顶栏能提示"有新版本"（真正安装仍走重启，见 clientUpdate.ts）。
 */
const UPDATE_CHECK_INTERVAL_MS = 30 * 60 * 1000
/** 进入应用后的首次检查延迟：先把首屏的状态/配置等本地代理请求让过去。 */
const UPDATE_FIRST_CHECK_DELAY_MS = 5000
/** 窗口重新获得焦点时的检查节流：频繁切换窗口不该反复打服务端。 */
const UPDATE_FOCUS_CHECK_GAP_MS = 10 * 60 * 1000

/**
 * 顶栏更新提示：有可用更新时显示一个圆形下载图标，悬停展开版本/说明与操作。
 *
 * 默认只露出图标，避免在顶栏常驻一段"发现新版本"的文字；鼠标移上去才展开成
 * 「更新」胶囊并弹出详情，用户不关心更新时几乎不占注意力。
 */
export function UpdateIndicator(): JSX.Element | null {
  const { t } = useI18n()
  const { message } = App.useApp()
  const { token } = theme.useToken()
  const agent = useAgent()
  const [info, setInfo] = useState<UpdateInfo | null>(null)
  const [hovered, setHovered] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [applying, setApplying] = useState(false)
  const inFlight = useRef(false)
  const lastCheckedAt = useRef(0)

  // 用户关掉「自动更新」后不再后台检查：手动检查仍在设置页，结果也会显示到这里。
  const autoUpdate = agent.config?.auto_update ?? true
  // 重新连上服务端（含启动后首次登录完成）时补一次检查：断网期间发起的检查只会拿到失败。
  const connected = Boolean(agent.state?.server?.connected)
  // 登录完成（代理侧出现会话）同样要补一次：没有会话时检查更新只会得到 no_session。
  const sessionUserId = agent.state?.session?.user_id ?? ''

  const updateState = agent.state?.update
  const stateVersion = updateState?.available_version ?? ''
  const version = info?.version ?? ''
  const hasUpdate = Boolean(version)
  // 没有本次检查结果（例如只有代理旧快照）时按整包更新处理：重启最安全，不会出现
  // "界面新了、本地服务还是旧的"。
  const clientPackage = info?.client_available ?? true
  const downloading = Boolean(updateState?.downloading)
  const totalBytes = updateState?.total_bytes ?? 0
  const receivedBytes = updateState?.received_bytes ?? 0
  const percent =
    totalBytes > 0 ? Math.min(100, Math.round((receivedBytes / totalBytes) * 100)) : undefined

  const check = useCallback(async (): Promise<void> => {
    // 下载/安装进行中不再发起检查：代理的检查会把进度计数清零（见 checkUpdate）。
    if (!agent.available || downloading || inFlight.current) return
    inFlight.current = true
    try {
      const result = await agentApi.checkUpdate()
      if (result.available) {
        setInfo(result)
      } else if (result.reason !== 'server_unreachable' && result.reason !== 'no_session') {
        // 明确"没有可用更新"才收起；网络/登录这类暂时性失败保留上一次结果，等下一轮再试。
        setInfo(null)
      }
      lastCheckedAt.current = Date.now()
    } catch {
      // 静默失败：顶栏提示不该因为一次网络抖动弹错误；下一次定时检查会再试。
    } finally {
      inFlight.current = false
    }
  }, [agent.available, downloading])

  useEffect(() => {
    if (!agent.available || !autoUpdate) return
    let stopped = false
    const first = window.setTimeout(() => {
      if (!stopped) void check()
    }, UPDATE_FIRST_CHECK_DELAY_MS)
    const timer = window.setInterval(() => {
      if (!stopped) void check()
    }, UPDATE_CHECK_INTERVAL_MS)
    const onFocus = (): void => {
      if (Date.now() - lastCheckedAt.current < UPDATE_FOCUS_CHECK_GAP_MS) return
      void check()
    }
    window.addEventListener('focus', onFocus)
    return () => {
      stopped = true
      window.clearTimeout(first)
      window.clearInterval(timer)
      window.removeEventListener('focus', onFocus)
    }
  }, [agent.available, autoUpdate, connected, sessionUserId, check])

  // 设置页的手动检查、以及其它界面触发的检查都会写回代理状态：状态里出现新版本时同步到
  // 本地（并复用已知的备注/产物类型），状态被清空时一并收起，避免显示已失效的更新。
  useEffect(() => {
    setInfo((prev) => {
      if (stateVersion === '') return prev === null ? prev : null
      if (prev?.version === stateVersion) return prev
      return { available: true, version: stateVersion }
    })
  }, [stateVersion])

  const apply = async (): Promise<void> => {
    setApplying(true)
    try {
      // 带上本体版本：清单若同时带客户端本体，代理会拒绝单独跑在本体前面。
      await agentApi.applyUpdate(getAgentVersionInfo().expected)
      message.success(t('settings.update.started'))
    } catch (error) {
      message.error(translateError(error))
    } finally {
      setApplying(false)
    }
  }

  if (!hasUpdate) return null

  const content = (
    <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
      {info?.notes?.trim() ? (
        <Typography.Paragraph
          type="secondary"
          style={{
            fontSize: fontSize.xs,
            marginBottom: 0,
            maxHeight: 160,
            overflow: 'auto',
            whiteSpace: 'pre-wrap'
          }}
        >
          {info.notes}
        </Typography.Paragraph>
      ) : null}
      {downloading || applying ? (
        <>
          {percent === undefined ? (
            <Progress status="active" size="small" />
          ) : (
            <Progress percent={percent} size="small" />
          )}
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('update.progress', {
              received: formatBytes(receivedBytes),
              total: formatBytes(totalBytes)
            })}
          </Typography.Text>
        </>
      ) : clientPackage ? (
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('settings.update.restartToInstall')}
        </Typography.Text>
      ) : (
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('update.downloadAndInstall')}
        </Typography.Text>
      )}
    </Space>
  )

  // 点击图标不再弹出"介绍 + 按钮"的气泡，而是直接给确认框：
  // 确认 = 原来的「立即重启」（整包更新）或「下载并安装」（仅服务端更新）。
  const confirm = (): void => {
    if (downloading || applying) return
    if (clientPackage) {
      setConfirmOpen(false)
      void relaunchClient()
      return
    }
    void apply()
  }

  return (
    <>
      <button
        type="button"
        aria-label={t('update.indicator.label', { version })}
        onMouseEnter={() => setHovered(true)}
        onMouseLeave={() => setHovered(false)}
        onClick={() => setConfirmOpen(true)}
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'center',
          gap: 4,
          // 与头像同高（24px）：之前 32px 的圆圈在顶栏里比头像大一圈，很突兀。
          height: 24,
          minWidth: 24,
          paddingInline: hovered ? spacing.xs : 0,
          borderRadius: 12,
          border: 'none',
          cursor: 'pointer',
          color: token.colorPrimary,
          background: hovered ? token.colorPrimaryBgHover : token.colorPrimaryBg,
          fontSize: 12,
          lineHeight: 1,
          verticalAlign: 'middle'
        }}
      >
        {hovered ? (
          <span style={{ fontSize: fontSize.xs, fontWeight: 600 }}>{t('update.indicator.short')}</span>
        ) : null}
        <ArrowDownOutlined style={{ fontSize: 12 }} />
      </button>
      <Modal
        open={confirmOpen}
        title={t('settings.update.available', { version })}
        okText={clientPackage ? t('settings.update.restartNow') : t('update.downloadAndInstall')}
        cancelText={t('common.cancel')}
        confirmLoading={applying}
        okButtonProps={{ disabled: downloading || applying }}
        onOk={confirm}
        onCancel={() => setConfirmOpen(false)}
        destroyOnHidden
        centered
      >
        {content}
      </Modal>
    </>
  )
}
