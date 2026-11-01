import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron'

import type { AgentRequestOptions, AgentRequestResult, AgentStreamMessage } from './agent'

/**
 * preload：只暴露最小 API。
 *
 * 渲染进程只能读写本地配置、查询本地代理状态、经主进程转发调用本地代理、
 * 打开路径与请求退出；无法访问 Node/Electron 其他能力
 * （nodeIntegration 关闭、contextIsolation 开启）。
 */

/**
 * 下发给渲染进程的代理状态。
 *
 * 刻意**不含令牌**：所有对代理的调用都由主进程代发（见 agentRequest），
 * 令牌只存在于主进程内存中。
 */
interface VaultAgentInfo {
  available: boolean
  baseUrl: string | null
  error?: string
  /** 代理自报版本。 */
  version?: string | null
  /** 客户端期望的代理版本。 */
  expectedVersion?: string
  /** 代理版本是否**落后**于客户端期望（陈旧代理；代理更新不置位）。 */
  versionMismatch?: boolean
}

/**
 * 正在运行的客户端**本体**版本（主进程通过 webPreferences.additionalArguments 注入）。
 *
 * 不能用渲染层打包时的 __APP_VERSION__：渲染层可以被热更到比本体更新的版本
 * （见 main.ts 的 additionalArguments 说明），那会把"界面版本"误当成"客户端版本"。
 * 参数缺失时返回空串，由渲染层的构建期常量兜底。
 */
function appVersionFromArgv(): string {
  const prefix = '--vault-app-version='
  const arg = process.argv.find((item) => item.startsWith(prefix))
  if (!arg) return ''
  try {
    return decodeURIComponent(arg.slice(prefix.length)).trim()
  } catch {
    return ''
  }
}

const appVersion = appVersionFromArgv()

const api = {
  /** 客户端本体版本（安装目录里正在运行的那一份）；浏览器 dev 下为空。 */
  appVersion,
  getConfig: (): Promise<unknown> => ipcRenderer.invoke('vault:get-config'),
  setConfig: (value: unknown): Promise<boolean> => ipcRenderer.invoke('vault:set-config', value),
  getConfigPath: (): Promise<string> => ipcRenderer.invoke('vault:config-path'),
  getAgent: (): Promise<VaultAgentInfo> => ipcRenderer.invoke('vault:agent-info'),
  /** 代理重启（端口/令牌变化）时回调；返回取消订阅函数。 */
  onAgentChanged: (callback: (info: VaultAgentInfo) => void): (() => void) => {
    const listener = (_event: IpcRendererEvent, info: VaultAgentInfo): void => callback(info)
    ipcRenderer.on('vault:agent-changed', listener)
    return () => {
      ipcRenderer.removeListener('vault:agent-changed', listener)
    }
  },
  /** 经主进程转发一次代理请求（令牌不进入渲染进程）。 */
  agentRequest: (options: AgentRequestOptions): Promise<AgentRequestResult> =>
    ipcRenderer.invoke('vault:agent-request', options),
  /** 让主进程订阅代理事件流（可指定页面级路径），并把事件推给本渲染进程。 */
  agentEventsStart: (path?: string): Promise<boolean> =>
    ipcRenderer.invoke('vault:agent-events-start', path),
  /** 关闭主进程为本渲染进程建立的指定代理事件流。 */
  agentEventsStop: (path?: string): Promise<boolean> =>
    ipcRenderer.invoke('vault:agent-events-stop', path),
  /** 订阅代理事件流消息（open/error/event）；返回取消订阅函数。 */
  onAgentEvent: (callback: (message: AgentStreamMessage) => void): (() => void) => {
    const listener = (_event: IpcRendererEvent, message: AgentStreamMessage): void => callback(message)
    ipcRenderer.on('vault:agent-event', listener)
    return () => {
      ipcRenderer.removeListener('vault:agent-event', listener)
    }
  },
  /** 把服务端证书指纹登记为可信（TOFU 首次信任）；见 electron/main.ts 的说明。 */
  trustCertificate: (serverUrl: string, sha256: string): Promise<boolean> =>
    ipcRenderer.invoke('vault:trust-certificate', { serverUrl, sha256 }),
  /** 打开系统目录选择框（选择源目录）；用户取消时 canceled=true。 */
  selectDirectory: (): Promise<{ canceled: boolean; path?: string }> =>
    ipcRenderer.invoke('vault:select-directory'),
  /** 为本地挂载点创建快捷方式（Windows/.lnk）；dir 为空时落到系统桌面。 */
  createShortcut: (input: { target: string; name?: string; dir?: string }): Promise<{ ok: boolean; path?: string; reason?: string }> =>
    ipcRenderer.invoke('vault:create-shortcut', input),
  openPath: (target: string): Promise<void> => ipcRenderer.invoke('vault:open-path', target),
  showItemInFolder: (target: string): Promise<void> => ipcRenderer.invoke('vault:show-item', target),
  quit: (): Promise<void> => ipcRenderer.invoke('vault:quit'),
  /** 立即重启客户端（用于让已激活的渲染层热更资源生效）。 */
  relaunch: (): Promise<void> => ipcRenderer.invoke('vault:relaunch'),
  /**
   * 让"刚激活的热更渲染层"立刻生效：主进程重新解析热更目录并加载（无需重启客户端）。
   *
   * applied=false 表示当前已是最新层（或没有可用热更层），调用方无需处理。
   */
  applyWebLayer: (): Promise<{ applied: boolean; version?: string; reason?: string }> =>
    ipcRenderer.invoke('vault:apply-web-layer'),
  /**
   * 告诉主进程"这一层界面已成功启动"。
   *
   * 主进程用它判断热更层是否白屏：切层后迟迟收不到就回退内置资源（见 main.ts 的看门狗）。
   */
  rendererReady: (): Promise<boolean> => ipcRenderer.invoke('vault:renderer-ready')
} as const

contextBridge.exposeInMainWorld('vault', api)
