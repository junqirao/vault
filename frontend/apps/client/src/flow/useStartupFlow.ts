import { useCallback, useEffect, useRef, useState } from 'react'

import {
  CLIENT_API_VERSION,
  isApiError,
  type BootstrapStatus,
  type ClientCheckResponse,
  type VaultApi
} from '@vault/ui'

import { CLIENT_VERSION } from '../config'
import { selectActiveServer, useAppStore } from '../store/appStore'

/** 启动阶段（严格对应需求给出的判定顺序）。 */
export type StartupPhase =
  | { kind: 'loading' }
  | { kind: 'config' }
  | { kind: 'offline'; detail?: string }
  | { kind: 'blocked'; status: BootstrapStatus }
  | { kind: 'compat'; check: ClientCheckResponse }
  | { kind: 'bootstrap'; status: BootstrapStatus }
  | { kind: 'login' }
  | { kind: 'select-server' }
  | { kind: 'ready' }

/**
 * 自动连接服务端连接失败后的自动重连退避（毫秒）。
 *
 * 最后一档持续循环：开启自动连接意味着用户希望"服务端恢复后自己连上"，
 * 而不是停在"无法连接"页等下一个人来点重试。退避只为避免服务端长时间不可达时刷日志/打满连接。
 */
const AUTO_RECONNECT_DELAYS_MS = [3000, 5000, 10000, 20000, 30000]

export interface StartupFlow {
  phase: StartupPhase
  checking: boolean
  /**
   * 活动服务端开启了自动连接、且当前处于"无法连接"阶段时，是否正在自动重连。
   *
   * 界面据此告诉用户"还在连"而不是"已经放弃"（两者都停在 offline 页，但观感完全不同）。
   */
  autoReconnecting: boolean
  /** 重新执行检查（重试 / 保存地址后）。 */
  recheck: () => void
  setPhase: (phase: StartupPhase) => void
  /** 服务端选择页：显式选择一台服务器（即使它没开启自动连接）。 */
  selectServer: (key: string) => void
  /** 光标处于不兼容提示页时，允许"仍然继续（只读查看）"。 */
  continueAfterCompat: (check: ClientCheckResponse) => void
  /**
   * 暂停自动重连：用户打开"修改地址"表单时调用，避免后台自动连上后把表单顶掉。
   *
   * 与 resumeAutoReconnect 成对使用；新的检查（保存地址 / 手动重试 / 切换服务端）
   * 也会自动恢复，不会让自动重连一直停着。
   */
  pauseAutoReconnect: () => void
  resumeAutoReconnect: () => void
}

interface RunOptions {
  /**
   * 后台重试：不进入 loading 页（避免每次自动重连都把"无法连接"页闪成整屏加载），
   * 失败后仍停留在 offline 页并继续退避。
   */
  background?: boolean
}

function isOfflineError(error: unknown): boolean {
  return isApiError(error) && (error.kind === 'network' || error.kind === 'timeout')
}

