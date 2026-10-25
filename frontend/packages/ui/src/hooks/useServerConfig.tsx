import { createContext, useContext, type ReactNode } from 'react'

/** 一个已保存的服务端条目（多服务端：连接、凭据与状态彼此隔离）。 */
export interface ServerEntry {
  /** 稳定标识：优先使用 server_instance_id，回退到规范化后的地址。 */
  key: string
  baseUrl: string
  serverName: string
  instanceId?: string
  /**
   * 已固化的服务端证书指纹（证书 DER 的 SHA-256，小写十六进制）。
   *
   * 服务端使用自建 CA，首次连接无法用公信 CA 验证：连通性探测成功后把探测到的指纹存下来
   * （TOFU），此后渲染进程与本地代理都按该指纹校验服务端证书。
   */
  certSha256?: string
  /** 会话令牌（持久化时受"记住我"控制）。 */
  token?: string
  tokenExpiresAt?: number
  /** 当前会话用户（用于界面展示与权限判定；服务端仍是最终权威）。 */
  userId?: string
  username?: string
  role?: 'super_admin' | 'user'
  /**
   * 是否在启动时自动连接这台服务端。
   *
   * 缺省视为 true（老配置没有这个字段，保持原有"启动即连"的行为）。
   * 只有开启的才会在启动时自动连接；全部关闭时进入服务端选择页。
   */
  autoConnect?: boolean
}

export interface ServerConfigValue {
  servers: ServerEntry[]
  activeKey: string | null
  active: ServerEntry | null
  setActive: (key: string) => void
  /** 新增或更新条目（按 key 匹配），并把该条目设为活动服务端。 */
  upsert: (entry: ServerEntry) => void
  /** 新增条目：已有活动服务端时**不**切换过去（设置页新增服务器用）。 */
  addServer: (entry: ServerEntry) => void
  /** 按 key 更新条目（允许同时修改 key；用于改名/地址/自动连接开关）。 */
  updateServer: (key: string, patch: Partial<ServerEntry>) => void
  remove: (key: string) => void
  /** 更新当前活动服务端（如保存令牌、名称、实例 ID）。 */
  updateActive: (patch: Partial<ServerEntry>) => void
}

const ServerConfigContext = createContext<ServerConfigValue | null>(null)

export function ServerConfigProvider({
  value,
  children
}: {
  value: ServerConfigValue
  children: ReactNode
}): JSX.Element {
  return <ServerConfigContext.Provider value={value}>{children}</ServerConfigContext.Provider>
}

/** 服务端配置（多服务端注册表 + 当前活动服务端）。 */
export function useServerConfig(): ServerConfigValue {
  const value = useContext(ServerConfigContext)
  if (!value) throw new Error('useServerConfig() 必须在 <ServerConfigProvider> 内使用')
  return value
}
