import { create } from 'zustand'

import { getLanguage, isLanguage, setLanguage as applyLanguage, type Language, type Role, type ServerEntry } from '@vault/ui'

import { createPersistence } from './persistence'

const CONFIG_VERSION = 1

/** 持久化结构（写入 %APPDATA%/Vault/config.json）。 */
interface PersistedConfig {
  version: number
  language: Language
  clientId: string
  mountMode: string
  autoMount: boolean
  servers: ServerEntry[]
  activeKey: string | null
}

export interface AppState extends PersistedConfig {
  hydrated: boolean
  hydrate: () => Promise<void>
  setLanguage: (language: Language) => void
  setMountMode: (mode: string) => void
  setAutoMount: (value: boolean) => void
  setActive: (key: string) => void
  upsertServer: (entry: ServerEntry) => void
  /** 新增服务器但不切换活动服务端（当前没有活动服务端时才设为活动）。 */
  addServer: (entry: ServerEntry) => void
  /** 按 key 更新服务器；patch.key 变化时同步更新 activeKey。 */
  updateServer: (key: string, patch: Partial<ServerEntry>) => void
  removeServer: (key: string) => void
  updateActiveServer: (patch: Partial<ServerEntry>) => void
  /** 用代理自动续期得到的新令牌更新活动服务端会话（仅更新令牌与过期时间）。 */
  updateActiveToken: (token: string, expiresAt: number) => void
  /**
   * 用代理自动续期得到的新令牌更新**指定**服务端会话（仅更新令牌与过期时间）。
   *
   * 代理会为**每一台**各自续期并广播带 server_key 的 session 事件：不按键写回的话，
   * 续期后的令牌会被记到活动服务端名下（另一台仍是旧令牌，下一个请求就 401）。
   */
  updateServerToken: (key: string, token: string, expiresAt: number) => void
  /**
   * 用续期/证书重登录得到的用户信息更新**指定**服务端的会话用户（id / 名字 / 角色）。
   *
   * 角色必须一起刷新：界面只该把本地这份角色当作"服务端最近一次的说法"。少了这一步，
   * 服务端把用户降级后，顶栏的"管理控制台"入口会一直留着，点进去只会一路 403。
   */
  updateServerUser: (key: string, user: { id: string; username: string; role: Role }) => void
}

const persistence = createPersistence()

function newClientId(): string {
  try {
    return `client-${crypto.randomUUID()}`
  } catch {
    return `client-${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`
  }
}

function isServerEntry(value: unknown): value is ServerEntry {
  if (typeof value !== 'object' || value === null) return false
  const entry = value as Partial<ServerEntry>
  return typeof entry.key === 'string' && typeof entry.baseUrl === 'string'
}

/**
 * 规范化服务端条目：老配置没有 autoConnect 字段时默认 true，
 * 保持升级前"启动即自动连接"的行为。
 */
function normalizeServerEntry(entry: ServerEntry): ServerEntry {
  return { ...entry, autoConnect: entry.autoConnect !== false }
}

/**
 * 规范化地址（用于"是否同一服务端"的比较）：去空白、去尾部斜杠、主机名大小写不敏感。
 *
 * 导出给界面用：多服务端下要拿代理回传的 `server_key` / `session.server_url` 去对上本地条目，
 * 而同一台的两种键（地址 / 实例 ID）在代理内会被合并，只按 key 比会漏。
 */
export function normalizeUrl(url: string): string {
  return url.trim().replace(/\/+$/, '').toLowerCase()
}

/**
 * 顶栏手动切换服务端时写一个短期标记。
 *
 * 切换后页面会整页重载（重建查询缓存）。如果目标服务端关闭了"自动连接"，
 * 重载时按 autoConnect 规则会把 activeKey 重置到别的服务端/选择页 ——
 * 这个标记让下一次持久化加载保留"用户刚刚手动选的那台"，而不是当成启动自动连接。
 * 只存在 sessionStorage，重载后即消费，不会影响之后的全新启动。
 */
const MANUAL_SERVER_SWITCH_KEY = 'vault.manual-server-switch'

