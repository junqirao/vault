import {
  VaultApi,
  agentApi,
  type AgentEventStream,
  type QueryValue
} from '@vault/ui'

import {
  demoAgentConfig,
  demoAgentHealth,
  demoAgentState,
  demoAllocations,
  demoCache,
  demoCacheHistory,
  demoDepsReport,
  demoMounts,
  demoPerformance,
  demoRepos,
  demoStorages,
  demoSystemInfo,
  demoUsage,
  demoUser,
  demoUsers,
  disksOf
} from './mockData'

/** demo 里的服务端地址只用于顶栏展示，所有请求都在下面被接管。 */
const DEMO_SERVER = 'http://127.0.0.1:5173'

function repoOf(id: string) {
  return demoRepos.find((item) => item.id === id)
}

class DemoApi extends VaultApi {
  constructor() {
    super({
      getConfig: () => ({ baseUrl: DEMO_SERVER, token: 'demo-token' }),
      timeoutMs: 3000
    })
  }

  protected async request<T>(
    _method: string,
    path: string,
    _body?: unknown,
    _query?: Record<string, QueryValue>,
    _allowRenew?: boolean
  ): Promise<T> {
    const clean = path.split('?')[0]

    if (clean === '/v1/auth/me') return demoUser as T
    if (clean === '/v1/users') return { items: demoUsers } as T
    if (clean === '/v1/storages') return { items: demoStorages } as T
    if (clean === '/v1/system/info') return demoSystemInfo as T
    if (clean === '/v1/system/deps') return demoDepsReport as T
    if (clean === '/v1/system/pools') return { items: [], default_pool: '' } as T
    if (clean === '/v1/repos') return { items: demoRepos } as T

    const repoSub = clean.match(/^\/v1\/repos\/([^/]+)\/(allocations|disks|members|diff-operation)$/)
    if (repoSub) {
      const id = decodeURIComponent(repoSub[1])
      switch (repoSub[2]) {
        case 'allocations':
          return { items: demoAllocations.filter((item) => item.repo_id === id) } as T
        case 'disks':
          return { items: disksOf(id) } as T
        case 'members':
          return { items: [{ user_id: demoUser.id, perm: 'manage' }] } as T
        case 'diff-operation':
          return { job: null } as T
      }
    }

    const repoOne = clean.match(/^\/v1\/repos\/([^/]+)$/)
    if (repoOne) {
      const target = repoOf(decodeURIComponent(repoOne[1]))
      if (target) return target as T
    }

    // 页面里偶尔会顺手拉一下任务/租约之类的列表，返回空集合即可，
    // 截图用不到，但不能让请求以未处理的异常收场。
    return { items: [] } as T
  }
}

export const demoApi = new DemoApi()

function createStream(): AgentEventStream & { emit: (type: string, data: unknown) => void } {
  const listeners = new Map<string, Set<(event: Event) => void>>()
  let closed = false
  const stream = {
    onopen: null as ((event: Event) => void) | null,
    onerror: null as ((event: Event) => void) | null,
    addEventListener(type: string, listener: (event: Event) => void) {
      const set = listeners.get(type) ?? new Set()
      set.add(listener)
      listeners.set(type, set)
    },
    close() {
      closed = true
      listeners.clear()
    },
    emit(type: string, data: unknown) {
      if (closed) return
      const event = { type, data: JSON.stringify(data) } as MessageEvent
      for (const listener of listeners.get(type) ?? []) listener(event as unknown as Event)
      // onmessage 风格的消费者（性能页用的是 addEventListener，这里保留兼容）
      const handler = (stream as unknown as Record<string, unknown>)[`on${type}`]
      if (typeof handler === 'function') (handler as (event: Event) => void)(event as unknown as Event)
    }
  }
  return stream
}

export function installDemoRuntime(): void {
  // 顶栏延迟探针走的是渲染进程直连 fetch，拦一下让它显示一个稳定的 2ms。
  const originalFetch = window.fetch.bind(window)
  window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    if (url.endsWith('/v1/system/ping')) {
      return new Response('{"ok":true}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      })
    }
    return originalFetch(input, init)
  }

  Object.assign(agentApi as unknown as Record<string, unknown>, {
    probe: async () => true,
    health: async () => demoAgentHealth,
    state: async () => demoAgentState,
    getConfig: async () => demoAgentConfig,
    getUploads: async () => ({ items: [] }),
    getDownloads: async () => ({ items: [] }),
    getWebUpdate: async () => ({ web_update: null }),
    getCache: async () => demoCache,
    getUsage: async () => ({ items: demoUsage }),
    getPerformance: async () => demoPerformance,
    getCacheHistory: async () => demoCacheHistory,
    openPath: async () => undefined,
    showItemInFolder: async () => undefined,
    subscribe: () => createStream(),
    subscribePerformance: () => {
      const stream = createStream()
      let timer = 0
      const push = (): void => stream.emit('performance', demoPerformance)
      const start = (): void => {
        window.setTimeout(push, 40)
        timer = window.setInterval(push, 2000)
      }
      const originalAdd = stream.addEventListener.bind(stream)
      stream.addEventListener = (type: string, listener: (event: Event) => void): void => {
        originalAdd(type, listener)
        if (type === 'performance' && timer === 0) start()
      }
      const originalClose = stream.close.bind(stream)
      stream.close = (): void => {
        if (timer) window.clearInterval(timer)
        timer = 0
        originalClose()
      }
      return stream
    }
  })

  // 挂载记录本身是只读的，这里补一个空操作，避免误点按钮时抛错把截图页面顶掉。
  Object.assign(agentApi as unknown as Record<string, unknown>, {
    mount: async () => demoMounts[0],
    unmount: async () => undefined,
    remount: async () => demoMounts[0],
    cancelMount: async () => undefined,
    reconnectServer: async () => demoAgentState.servers[0]
  })
}
