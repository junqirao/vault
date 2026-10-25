import { useCallback, useEffect, useRef, useState } from 'react'

import {
  CLIENT_API_VERSION,
  agentApi,
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

export interface StartupFlow {
  phase: StartupPhase
  checking: boolean
  /** 自动挂载进度（由本地代理执行，前端只展示；无自动挂载时为 null）。 */
  autoMount: AutoMountProgress | null
  /** 重新执行检查（重试 / 保存地址后）。 */
  recheck: () => void
  setPhase: (phase: StartupPhase) => void
  /** 服务端选择页：显式选择一台服务器（即使它没开启自动连接）。 */
  selectServer: (key: string) => void
  /** 光标处于不兼容提示页时，允许"仍然继续（只读查看）"。 */
  continueAfterCompat: (check: ClientCheckResponse) => void
}

/** 自动挂载进度摘要。 */
export interface AutoMountProgress {
  total: number
  pending: number
}

function isOfflineError(error: unknown): boolean {
  return isApiError(error) && (error.kind === 'network' || error.kind === 'timeout')
}

/** 启动流程：读取配置 → client-check → bootstrap 状态 → token 校验 → 登录/主界面。 */
export function useStartupFlow(api: VaultApi): StartupFlow {
  const [phase, setPhase] = useState<StartupPhase>({ kind: 'loading' })
  const [checking, setChecking] = useState(false)
  const [autoMount, setAutoMount] = useState<AutoMountProgress | null>(null)
  const activeKey = useAppStore((state) => state.activeKey)
  const hydrated = useAppStore((state) => state.hydrated)
  const runTokenRef = useRef(0)

  // 进入主界面后展示自动挂载进度：挂载由代理执行，前端只读取一次状态。
  useEffect(() => {
    if (phase.kind !== 'ready') {
      setAutoMount(null)
      return
    }
    let cancelled = false
    agentApi
      .state()
      .then((state) => {
        if (cancelled) return
        const total = state.mounts.length
        if (state.auto_mount && total > 0) {
          setAutoMount({ total, pending: state.mounts.filter((mount) => mount.state === 'mounting').length })
        } else {
          setAutoMount(null)
        }
      })
      .catch(() => {
        if (!cancelled) setAutoMount(null)
      })
    return () => {
      cancelled = true
    }
  }, [phase.kind])

  const run = useCallback(
    async (forceContinue: boolean): Promise<void> => {
      const token = ++runTokenRef.current
      setPhase({ kind: 'loading' })
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
    [api]
  )

  useEffect(() => {
    if (!hydrated) return
    void run(false)
  }, [hydrated, activeKey, run])

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

  return { phase, checking, autoMount, recheck, setPhase, selectServer, continueAfterCompat }
}

function errorMessage(error: unknown): string {
  if (isApiError(error)) return `${error.code}${error.status ? ` (HTTP ${error.status})` : ''}`
  return error instanceof Error ? error.message : 'unknown'
}