export function rememberManualServerSwitch(key: string): void {
  try {
    sessionStorage.setItem(MANUAL_SERVER_SWITCH_KEY, key)
  } catch {
    // 非浏览器环境/隐私模式：退化为不保留，不影响主流程。
  }
}

function takeManualServerSwitch(): string {
  try {
    const key = sessionStorage.getItem(MANUAL_SERVER_SWITCH_KEY) ?? ''
    if (key) sessionStorage.removeItem(MANUAL_SERVER_SWITCH_KEY)
    return key
  } catch {
    return ''
  }
}

/**
 * 判断两个条目是否指向**同一个服务端**。
 *
 * 判定依据（任一成立即视为同一个）：
 *  1. 双方都有 `instanceId` 且相等（服务端重装换地址也算同一台）；
 *  2. 规范化后的地址相同（同一台服务端在地址没变时重复添加）。
 *
 * 为什么必须去重：同一台服务端一旦出现多条，用户会在顶栏看到两个一模一样的条目，
 * 会话/证书指纹也会分散在不同条目上（例如早期条目以地址为 key、后来以实例 ID 为 key）。
 */
function isSameServer(a: ServerEntry, b: ServerEntry): boolean {
  if (a.instanceId && b.instanceId && a.instanceId === b.instanceId) return true
  return normalizeUrl(a.baseUrl) === normalizeUrl(b.baseUrl) && normalizeUrl(a.baseUrl) !== ''
}

/** 去掉指向同一服务端的重复条目（保留最后一条，即最新的那条）。 */
function dedupeServers(servers: ServerEntry[]): ServerEntry[] {
  const out: ServerEntry[] = []
  for (const entry of servers) {
    for (let i = out.length - 1; i >= 0; i -= 1) {
      if (isSameServer(out[i], entry)) out.splice(i, 1)
    }
    out.push(entry)
  }
  return out
}

function parsePersisted(raw: unknown): Partial<PersistedConfig> {
  if (typeof raw !== 'object' || raw === null) return {}
  const value = raw as Record<string, unknown>
  const out: Partial<PersistedConfig> = {}
  if (isLanguage(value.language)) out.language = value.language
  if (typeof value.clientId === 'string' && value.clientId) out.clientId = value.clientId
  if (typeof value.mountMode === 'string' && value.mountMode) out.mountMode = value.mountMode
  if (typeof value.autoMount === 'boolean') out.autoMount = value.autoMount
  if (Array.isArray(value.servers)) {
    out.servers = dedupeServers(value.servers.filter(isServerEntry).map(normalizeServerEntry))
  }

  // 活动服务端只从"开启自动连接"的条目里选：
  //   - 全部关闭 → 不自动激活，启动后进入服务端选择页；
  //   - 原 activeKey 仍然有效且开启了自动连接 → 保留；
  //   - 否则取第一台开启自动连接的。
  const servers = out.servers ?? []
  const autoServers = servers.filter((entry) => entry.autoConnect !== false)
  const requested = typeof value.activeKey === 'string' ? value.activeKey : null
  // 顶栏手动切换后的那次重载：优先保留用户刚选的服务端（即使它没开自动连接）。
  const manual = takeManualServerSwitch()
  if (manual && servers.some((entry) => entry.key === manual)) {
    out.activeKey = manual
  } else if (autoServers.length === 0) {
    out.activeKey = null
  } else if (requested && autoServers.some((entry) => entry.key === requested)) {
    out.activeKey = requested
  } else {
    out.activeKey = autoServers[0].key
  }
  return out
}

function snapshot(state: AppState): PersistedConfig {
  return {
    version: CONFIG_VERSION,
    language: state.language,
    clientId: state.clientId,
    mountMode: state.mountMode,
    autoMount: state.autoMount,
    servers: state.servers,
    activeKey: state.activeKey
  }
}

function persist(state: AppState): void {
  void persistence.save(snapshot(state))
}