/** 启动流程：读取配置 → client-check → bootstrap 状态 → token 校验 → 登录/主界面。 */
export function useStartupFlow(api: VaultApi): StartupFlow {
  const [phase, setPhaseState] = useState<StartupPhase>({ kind: 'loading' })
  const [checking, setChecking] = useState(false)
  const [autoReconnecting, setAutoReconnecting] = useState(false)
  const [autoReconnectPaused, setAutoReconnectPaused] = useState(false)
  const activeKey = useAppStore((state) => state.activeKey)
  const hydrated = useAppStore((state) => state.hydrated)
  // 只订阅"活动服务端是否开启自动连接"这个布尔量：设置页改了开关也要立即生效。
  const autoConnectActive = useAppStore((state) => {
    const active = selectActiveServer(state)
    return Boolean(active && active.autoConnect !== false)
  })
  const runTokenRef = useRef(0)
  /** 自动重连已经排到第几次（用于选退避档位；离开 offline 或手动检查时清零）。 */
  const autoReconnectAttemptRef = useRef(0)
  /** 待触发的自动重连定时器；显式检查 / 暂停时先取消，避免和手动操作抢跑。 */
  const autoReconnectTimerRef = useRef<number | null>(null)
  /** 指向最新的 run，避免自动重连定时器闭包里拿到旧实现。 */
  const runRef = useRef<(forceContinue: boolean, options?: RunOptions) => Promise<void>>(async () => undefined)

  const setPhase = useCallback((next: StartupPhase): void => {
    setPhaseState(next)
  }, [])

  const pauseAutoReconnect = useCallback((): void => {
    setAutoReconnectPaused(true)
    // 用户在改地址期间如果恰好有一次后台重连在途，作废它的结果：
    // 否则它稍后连上会把正在编辑的表单直接顶掉。
    if (autoReconnectTimerRef.current !== null) {
      window.clearTimeout(autoReconnectTimerRef.current)
      autoReconnectTimerRef.current = null
    }
    runTokenRef.current += 1
    setChecking(false)
    setAutoReconnecting(false)
  }, [])

  const resumeAutoReconnect = useCallback((): void => {
    setAutoReconnectPaused(false)
  }, [])

  const run = useCallback(
    async (forceContinue: boolean, options: RunOptions = {}): Promise<void> => {
      const token = ++runTokenRef.current
      const background = options.background === true
      if (!background) {
        // 任何一次"显式"检查都从第一档退避重新开始，并恢复被表单暂停的自动重连。
        // 先取消已排队的自动重连，避免它和本次检查同时打服务端。
        if (autoReconnectTimerRef.current !== null) {
          window.clearTimeout(autoReconnectTimerRef.current)
          autoReconnectTimerRef.current = null
        }
        autoReconnectAttemptRef.current = 0
        setAutoReconnectPaused(false)
        setAutoReconnecting(false)
        setPhase({ kind: 'loading' })
      }
      setChecking(true)

      const state = useAppStore.getState()
      const active = selectActiveServer(state)

      // 自动连接规则在持久化加载时（parsePersisted）已经决定好了 activeKey：
      //   - 有开启自动连接的 → 选第一台/保留原来的；
      //   - 全都没开 → activeKey 为 null，这里进入服务端选择页。
      // 用户从选择页显式选了某台后 activeKey 就有值了，直接继续，不会又被弹回选择页。
      if (!active) {
        setChecking(false)
        setPhase(state.servers.length > 0 ? { kind: 'select-server' } : { kind: 'config' })
        return
      }

      try {
        const check = await api.clientCheck({
          api_version: CLIENT_API_VERSION,
          client_version: CLIENT_VERSION
        })
        if (!forceContinue && check.state !== 'compatible') {
          if (runTokenRef.current === token) setPhase({ kind: 'compat', check })
          return
        }

        const status = await api.bootstrapStatus()
        if (runTokenRef.current !== token) return

        if (status.needs_bootstrap) {
          setPhase({ kind: 'bootstrap', status })
          return
        }
        if (status.blocked_reason === 'bootstrap_disabled' || status.blocked_reason === 'bootstrap_window_expired') {
          setPhase({ kind: 'blocked', status })
          return
        }

        const current = selectActiveServer(useAppStore.getState())
        // 本地已经知道过期的令牌不再拿去探针：那一发必然 401（服务端重启也会清空内存
        // 会话），除了日志噪音没有任何信息量。直接清掉，走登录 / 证书免密流程。
        // 兼容老配置：过期时间可能是秒，也可能是毫秒。
        const rawExpiry = current?.tokenExpiresAt
        const expiresAtMs =
          typeof rawExpiry === 'number' && rawExpiry > 0 ? (rawExpiry < 1e12 ? rawExpiry * 1000 : rawExpiry) : 0
        const tokenExpired = expiresAtMs > 0 && expiresAtMs <= Date.now()
        if (current?.token && !tokenExpired) {
          try {
            // system/info 是匿名接口，无法校验令牌；这里用需要认证的 repos 列表做探针。
            await api.listRepos({ limit: 1 })
            if (runTokenRef.current === token) setPhase({ kind: 'ready' })
            return
          } catch (error) {
            if (isOfflineError(error)) {
              if (runTokenRef.current === token) setPhase({ kind: 'offline', detail: errorMessage(error) })
              return
            }
            // 令牌失效：清除后进入登录页。
            useAppStore.getState().updateActiveServer({
              token: undefined,
              tokenExpiresAt: undefined,
              userId: undefined,
              username: undefined,
              role: undefined
            })
          }
        } else if (current?.token) {
          useAppStore.getState().updateActiveServer({
            token: undefined,
            tokenExpiresAt: undefined,
            userId: undefined,
            username: undefined,
            role: undefined
          })
        }
        if (runTokenRef.current === token) setPhase({ kind: 'login' })
      } catch (error) {
        if (runTokenRef.current !== token) return
        if (isOfflineError(error)) {
          setPhase({ kind: 'offline', detail: errorMessage(error) })
        } else {
          // 其他错误（如 5xx / 业务错误）同样以"无法连接"呈现，并提供重试。
          setPhase({ kind: 'offline', detail: errorMessage(error) })
        }
      } finally {
        if (runTokenRef.current === token) setChecking(false)
      }
    },
    [api, setPhase]
  )

  useEffect(() => {
    runRef.current = run
  }, [run])

  useEffect(() => {
    if (!hydrated) return
    void run(false)
  }, [hydrated, activeKey, run])

  /**
   * 自动重连循环：只有"活动服务端开启了自动连接"才自动重试。
   *
   * 依赖 phase 对象而不是 phase.kind：后台重试失败会重新 setPhase(offline)（detail 可能变化），
   * 必须据此安排下一次退避；一旦离开 offline（连上、进入登录/兼容/初始化页），清理定时器并重置档位。
   * 用户打开"修改地址"表单时暂停，避免后台连上后把正在输入的表单顶掉。
   */
  useEffect(() => {
    if (phase.kind !== 'offline' || autoReconnectPaused) {
      setAutoReconnecting(false)
      if (phase.kind !== 'offline') autoReconnectAttemptRef.current = 0
      return
    }
    if (!autoConnectActive) {
      // 手动选择了一台"不自动连接"的服务端：失败就停在这里，由用户决定下一步。
      setAutoReconnecting(false)
      return
    }

    const attempt = autoReconnectAttemptRef.current
    autoReconnectAttemptRef.current = attempt + 1
    const delay = AUTO_RECONNECT_DELAYS_MS[Math.min(attempt, AUTO_RECONNECT_DELAYS_MS.length - 1)]
    setAutoReconnecting(true)
    const timer = window.setTimeout(() => {
      if (autoReconnectTimerRef.current === timer) autoReconnectTimerRef.current = null
      void runRef.current(false, { background: true })
    }, delay)
    autoReconnectTimerRef.current = timer
    return () => {
      window.clearTimeout(timer)
      if (autoReconnectTimerRef.current === timer) autoReconnectTimerRef.current = null
    }
  }, [phase, activeKey, autoConnectActive, autoReconnectPaused])

  const recheck = useCallback(() => {
    void run(false)
  }, [run])

  const continueAfterCompat = useCallback(
    (check: ClientCheckResponse) => {
      // 区间不兼容时允许只读继续；协议不匹配属硬性不可用，不提供继续入口。
      if (check.state === 'incompatible_protocol') return
      void run(true)
    },
    [run]
  )

  const selectServer = useCallback((key: string) => {
    // 显式选择：直接设为活动服务端；下一次检查会沿用它（不会按 autoConnect 再筛一次）。
    useAppStore.getState().setActive(key)
  }, [])

  return {
    phase,
    checking,
    autoReconnecting,
    recheck,
    setPhase,
    selectServer,
    continueAfterCompat,
    pauseAutoReconnect,
    resumeAutoReconnect
  }
}

function errorMessage(error: unknown): string {
  if (isApiError(error)) return `${error.code}${error.status ? ` (HTTP ${error.status})` : ''}`
  return error instanceof Error ? error.message : 'unknown'
}