export const useAppStore = create<AppState>()((set, get) => {
  /** 修改状态并落盘。 */
  const update = (patch: Partial<AppState>): void => {
    set(patch as AppState)
    persist(get())
  }

  return {
    hydrated: false,
    version: CONFIG_VERSION,
    language: getLanguage(),
    clientId: newClientId(),
    mountMode: 'letter',
    autoMount: false,
    servers: [],
    activeKey: null,

    hydrate: async () => {
      const raw = await persistence.load()
      const parsed = parsePersisted(raw)
      const language = parsed.language ?? getLanguage()
      applyLanguage(language)
      set({
        ...parsed,
        language,
        hydrated: true
      })
    },

    setLanguage: (language) => {
      applyLanguage(language)
      update({ language })
    },

    setMountMode: (mountMode) => update({ mountMode }),

    setAutoMount: (autoMount) => update({ autoMount }),

    setActive: (key) => update({ activeKey: key }),

    upsertServer: (entry) => {
      // 同一服务端只保留一条：先取出指向同一服务端的旧条目，把其中的会话/展示字段继承过来
      // （避免重新配置地址时丢掉登录态），再从列表里移除它们，最后加入合并结果。
      const previous = get().servers.filter((item) => isSameServer(item, entry))
      const inherited = previous.reduce<Partial<ServerEntry>>((acc, item) => ({ ...acc, ...item }), {})
      const next: ServerEntry = {
        ...inherited,
        ...entry,
        autoConnect: entry.autoConnect ?? inherited.autoConnect ?? true
      }
      const servers = get().servers.filter((item) => !isSameServer(item, entry))
      servers.push(next)
      update({ servers, activeKey: next.key })
    },

    addServer: (entry) => {
      // 设置页新增：当前已有活动服务端时不要偷偷切过去（用户可能只是在补录另一台）。
      const previous = get().servers.filter((item) => isSameServer(item, entry))
      const inherited = previous.reduce<Partial<ServerEntry>>((acc, item) => ({ ...acc, ...item }), {})
      const next: ServerEntry = {
        ...inherited,
        ...entry,
        autoConnect: entry.autoConnect ?? inherited.autoConnect ?? true
      }
      const servers = get().servers.filter((item) => !isSameServer(item, entry))
      servers.push(next)
      update({ servers, activeKey: get().activeKey ?? next.key })
    },

    updateServer: (key, patch) => {
      const currentActiveKey = get().activeKey
      const servers = get().servers.map((item) => (item.key === key ? { ...item, ...patch } : item))
      // patch 允许改 key（例如测试连接后拿到实例 ID）：活动键要跟着一起改。
      const activeKey = currentActiveKey === key && patch.key ? patch.key : currentActiveKey
      update({ servers, activeKey })
    },

    removeServer: (key) => {
      const servers = get().servers.filter((item) => item.key !== key)
      // 删掉的正好是活动服务端：优先落到另一台开启自动连接的；全都没开就置空，
      // 让启动流程进入服务端选择页，而不是偷偷连一台用户没允许自动连接的。
      const activeKey =
        get().activeKey === key
          ? (servers.find((item) => item.autoConnect !== false)?.key ?? null)
          : get().activeKey
      update({ servers, activeKey })
    },

    updateActiveServer: (patch) => {
      const activeKey = get().activeKey
      if (!activeKey) return
      const servers = get().servers.map((item) => (item.key === activeKey ? { ...item, ...patch } : item))
      update({ servers })
    },

    updateActiveToken: (token, expiresAt) => {
      const activeKey = get().activeKey
      if (!activeKey) return
      const servers = get().servers.map((item) =>
        item.key === activeKey ? { ...item, token, tokenExpiresAt: expiresAt } : item
      )
      update({ servers })
    },

    updateServerToken: (key, token, expiresAt) => {
      if (!key) return
      const servers = get().servers.map((item) =>
        item.key === key ? { ...item, token, tokenExpiresAt: expiresAt } : item
      )
      update({ servers })
    },

    updateServerUser: (key, user) => {
      if (!key) return
      const servers = get().servers.map((item) =>
        item.key === key ? { ...item, userId: user.id, username: user.username, role: user.role } : item
      )
      update({ servers })
    }
  }
})

/** 当前活动服务端（派生读取）。 */
export function selectActiveServer(state: AppState): ServerEntry | null {
  if (!state.activeKey) return null
  return state.servers.find((item) => item.key === state.activeKey) ?? null
}
