import crypto from 'node:crypto'
import { promises as fs } from 'node:fs'
import path from 'node:path'
import { BrowserWindow, Menu, Tray, app, dialog, ipcMain, nativeImage, shell } from 'electron'

import { AgentManager, listProcesses, runPowerShell, type AgentRequestOptions, type NamedProcess } from './agent'
import { syncAutoStart } from './autostart'
import {
  awaitStartupClientUpdate,
  logClientUpdate,
  maybeHandoffOnStartup,
  startClientUpdateWatch,
  type ClientUpdateWatch,
  type StartupUpdateStatus
} from './clientUpdate'
import { resolveBundledVersion, resolveWebappEntry } from './webapp'

/**
 * Electron 主进程。
 *
 * 职责（严格最小化）：
 *  - 单实例锁；
 *  - 创建窗口（记忆窗口尺寸）；
 *  - 系统托盘（显示/退出）；
 *  - 通过 IPC 读写本地配置与本地代理状态；
 *  - 拉起并守护本地代理 Vault-Agent.exe（找不到时降级，不影响应用可用性）。
 *
 * 安全约束：不开启 nodeIntegration，开启 contextIsolation，preload 只暴露最小 API。
 */

/** 配置目录固定为 %APPDATA%/Vault（与需求一致）。 */
const CONFIG_DIR = path.join(app.getPath('appData'), 'Vault')
const CONFIG_FILE = path.join(CONFIG_DIR, 'config.json')
const WINDOW_STATE_FILE = path.join(CONFIG_DIR, 'window-state.json')
/** 记录"上次清理渲染缓存时的应用版本"（与用户配置同目录，不受缓存清理影响）。 */
const CACHE_VERSION_FILE = path.join(CONFIG_DIR, 'renderer-cache-version.json')
/** 内置渲染层入口（随 exe 打包）：热更层不可用、或加载失败时的兜底。 */
const BUNDLED_RENDERER = path.join(__dirname, '../dist/index.html')
/**
 * 热更切换时的过渡页（vite 把 public/loading.html 原样复制到 dist/）。
 *
 * 纯静态、无外部资源，见 showLayerLoading 的说明。
 */
const LOADING_PAGE = path.join(__dirname, '../dist/loading.html')

const DEV_SERVER_URL = process.env.VITE_DEV_SERVER_URL

let mainWindow: BrowserWindow | null = null
let tray: Tray | null = null
let quitting = false
/** 客户端本体更新的运行期守护（见 clientUpdate.ts）；退出时停掉，避免往回连。 */
let clientUpdateWatch: ClientUpdateWatch | null = null
/**
 * 是否正在切换渲染层。
 *
 * loadFile 是"整页替换"：两次切换并发时后一次会把前一次的加载打断，窗口可能停在
 * 过渡页上。所以同一时刻只允许一次切换，后来的调用直接返回 in_flight。
 */
let applyingLayer = false

/**
 * 内置渲染层（随 exe 打包）的版本号：createWindow 时解析一次，之后用于热更层判定
 * （热更层必须比它新才会被加载，见 resolveWebappEntry）。
 */
let bundledVersion = ''
/**
 * 当前已加载的渲染层入口文件（绝对路径）。
 *
 * 用途：静默热更在代理激活新资源后要求切换渲染层（vault:apply-web-layer），
 * 这里用"解析出的文件 == 已加载文件"来判定无需切换，从而**杜绝重载循环**。
 */
let loadedRendererFile = ''

/**
 * 判定为"启动不起来"的热更层版本记录（与用户配置同目录）。
 *
 * 热更资源是**在现有客户端里**运行的新界面，与当前 exe 的 preload / 代理接口未必兼容；
 * 新层若一启动就崩（JS 异常），窗口就是白板，而 loadFile 本身是成功的，主进程无从得知。
 * 记下坏版本后，之后启动直接跳过它，用户不会每次都被白屏挡在门外。
 */
const BAD_LAYER_FILE = path.join(CONFIG_DIR, 'renderer-bad-layer.json')

/** 切到新层后等待渲染层上报"已启动"的最长时间；超时即判定该层不可用。 */
const LAYER_READY_TIMEOUT_MS = 10_000

/** 已知不可用的热更层版本（空 = 无）；启动时读盘，运行期判定后更新。 */
let badLayerVersion = ''
/** 已加载那一层的激活时间（current.json.activated_at）；同路径重新激活也要重载。 */
let loadedLayerActivatedAt = 0
/** 正在等 ready 上报的层（空 = 当前没有待确认的切换）。 */
let pendingLayerFile = ''
/** 待确认层的版本，判定坏层时写盘用。 */
let pendingLayerVersion = ''
let layerReadyTimer: ReturnType<typeof setTimeout> | null = null

/**
 * 最近一次整包更新请求的时间（毫秒）；代理重启就绪后据此重新加载界面。
 *
 * 用时间戳而不是布尔：代理替换 exe 时会**先退出再重启**，请求本身可能因连接中断而
 * 拿不到响应；只要窗口期内看到代理重新可用，就认为更新已完成并刷新界面。
 */
let updateAppliedAt = 0
/** 整包更新请求到界面重载之间认账的窗口：超出这个时间就不再把代理重启当成更新完成。 */
const UPDATE_RELOAD_WINDOW_MS = 10 * 60 * 1000
/** "代理就绪后补查客户端本体更新"的节流间隔（代理可能反复重启，别每次都读盘）。 */
const CLIENT_HANDOFF_RECHECK_MS = 60 * 1000
/**
 * 启动期更新门是否已经跑过。
 *
 * 只在**首次建窗**时等更新：窗口关掉再从托盘打开、或热更重载界面时都不该再让用户等一次。
 */
let startupUpdateGateDone = false
/** 启动期更新门是否正在等待（期间不让 onChanged 的补查抢着切换，避免重复拉起）。 */
let startupUpdateGateRunning = false

/** 本地代理守护器（单例）。 */
const agent = new AgentManager()

/**
 * 版本变化时清理渲染进程缓存。
 *
 * 目的：避免 Chromium 磁盘缓存让界面加载到旧资源（表现为"客户端一直访问旧内容"）。
 *
 * 取舍：另一种做法是把 userData 路径改为包含应用版本（app.setPath('userData', ...)），
 * 使不同版本各自使用独立的缓存目录。但用户配置与窗口状态本就存放在
 * %APPDATA%/Vault（CONFIG_DIR，与 userData 无关），改 userData 虽不会真正丢配置，
 * 却会让缓存目录随版本无限增长，且更容易被误解为"配置丢失"。
 * 因此这里采用副作用更小、更稳妥的方式：仅在**应用版本变化**时删除
 * Cache / Code Cache / GPUCache 三个缓存目录，用户配置完整保留。
 *
 * 在模块加载阶段即启动（早于窗口创建），确保 Chromium 尚未锁定缓存文件。
 */
async function pruneRendererCacheOnVersionChange(): Promise<void> {
  const version = app.getVersion()
  const record = await readJson<{ version?: string }>(CACHE_VERSION_FILE)
  if (record?.version === version) return
  const userData = app.getPath('userData')
  for (const name of ['Cache', 'Code Cache', 'GPUCache']) {
    try {
      await fs.rm(path.join(userData, name), { recursive: true, force: true })
    } catch {
      // 缓存被占用（Chromium 已启动）时忽略：下次版本变化或手动清理即可，不影响启动。
    }
  }
  await writeJson(CACHE_VERSION_FILE, { version })
}

/** 尽早启动缓存清理；主窗口创建前会 await 它。 */
const rendererCachePruned = pruneRendererCacheOnVersionChange()

/**
 * 已固化的服务端证书指纹（TOFU 首次信任），见 certificate-error 的处理。
 *
 * 与用户配置同目录，按"站点 → 允许的证书指纹集合"存储。
 */
const TRUSTED_CERTS_FILE = path.join(CONFIG_DIR, 'trusted-certs.json')

/** 站点 → 允许的证书指纹集合（小写十六进制）。 */
const trustedCertificates = new Map<string, Set<string>>()

/** 载入已固化的证书指纹；主窗口创建前会 await 它（首个请求发出前必须就绪）。 */
const trustedCertificatesLoaded = (async (): Promise<void> => {
  const record = await readJson<Record<string, unknown>>(TRUSTED_CERTS_FILE)
  if (!record) return
  for (const [origin, value] of Object.entries(record)) {
    if (!Array.isArray(value)) continue
    const set = new Set(value.filter((item): item is string => typeof item === 'string'))
    if (set.size > 0) trustedCertificates.set(origin, set)
  }
})()

/** 把内存中的证书指纹写入磁盘。 */
async function persistTrustedCertificates(): Promise<void> {
  const record: Record<string, string[]> = {}
  for (const [origin, set] of trustedCertificates) record[origin] = [...set]
  await writeJson(TRUSTED_CERTS_FILE, record)
}

/** 规范化站点标识（协议 + 主机 + 端口）；无法解析时返回空串。 */
function originOf(url: string): string {
  try {
    const parsed = new URL(url)
    return `${parsed.protocol}//${parsed.host}`
  } catch {
    return ''
  }
}

/** 计算证书 DER 的 SHA-256（小写十六进制）；算法与 Go 侧 certFingerprint 一致。 */
function certificateSha256(pem: string): string {
  const base64 = pem
    .replace(/-----BEGIN CERTIFICATE-----/g, '')
    .replace(/-----END CERTIFICATE-----/g, '')
    .replace(/\s+/g, '')
  return crypto.createHash('sha256').update(Buffer.from(base64, 'base64')).digest('hex')
}

/**
 * 自签证书放行策略（TOFU 首次信任）。
 *
 * 服务端使用自建 CA，Chromium 会以"证书链不可信"为由拒绝它的**所有**请求
 * （等价于 curl 的 SEC_E_UNTRUSTED_ROOT），表现就是"测试连接成功、保存后却什么也做不了"。
 *
 * 这里**不做无差别放行**：只有实际证书的 SHA-256 与已登记指纹完全一致才放行。
 * 指纹来自本地代理的连通性探测（该探测有意跳过链校验，只回传指纹供人工核对），
 * 用户核对服务端名称/指纹后点击"测试连接/保存"即视为完成首次信任。
 */
app.on('certificate-error', (event, _webContents, url, _error, certificate, callback) => {
  void (async (): Promise<void> => {
    await trustedCertificatesLoaded
    const origin = originOf(url)
    const allowed = origin ? trustedCertificates.get(origin) : undefined
    if (allowed && allowed.has(certificateSha256(certificate.data))) {
      event.preventDefault()
      callback(true)
      return
    }
    callback(false)
  })()
})

interface WindowState {
  width: number
  height: number
  x?: number
  y?: number
  /** 写入时的布局版本；与当前版本不一致时忽略已保存的尺寸（见 loadWindowState）。 */
  layoutVersion?: number
}

/** 默认窗口尺寸：以宽屏为主（内容区需要横向排布表格 + 表单）。 */
const DEFAULT_WINDOW: WindowState = { width: 1440, height: 900 }

/** 窗口最小尺寸。 */
const MIN_WINDOW = { width: 1180, height: 720 }

/**
 * 窗口布局版本。
 *
 * 递增它会让**已保存的窗口尺寸失效**（下次启动回到新的默认尺寸），但保留窗口位置。
 * 用途：调整默认尺寸时让存量用户也能生效——否则"没手动调过窗口"的用户永远沿用旧默认值。
 * 用户自己拖过窗口后写入的是当前版本号，之后再改不会覆盖他的选择。
 */
const WINDOW_LAYOUT_VERSION = 2

async function readJson<T>(file: string): Promise<T | null> {
  try {
    const raw = await fs.readFile(file, 'utf8')
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

async function writeJson(file: string, value: unknown): Promise<void> {
  await fs.mkdir(path.dirname(file), { recursive: true })
  await fs.writeFile(file, JSON.stringify(value, null, 2), 'utf8')
}

async function loadWindowState(): Promise<WindowState> {
  const state = await readJson<WindowState>(WINDOW_STATE_FILE)
  if (!state || typeof state.width !== 'number' || typeof state.height !== 'number') {
    return { ...DEFAULT_WINDOW }
  }
  if (state.layoutVersion !== WINDOW_LAYOUT_VERSION) {
    return { ...DEFAULT_WINDOW, x: state.x, y: state.y }
  }
  return {
    ...state,
    width: Math.max(state.width, MIN_WINDOW.width),
    height: Math.max(state.height, MIN_WINDOW.height)
  }
}

/**
 * 下发给渲染进程的代理状态：**去掉令牌**。
 *
 * 令牌只留在主进程，渲染进程的所有代理调用都经 vault:agent-request 转发（见 registerIpc）。
 */
function publicAgentInfo(): Omit<ReturnType<AgentManager['getInfo']>, 'token'> {
  const { token: _token, ...rest } = agent.getInfo()
  return rest
}

/** 每个渲染进程可以同时开多条 SSE（全局事件流 + 页面级性能流）；key 为 webContents.id:path。 */
const agentStreams = new Map<string, () => void>()

function agentStreamKey(id: number, path: string): string {
  return `${id}:${path}`
}

function closeAgentStream(key: string): void {
  const close = agentStreams.get(key)
  if (!close) return
  agentStreams.delete(key)
  close()
}

/** 关闭某个渲染进程的全部 SSE（页面关闭/刷新时调用）。 */
function closeAgentStreams(id: number): void {
  const prefix = `${id}:`
  for (const key of [...agentStreams.keys()]) {
    if (key.startsWith(prefix)) closeAgentStream(key)
  }
}

/** 只允许渲染进程访问本代理的 /agent/ 路径；非法值退回全局事件流。 */
function normalizeAgentStreamPath(value: unknown): string {
  const path = typeof value === 'string' ? value : ''
  if (!path.startsWith('/agent/') || path.startsWith('//') || path.includes('..')) {
    return '/agent/events'
  }
  return path
}

/** 只允许渲染进程访问代理的 /agent/ 前缀路径，避免借主进程之手访问任意地址。 */
function parseAgentRequest(value: unknown): AgentRequestOptions | null {
  if (typeof value !== 'object' || value === null) return null
  const input = value as { method?: unknown; path?: unknown; body?: unknown; timeoutMs?: unknown }
  const method = typeof input.method === 'string' ? input.method.toUpperCase() : ''
  if (!['GET', 'POST', 'PATCH', 'DELETE'].includes(method)) return null
  const requestPath = typeof input.path === 'string' ? input.path : ''
  // 必须是以 /agent/ 开头的站内路径：不允许协议、主机或 .. 逃逸。
  if (!requestPath.startsWith('/agent/') || requestPath.startsWith('//') || requestPath.includes('..')) {
    return null
  }
  const timeoutMs =
    typeof input.timeoutMs === 'number' && Number.isFinite(input.timeoutMs) && input.timeoutMs > 0
      ? Math.min(input.timeoutMs, 300000)
      : undefined
  return { method, path: requestPath, body: input.body, timeoutMs }
}

/**
 * 从代理返回体里取"开机自启"开关值。
 *
 * 两种形态都要认：GET /agent/config 直接返回 Config（字段在顶层），
 * PATCH /agent/config 返回 `{config: ...}`（字段在下一层）。
 */
function readStartAtLogin(payload: unknown): boolean | null {
  if (typeof payload !== 'object' || payload === null) return null
  const direct = (payload as { start_at_login?: unknown }).start_at_login
  if (typeof direct === 'boolean') return direct
  const nested = (payload as { config?: unknown }).config
  if (typeof nested === 'object' && nested !== null) {
    const value = (nested as { start_at_login?: unknown }).start_at_login
    if (typeof value === 'boolean') return value
  }
  return null
}

/**
 * 等代理就绪后，把系统自启项对齐到配置里的 start_at_login。
 *
 * 为什么必须在这里做：配置开关由渲染层经代理的 PATCH /agent/config 写入，而
 * "开机自启"是操作系统层面的登记（计划任务 / Run 项），代理不管这件事。
 * 没有这段同步，开关就只是往 JSON 里存了一个没人读的布尔值——真实反馈"开机自启无效"。
 *
 * 代理是异步拉起的，所以轮询等它就绪（最多约 30s）。**读不到配置时什么都不做**：
 * 在不知道用户意图的情况下把自启项删掉，比不生效更糟。
 */
async function syncAutoStartFromAgent(): Promise<void> {
  for (let attempt = 0; attempt < 30; attempt += 1) {
    const result = await agent.request({ method: 'GET', path: '/agent/config', timeoutMs: 3000 })
    if (result.status === 200) {
      const enabled = readStartAtLogin(result.payload)
      if (enabled !== null) await syncAutoStart(enabled)
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 1000))
  }
  console.warn('[vault] 代理未就绪，跳过开机自启校准（保持系统现有登记不变）')
}

/** 过渡页支持的语言（与 @vault/ui 的 Language 一致）。 */
const LOADING_LANGUAGES = ['zh-CN', 'en-US', 'ja-JP', 'ko-KR'] as const

/**
 * 过渡页用哪种语言：取自客户端配置（%APPDATA%/Vault/config.json 的 language）。
 *
 * 读不到、值不认识时用 zh-CN —— 过渡页自己也带一份中文兜底，绝不会空白。
 */
async function loadingLanguage(): Promise<string> {
  const config = await readJson<{ language?: unknown }>(CONFIG_FILE)
  const value = typeof config?.language === 'string' ? config.language.trim() : ''
  return (LOADING_LANGUAGES as readonly string[]).includes(value) ? value : 'zh-CN'
}

/**
 * 显示"正在切换到新版本界面"的过渡页。
 *
 * 为什么需要：renderer 热更生效时主进程直接 `loadFile(新层/index.html)`，这一下旧页面立刻就
 * 被卸载，而新层的 index.html 与 bundle 还没解析完 —— 中间这段时间窗口就是**一块白板**
 * （真实反馈："客户端在自动更新时白屏"）。过渡页是 dist 里的一个纯静态 HTML（内联样式与
 * 脚本，零外部请求），加载即出画面，正好把这段空窗盖住。
 *
 * 过渡页缺失（例如开发态直接跑 vite dev，根本没有 dist）时不阻断切换：只打一行 WARN，
 * 继续加载新层，行为与改造前一致。
 */
async function showLayerLoading(): Promise<void> {
  if (!mainWindow || mainWindow.isDestroyed()) return
  try {
    await mainWindow.loadFile(LOADING_PAGE, { query: { lang: await loadingLanguage() } })
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    console.warn(`[vault] 过渡页加载失败（直接切换渲染层）：${reason}`)
  }
}

/**
 * 把启动期更新进度写到全屏过渡页上。
 *
 * 文案表在 `public/loading.html` 的 BOOT 里（按语言取）：主进程只传状态 key 与百分比，
 * 不在这里拼中文，避免四种语言的文案散落在主进程里。
 */
function setBootStatus(status: StartupUpdateStatus, percent?: number): void {
  const win = mainWindow
  if (!win || win.isDestroyed()) return
  const percentArg = typeof percent === 'number' ? String(percent) : 'null'
  void win.webContents
    .executeJavaScript(
      `window.__vaultBootStatus && window.__vaultBootStatus(${JSON.stringify(status)}, ${percentArg})`,
      true
    )
    .catch(() => undefined)
}

function registerIpc(): void {
  ipcMain.handle('vault:get-config', async () => readJson<unknown>(CONFIG_FILE))
  ipcMain.handle('vault:set-config', async (_event, value: unknown) => {
    await writeJson(CONFIG_FILE, value)
    return true
  })
  ipcMain.handle('vault:config-path', () => CONFIG_DIR)
  ipcMain.handle('vault:agent-info', () => publicAgentInfo())
  ipcMain.handle('vault:agent-request', async (_event, options: unknown) => {
    const parsed = parseAgentRequest(options)
    if (!parsed) return { status: 0, error: 'network', message: 'invalid_request' }
    const isUpdateApply = parsed.path.startsWith('/agent/update/apply')
    if (isUpdateApply) updateAppliedAt = Date.now()
    const result = await agent.request(parsed)
    // 没受理成功就不算更新（例如没有可用产物）：别让下一次无关的代理重启误触发刷新。
    if (isUpdateApply && (result.status < 200 || result.status >= 300)) updateAppliedAt = 0
    // 开机自启开关存在代理配置里，系统自启项由主进程维护：配置一写入就立刻对齐，
    // 用户拨完开关不必等下次启动才生效。
    if (parsed.method === 'PATCH' && parsed.path === '/agent/config' && result.status === 200) {
      const enabled = readStartAtLogin(result.payload)
      if (enabled !== null) void syncAutoStart(enabled)
    }
    return result
  })
  // 渲染层上报"已启动"：撤掉热更层白屏看门狗（见 armLayerWatchdog）。
  ipcMain.handle('vault:renderer-ready', () => {
    clearLayerWatchdog()
    return true
  })
  ipcMain.handle('vault:agent-events-start', (event, pathArg: unknown) => {
    const id = event.sender.id
    const path = normalizeAgentStreamPath(pathArg)
    const key = agentStreamKey(id, path)
    closeAgentStream(key)
    const close = agent.openEventStream(path, (message) => {
      if (event.sender.isDestroyed()) return
      event.sender.send('vault:agent-event', { ...message, stream: path })
    })
    agentStreams.set(key, close)
    // 渲染进程销毁（关闭窗口/刷新）时释放该进程的所有长连接，避免连接泄漏。
    event.sender.once('destroyed', () => closeAgentStreams(id))
    return true
  })
  ipcMain.handle('vault:agent-events-stop', (event, pathArg: unknown) => {
    closeAgentStream(agentStreamKey(event.sender.id, normalizeAgentStreamPath(pathArg)))
    return true
  })
  // 登记服务端证书指纹（TOFU 首次信任）；只有指纹与登记值一致的自签证书才会被放行。
  ipcMain.handle('vault:trust-certificate', async (_event, payload: unknown) => {
    if (typeof payload !== 'object' || payload === null) return false
    const input = payload as { serverUrl?: unknown; sha256?: unknown }
    const serverUrl = typeof input.serverUrl === 'string' ? input.serverUrl : ''
    const sha256 = typeof input.sha256 === 'string' ? input.sha256.trim().toLowerCase() : ''
    const origin = originOf(serverUrl)
    if (!origin || !/^[0-9a-f]{64}$/.test(sha256)) return false
    // 必须先载入历史指纹，否则会把已有记录覆盖掉。
    await trustedCertificatesLoaded
    const set = trustedCertificates.get(origin) ?? new Set<string>()
    set.add(sha256)
    trustedCertificates.set(origin, set)
    await persistTrustedCertificates()
    return true
  })
  ipcMain.handle('vault:open-path', async (_event, target: unknown) => {
    if (typeof target !== 'string' || !target) return 'invalid_path'
    return shell.openPath(target)
  })
  // 选择源目录（单选目录）；用户取消时返回 canceled=true。
  ipcMain.handle('vault:select-directory', async () => {
    const result = await dialog.showOpenDialog({ properties: ['openDirectory'] })
    if (result.canceled || result.filePaths.length === 0) return { canceled: true }
    return { canceled: false, path: result.filePaths[0] }
  })
  // 为本地挂载点创建快捷方式（Windows Shell Link）。
  //
  // dir 为空时落到系统桌面；name 缺省用目标目录名。只支持 Windows：
  // shell.writeShortcutLink 是 Windows 专有 API，其它平台如实返回 unsupported。
  ipcMain.handle('vault:create-shortcut', async (_event, payload: unknown) => {
    const input = (payload ?? {}) as { target?: unknown; name?: unknown; dir?: unknown }
    const target = typeof input.target === 'string' ? input.target.trim() : ''
    if (!target) return { ok: false, reason: 'invalid_target' }
    if (process.platform !== 'win32') return { ok: false, reason: 'unsupported' }
    const dir =
      typeof input.dir === 'string' && input.dir.trim() ? input.dir.trim() : app.getPath('desktop')
    const rawName =
      typeof input.name === 'string' && input.name.trim() ? input.name.trim() : path.basename(target)
    const safeName = rawName.replace(/[\\/:*?"<>|]/g, '_').slice(0, 120) || 'Vault'
    try {
      await fs.mkdir(dir, { recursive: true })
      const shortcutPath = path.join(dir, `${safeName}.lnk`)
      const ok = shell.writeShortcutLink(shortcutPath, {
        target,
        cwd: path.dirname(target),
        description: 'Vault repository'
      })
      return ok ? { ok: true, path: shortcutPath } : { ok: false, reason: 'failed' }
    } catch (error) {
      return { ok: false, reason: error instanceof Error ? error.message : String(error) }
    }
  })
  ipcMain.handle('vault:show-item', async (_event, target: unknown) => {
    if (typeof target === 'string' && target) shell.showItemInFolder(target)
    return true
  })
  ipcMain.handle('vault:quit', async () => {
    await agent.shutdown()
    quitting = true
    app.quit()
  })
  // 立即重启客户端，让已激活的渲染层热更资源生效（见 resolveWebappEntry）。
  ipcMain.handle('vault:relaunch', async () => {
    await agent.shutdown()
    quitting = true
    app.relaunch()
    app.exit(0)
  })
  // 让"刚刚激活的热更渲染层"立刻生效：重新解析热更目录并加载，无需重启整个客户端。
  //
  // 由渲染层在收到代理的 web_update(activated) 事件后调用（静默热更，见
  // internal/agent 的 onSessionEstablished）。返回 applied=false 表示"当前已是最新层"
  // （或没有可用的热更层）——调用方据此无需做任何事，也不会形成重载循环。
  ipcMain.handle('vault:apply-web-layer', async () => {
    const win = mainWindow
    if (!win || win.isDestroyed()) return { applied: false, reason: 'no_window' }
    if (applyingLayer) return { applied: false, reason: 'in_flight' }
    const entry = await resolveWebappEntry(bundledVersion)
    if (!entry.ok) return { applied: false, reason: entry.reason }
    // 已经判定跑不起来的版本不再尝试：否则每次激活都会白屏一次再回退。
    if (entry.version === badLayerVersion) return { applied: false, reason: 'marked_bad' }
    // 同路径但激活时间变了 = 服务端用同一版本号重发了资源（目录被重建），必须重载。
    if (entry.file === loadedRendererFile && entry.activatedAt === loadedLayerActivatedAt) {
      return { applied: false, reason: 'already_loaded' }
    }
    applyingLayer = true
    try {
      // 先盖过渡页，再切新层：两段 loadFile 之间的空白由它顶住（见 showLayerLoading）。
      await showLayerLoading()
      await win.loadFile(entry.file)
      loadedRendererFile = entry.file
      loadedLayerActivatedAt = entry.activatedAt
      // 切完还不算数：等新层上报"已启动"，超时同样回退（见 armLayerWatchdog）。
      armLayerWatchdog(entry.file, entry.version)
      console.info(`[vault] 已自动切换到新渲染层：${entry.dir}`)
      return { applied: true, version: entry.dir }
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error)
      console.warn(`[vault] 切换渲染层失败，回退内置资源：${reason}`)
      await markLayerBad(entry.version, `load_failed:${reason}`)
      // 过渡页已经把旧界面换掉了，失败时绝不能把它留在窗口上：回到随 exe 打包的界面。
      clearLayerWatchdog()
      try {
        await win.loadFile(BUNDLED_RENDERER)
        loadedRendererFile = BUNDLED_RENDERER
        loadedLayerActivatedAt = 0
      } catch (fallbackError) {
        const fallbackReason =
          fallbackError instanceof Error ? fallbackError.message : String(fallbackError)
        console.warn(`[vault] 回退内置渲染层也失败：${fallbackReason}`)
      }
      return { applied: false, reason }
    } finally {
      applyingLayer = false
    }
  })
}

/** 代理状态变化（含重启换端口）时广播给所有渲染进程。 */
function broadcastAgentInfo(): void {
  const info = publicAgentInfo()
  for (const window of BrowserWindow.getAllWindows()) {
    if (!window.isDestroyed()) window.webContents.send('vault:agent-changed', info)
  }
}

/** 关闭窗口：仍有活跃挂载时先确认；选择"仅关闭窗口"则最小化到托盘。 */
async function handleCloseRequest(window: BrowserWindow): Promise<void> {
  const count = await agent.activeMountCount()
  if (count <= 0) {
    window.hide()
    return
  }
  const { response } = await dialog.showMessageBox(window, {
    type: 'warning',
    title: 'Vault',
    message: `仍有 ${count} 个存储库处于挂载状态，确定退出吗？`,
    buttons: ['退出', '仅关闭窗口', '取消'],
    defaultId: 1,
    cancelId: 2,
    noLink: true
  })
  if (response === 0) {
    await agent.shutdown()
    quitting = true
    app.quit()
    return
  }
  if (response === 1) window.hide()
}

/**
 * 切换器接手后的收尾：隐窗 → 停代理 → 退出自身。
 *
 * 顺序不能变：
 *  1. 先隐藏窗口 —— 用户看不到窗口被拆掉，也就没有"闪屏"；
 *  2. 再让代理优雅退出 —— 释放 `Global\VaultAgent` 单实例互斥体。老流程用 SIGKILL 杀
 *     旧进程，代理成了孤儿占着这个互斥体，新客户端的代理起不来，界面上就是一片
 *     network.error，只能手动点"重启服务"；
 *  3. 最后 `app.exit(0)` —— 立即释放客户端单实例锁，切换器的稳定期（800ms）过后才会
 *     启动新版本，新进程不会撞上"已有实例在运行"。
 *
 * `app.exit` 不会触发 before-quit / will-quit：这是刻意的，代理已经自己退出，
 * 不需要 will-quit 里的守护逻辑再插手（重复 stop 还会去杀已经不存在的进程）。
 */
let switchingForUpdate = false
async function handleSwitching(): Promise<void> {
  if (switchingForUpdate) return
  switchingForUpdate = true
  logClientUpdate('切换器已接手：隐藏窗口、停止本地代理、退出自身，由切换器启动新版本')
  try {
    mainWindow?.hide()
  } catch {
    // 窗口可能已销毁：收尾流程不能因此中断。
  }
  await agent.shutdown(3000)
  quitting = true
  app.exit(0)
}

async function createWindow(): Promise<void> {
  const state = await loadWindowState()
  mainWindow = new BrowserWindow({
    width: state.width,
    height: state.height,
    x: state.x,
    y: state.y,
    minWidth: MIN_WINDOW.width,
    minHeight: MIN_WINDOW.height,
    show: false,
    backgroundColor: '#F5F5F7',
    title: 'Vault',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true
    }
  })

  mainWindow.once('ready-to-show', () => mainWindow?.show())

  const persistState = async (): Promise<void> => {
    if (!mainWindow || mainWindow.isDestroyed()) return
    const bounds = mainWindow.getNormalBounds()
    await writeJson(WINDOW_STATE_FILE, {
      width: bounds.width,
      height: bounds.height,
      x: bounds.x,
      y: bounds.y,
      layoutVersion: WINDOW_LAYOUT_VERSION
    })
  }

  mainWindow.on('resized', () => void persistState())
  mainWindow.on('moved', () => void persistState())
  // 主框架加载失败 / 渲染进程崩溃：若正在确认的是热更层，回退内置资源，绝不把用户留在白板上。
  mainWindow.webContents.on(
    'did-fail-load',
    (_event, errorCode, errorDescription, _url, isMainFrame) => {
      // -3 = ERR_ABORTED：切层时主动中止上一次导航，属正常现象。
      if (!isMainFrame || errorCode === -3) return
      if (pendingLayerFile === '') return
      void fallbackFromBadLayer(`did_fail_load(${errorCode}:${errorDescription})`)
    }
  )
  mainWindow.webContents.on('render-process-gone', (_event, details) => {
    if (pendingLayerFile === '') return
    void fallbackFromBadLayer(`render_process_gone(${details.reason})`)
  })

  mainWindow.on('closed', () => {
    mainWindow = null
  })

  // 关闭窗口不直接退出：有活跃挂载时先确认，否则最小化到托盘。
  mainWindow.on('close', (event) => {
    if (quitting || !mainWindow) return
    event.preventDefault()
    void handleCloseRequest(mainWindow)
  })

  // 外部链接一律用系统浏览器打开，避免在应用内导航。
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    void shell.openExternal(url)
    return { action: 'deny' }
  })

  if (DEV_SERVER_URL) {
    await mainWindow.loadURL(DEV_SERVER_URL)
    return
  }
  // 生产环境：解析内置层版本后加载渲染层（详见 loadRendererLayer）。
  bundledVersion = await resolveBundledVersion(app.getAppPath(), app.getVersion())

  /*
   * 启动期更新门：勾了「自动更新」的话，先在全屏启动页把客户端本体更新装完，再进界面。
   *
   * 没有这道门时用户会先看到登录/主界面，随后整包更新把窗口整个重启掉（或者顶栏提示
   * "版本不一致，建议重启"）—— 主观感受就是"更新把我从页面里踢出去了"。用户的要求是
   * 留在启动页上，看它检查、下载、装好，然后直接进新版本。
   *
   * 新进程接管后本进程会被结束，所以这里**不要**再加载界面：停在过渡页即可，避免
   * 闪一下旧界面再被关掉。
   */
  if (!startupUpdateGateDone) {
    startupUpdateGateDone = true
    startupUpdateGateRunning = true
    await showLayerLoading()
    setBootStatus('checking')
    let outcome: string = 'failed'
    try {
      outcome = await awaitStartupClientUpdate(agent, bundledVersion, {
        onStatus: setBootStatus,
        onSwitching: handleSwitching
      })
    } catch (error) {
      // 更新门本身出问题绝不能把用户锁在启动页上：记日志，照常进界面。
      const reason = error instanceof Error ? error.message : String(error)
      logClientUpdate(`启动期更新门异常，直接进入界面：${reason}`)
    } finally {
      startupUpdateGateRunning = false
    }
    if (outcome === 'switched') {
      // 正常路径下 onSwitching 里已经 app.exit 了，走不到这里；保留是为了兜底
      // （onSwitching 未生效时，也要把"隐窗 → 停代理 → 退出"补完，不能停在过渡页不动）。
      logClientUpdate('启动期已发起本体更新切换，等待切换器启动新版本')
      await handleSwitching()
      return
    }
    logClientUpdate(`启动期更新门结束（${outcome}），继续加载界面`)
  }

  await loadRendererLayer()
}

/**
 * 加载渲染层：优先热更资源（%ProgramData%\Vault\webapp\<version>），但**只在热更层比
 * 内置层新时**才采用（比较见 resolveWebappEntry 的说明：否则旧的热更资源会把界面永久
 * 钉在老版本上）。任何一步失败都回退打包内置的 dist/index.html，绝不让界面打不开。
 *
 * 记录实际加载的文件路径，供 applyWebLayer 判断"要不要切换"（同路径即视为已切换，
 * 从而杜绝重载循环）。
 */
async function loadRendererLayer(): Promise<void> {
  if (!mainWindow) return
  badLayerVersion = await readBadLayerVersion()
  const entry = await resolveWebappEntry(bundledVersion)
  if (entry.ok && entry.version !== badLayerVersion) {
    try {
      // 先盖过渡页：热更层万一启动即崩（见 armLayerWatchdog 的说明），用户看到的是
      // "正在加载"而不是一块白板，10s 后自动回退内置层。
      await showLayerLoading()
      await mainWindow.loadFile(entry.file)
      loadedRendererFile = entry.file
      loadedLayerActivatedAt = entry.activatedAt
      // loadFile 成功不代表界面能跑：等它上报"已启动"，超时就回退（见 fallbackFromBadLayer）。
      armLayerWatchdog(entry.file, entry.version)
      console.info(`[vault] 已加载热更渲染层：${entry.dir}（内置版本 ${bundledVersion}）`)
      return
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error)
      console.warn(`[vault] 热更渲染层加载失败，回退内置资源：${reason}`)
      // 记下坏版本：否则每次启动都会在这一个坏层上再失败一次。
      await markLayerBad(entry.version, `load_failed:${reason}`)
    }
  } else if (entry.ok) {
    console.warn(`[vault] 跳过此前判定不可用的热更渲染层 ${entry.version}，使用内置资源`)
  } else {
    console.warn(`[vault] 未启用热更渲染层（${entry.reason}），使用内置资源`)
  }
  await mainWindow.loadFile(BUNDLED_RENDERER)
  loadedRendererFile = BUNDLED_RENDERER
  loadedLayerActivatedAt = 0
  console.info(`[vault] 已加载内置渲染层（版本 ${bundledVersion}）`)
}

/** 读取已记录的坏层版本；无记录 / 读不到返回空串。 */
async function readBadLayerVersion(): Promise<string> {
  const record = await readJson<{ version?: unknown }>(BAD_LAYER_FILE)
  return typeof record?.version === 'string' ? record.version.trim() : ''
}

/** 记录一个"启动不起来"的热更层版本，之后启动直接跳过。 */
async function markLayerBad(version: string, reason: string): Promise<void> {
  if (version === '') return
  badLayerVersion = version
  console.warn(`[vault] 渲染层 ${version} 判定不可用（${reason}），回退内置资源`)
  try {
    await writeJson(BAD_LAYER_FILE, { version, reason, at: new Date().toISOString() })
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error)
    console.warn(`[vault] 坏层记录写入失败：${detail}`)
  }
}

/** 撤掉待确认状态（收到 ready 上报、或已经处理完一次失败）。 */
function clearLayerWatchdog(): void {
  if (layerReadyTimer) {
    clearTimeout(layerReadyTimer)
    layerReadyTimer = null
  }
  pendingLayerFile = ''
  pendingLayerVersion = ''
}

/**
 * 新层就位后开始计时：限期内没等到渲染层上报"已启动"就认为它白屏了。
 *
 * 这是**唯一**能发现"loadFile 成功、页面却崩溃"的手段——Electron 不会因为渲染进程的
 * JS 异常而让 loadFile 失败，窗口就那样停在白板上。
 */
function armLayerWatchdog(file: string, version: string): void {
  clearLayerWatchdog()
  pendingLayerFile = file
  pendingLayerVersion = version
  layerReadyTimer = setTimeout(() => {
    layerReadyTimer = null
    void fallbackFromBadLayer('ready_timeout')
  }, LAYER_READY_TIMEOUT_MS)
}

/**
 * 当前加载的热更层没跑起来：记为坏层并回退随 exe 打包的内置界面。
 *
 * 内置层是最后一道兜底，它再失败就无路可退，因此这里只处理热更层。
 */
async function fallbackFromBadLayer(reason: string): Promise<void> {
  const file = pendingLayerFile
  const version = pendingLayerVersion
  // 没有待确认的切换 = 已经处理过（或本来就没切层），直接返回，杜绝回退循环。
  if (file === '' || file === BUNDLED_RENDERER) {
    clearLayerWatchdog()
    return
  }
  clearLayerWatchdog()
  await markLayerBad(version, reason)
  const win = mainWindow
  if (!win || win.isDestroyed()) return
  try {
    await showLayerLoading()
    await win.loadFile(BUNDLED_RENDERER)
    loadedRendererFile = BUNDLED_RENDERER
    console.info('[vault] 已回退到内置渲染层')
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error)
    console.warn(`[vault] 回退内置渲染层失败：${detail}`)
  }
}

/**
 * 托盘图标：内嵌 32×32 PNG（Vault 标记：圆角方形底 + 白色 V），
 * 内嵌而非读文件，避免打包后找不到资源；32px 在 100%/200% 缩放下都是整数倍缩放，最清晰。
 */
function createTrayIcon(): Electron.NativeImage {
  const png =
    'iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAAAXNSR0IArs4c6QAAAARnQU1BAACxjwv8YQUAAAAJcEhZcwAADsMAAA7DAcdvqGQAAAICSURBVFhH7ZchTMNAFIbn2BUSCAocOHCTkziQOHBDIXaFgYAQBFlCCAgMwYFAIMBgERMIEIAiCEKCgQQBhgCGJYh75Gfp1nuv67p2TRD8yWe6//r+vrvrrpnMv0LUW6K+bk1jSlM5LhjvFGmA3ztUPS6NKNfcOC5Rp1CueXGKZoLXEnJcmnFcU+U36BToCq9Zl1OkXJrF6zTrhNLmXphTANORKVDWKt5VoiFuTBM1Z/JWAOXSJDelidJmwQ6gqcxNHsOrRLl1eT0MjBldk9c9xGJsFmD5hOqaP5a/BzG1R/TxVRtzeCV/B5EDPL01AlS/ifJb0uNncIno9bMxBsI17oscoHJn3+z6UXr87J7ZfjwA94DIAfDEXLOH0teuN3IAwJ8KLQ5q6+2z7QvrVlsBguZ1/8L2YIH61Wq9tBUAoJVcXgFsOW/Ve0LX+D0SBQDnD3YRtBzXsdX8Qrf6F+X4xAHwxGitX7w4hPcAH8uJFQBsV3g5W9i2fEwQsQOgtXxBekJ3or6yYwcAaHGQNk6ltxmJAgD+hsQbr9XC85M4AP7pvK2H1o/vSE8YIkDtLCiNYWD/Fw6izztjxQ7wex4UptTAAcgKAClt3rkxHUw18FshzjTEQcy/X45rjviAjqLNpTgRc2GBpPF9oLTZbFncE47pWChoV1Ky2kzjc4/X+BP6AYrvFkLf9xIUAAAAAElFTkSuQmCC'
  return nativeImage.createFromDataURL(`data:image/png;base64,${png}`)
}

function createTray(): void {
  try {
    tray = new Tray(createTrayIcon())
    tray.setToolTip('Vault')
    tray.setContextMenu(
      Menu.buildFromTemplate([
        { label: '显示窗口', click: () => showWindow() },
        // 版本与开源许可不再打在托盘上：它们是"偶尔核对一次"的信息，已集中进客户端侧边栏的
        // 「关于」（见 packages/ui 的 AboutPage），托盘只留最常用的动作。
        { type: 'separator' },
        {
          label: '退出',
          click: () => {
            void agent.shutdown().finally(() => {
              quitting = true
              app.quit()
            })
          }
        }
      ])
    )
    tray.on('click', () => showWindow())
  } catch {
    // 无图形环境（如 CI）时静默跳过托盘，不影响窗口本身。
    tray = null
  }
}

function showWindow(): void {
  if (!mainWindow) {
    void createWindow()
    return
  }
  if (mainWindow.isMinimized()) mainWindow.restore()
  mainWindow.show()
  mainWindow.focus()
}

/** 结束旧实例后等待其退出（Electron 的单实例锁由系统在进程结束后释放）的最长时间。 */
const TAKEOVER_WAIT_MS = 3000
const TAKEOVER_POLL_MS = 150
/** 重启自身时写入的标记：确保重启后的进程不会再尝试接管（防极端情况下反复重启）。 */
const TAKEOVER_DONE_ENV = 'VAULT_TAKEOVER_DONE'
/**
 * 接管时经环境变量把"旧安装目录"传给重启后的进程。
 *
 * 为什么需要传递：旧的 Vault-Agent.exe 位于**旧安装目录**下，默认不在客户端的
 * "可清理目录"白名单里，会一直占着 Global\VaultAgent 锁导致新代理起不来。
 * 而接管动作发生在即将退出的进程里，白名单必须交给重启后的进程才能真正生效。
 */
const TAKEOVER_ROOTS_ENV = 'VAULT_TAKEOVER_STALE_ROOTS'

/** 应用接管时继承下来的"可清理代理目录"，并清除环境变量（避免再传给子进程）。 */
function applyInheritedStaleRoots(): void {
  const raw = process.env[TAKEOVER_ROOTS_ENV]
  delete process.env[TAKEOVER_ROOTS_ENV]
  if (!raw) return
  for (const dir of raw.split(path.delimiter)) {
    const trimmed = dir.trim()
    if (trimmed) agent.allowStaleRoot(trimmed)
  }
}

type TakeoverOutcome =
  /** 已有实例（同一安装目录的重复启动）：保持原行为，让位给它。 */
  | { kind: 'same' }
  /** 已结束旧实例：重启自身以干净地取得单实例锁。 */
  | { kind: 'takeover' }
  /** 运行中的实例版本更高或无法结束：提示用户先退出它。 */
  | { kind: 'blocked'; path: string }

/** 解析 "0.1.9" / "0.1.9.0" 形式的版本号，便于逐段比较。 */
function parseVersion(value: string): number[] {
  return value.split('.').map((part) => {
    const parsed = Number.parseInt(part.replace(/[^0-9].*$/, ''), 10)
    return Number.isFinite(parsed) ? parsed : 0
  })
}

/** a 是否比 b 新（逐段比较，长度不同按 0 补齐）。 */
function isNewerVersion(a: string, b: string): boolean {
  const left = parseVersion(a)
  const right = parseVersion(b)
  const length = Math.max(left.length, right.length)
  for (let i = 0; i < length; i += 1) {
    const x = left[i] ?? 0
    const y = right[i] ?? 0
    if (x !== y) return x > y
  }
  return false
}

/** 一次 PowerShell 调用读取多个可执行文件的 FileVersion（由 electron-builder 写入）。 */
async function readFileVersions(paths: string[]): Promise<Map<string, string>> {
  const versions = new Map<string, string>()
  if (paths.length === 0) return versions
  const quoted = paths.map((item) => `'${item.replace(/'/g, "''")}'`).join(',')
  const script =
    `@(${quoted}) | ForEach-Object { ` +
    `[pscustomobject]@{ Path = $_; Version = (Get-Item -LiteralPath $_).VersionInfo.FileVersion } } | ` +
    `ConvertTo-Json -Compress`
  try {
    const stdout = (await runPowerShell(script)).trim()
    if (!stdout) return versions
    const parsed = JSON.parse(stdout) as unknown
    for (const row of Array.isArray(parsed) ? parsed : [parsed]) {
      if (typeof row !== 'object' || row === null) continue
      const value = row as { Path?: unknown; Version?: unknown }
      if (typeof value.Path === 'string' && typeof value.Version === 'string') {
        versions.set(value.Path, value.Version)
      }
    }
  } catch {
    // 读不到版本：留空，调用方按"无法确认"处理（保守起见不接管）。
  }
  return versions
}

/**
 * 判断"拿不到单实例锁"时该怎么办。
 *
 * 背景：Electron 的单实例锁按 userData（%APPDATA%/Vault）判定，**与安装目录无关**。
 * 因此当本机还跑着另一个安装目录下的旧客户端时，新客户端会拿不到锁、静默退出，旧进程
 * 把窗口顶到前台——用户看到的永远是旧界面（即"客户端一直访问旧内容"）。
 *
 * 处置规则：
 *   1. 运行中的实例与本进程同目录 → 这是同一份安装的重复启动，让位（原行为）；
 *   2. 运行中的实例来自其它目录且版本更低 → 结束它并回收其代理，重启自身；
 *   3. 运行中的实例版本更高 → 提示用户先退出它，不做任何强杀。
 */
async function takeoverOlderInstance(): Promise<TakeoverOutcome> {
  // 已经接管过一次（本进程由接管后的 relaunch 启动）：不再重复，直接让位。
  if (process.env[TAKEOVER_DONE_ENV] === '1') return { kind: 'same' }

  const ownDir = path.dirname(app.getPath('exe'))
  let processes: NamedProcess[]
  try {
    processes = await listProcesses('Vault.exe')
  } catch {
    return { kind: 'same' } // 枚举失败：保持原行为，不冒险
  }

  const others = processes.filter((proc) => proc.pid !== process.pid && proc.exePath !== '')
  if (others.length === 0) return { kind: 'same' }

  const ownResolved = path.resolve(ownDir).toLowerCase()
  const sameInstall = others.some(
    (proc) => path.resolve(path.dirname(proc.exePath)).toLowerCase() === ownResolved
  )
  if (sameInstall) return { kind: 'same' }

  const ownVersion = app.getVersion()
  const versions = await readFileVersions(others.map((proc) => proc.exePath))
  // 只有当"每一个运行中的实例版本都比本进程低"时才接管；读不到版本同样视为不能接管。
  const blocker = others.find((proc) => {
    const other = versions.get(proc.exePath)
    return !other || !isNewerVersion(ownVersion, other)
  })
  if (blocker) return { kind: 'blocked', path: blocker.exePath }

  // 本进程版本更高：结束旧实例，并把它们的目录记入"可清理代理"白名单。
  const staleRoots = new Set<string>()
  for (const proc of others) {
    const dir = path.dirname(proc.exePath)
    staleRoots.add(dir)
    agent.allowStaleRoot(dir)
    try {
      process.kill(proc.pid, 'SIGKILL')
    } catch {
      // 结束失败（权限不足/已自行退出）：下面的存活检查会再次判定。
    }
  }
  // 白名单要交给重启后的进程才能真正生效（本进程即将退出）。
  process.env[TAKEOVER_ROOTS_ENV] = [...staleRoots].join(path.delimiter)

  const deadline = Date.now() + TAKEOVER_WAIT_MS
  for (;;) {
    const alive = (await listProcesses('Vault.exe').catch(() => [])).filter(
      (proc) => proc.pid !== process.pid && proc.exePath !== ''
    )
    if (alive.length === 0) return { kind: 'takeover' }
    if (Date.now() >= deadline) return { kind: 'blocked', path: alive[0]?.exePath ?? others[0].exePath }
    await new Promise((resolve) => setTimeout(resolve, TAKEOVER_POLL_MS))
  }
}

/** 作为主实例运行（唯一实例，正常路径）。 */
function startPrimaryInstance(): void {
  app.on('second-instance', () => showWindow())

  app.whenReady().then(async () => {
    // 应用自带完整 UI，不需要 Electron 默认的 File/Edit/View/Window/Help 菜单栏。
    // 托盘菜单（createTray）是独立对象，不受影响。
    Menu.setApplicationMenu(null)
    // 主窗口创建前完成渲染缓存清理（版本变化时），避免加载到旧资源。
    await rendererCachePruned
    // 已固化的服务端证书指纹必须在任何请求发出前就绪（否则首个自签证书请求会被拒）。
    await trustedCertificatesLoaded
    // 若本进程是"接管旧实例"后重启的，先继承旧安装目录，供代理清理陈旧进程。
    applyInheritedStaleRoots()
    // 版本号用 resolveBundledVersion 解析，不直接用 app.getVersion()：后者在 app.asar 里
    // 读不到 version 时会回退成 Electron 的大版本号（33.x），导致"新版本永远不比当前新"。
    //
    // 本体更新的启动检查**不在这里**：它由窗口里的启动期更新门完成（见 createWindow），
    // 那样才能在"全屏启动页上等更新装完"，而不是让界面先跑起来、再被更新重启掉。
    bundledVersion = await resolveBundledVersion(app.getAppPath(), app.getVersion())
    registerIpc()
    /** 上一次"代理就绪后补查本体更新"的时间（节流用）。 */
    let lastHandoffCheckAt = 0
    agent.onChanged((info) => {
      broadcastAgentInfo()
      // 整包更新后代理会以新版本重新起来：此时重新加载界面（会重新解析热更层），
      // 让前端与代理都回到同一版本，用户不需要手动重启。
      if (info.available && updateAppliedAt > 0 && Date.now() - updateAppliedAt < UPDATE_RELOAD_WINDOW_MS) {
        updateAppliedAt = 0
        console.info('[vault] 代理已更新并就绪，重新加载界面')
        // loadRendererLayer 内部在加载热更层前会先盖过渡页，这里直接调用即可。
        void loadRendererLayer()
      }
      // 代理就绪后补一次本体切换检查。
      //
      // 启动那一刻 current.json 往往还不存在（代理正在后台下载/解压新本体），
      // 只靠 whenReady 里那一次检查会永远错过；运行期的 watch 又是异步的，
      // 一旦它的轮询链被代理重启打断，包就绪后也再没人切换。
      // 这里以"代理可用"为信号补一次，代价只有一次本地文件读。
      if (
        !startupUpdateGateRunning &&
        info.available &&
        Date.now() - lastHandoffCheckAt > CLIENT_HANDOFF_RECHECK_MS
      ) {
        lastHandoffCheckAt = Date.now()
        void maybeHandoffOnStartup(agent, bundledVersion, { onSwitching: handleSwitching })
        // 代理刚就绪/刚重连成功：这是"能不能更新"答案发生变化的时刻，立刻补一轮
        // 请求准备，不要等 watch 的退避定时器（那会让更新看起来根本没开始）。
        clientUpdateWatch?.nudge()
      }
    })
    // 代理必须先于窗口起来：启动期更新门要向它确认"有没有新版本、能不能装"，
    // 而那道门在 createWindow 里、加载界面之前。找不到可执行文件时进入降级模式，不影响窗口。
    agent.start()
    await createWindow()
    createTray()
    // 客户端本体更新：请求代理准备新版本（下载/校验/解压），就绪且无活跃挂载时切换。
    // 有挂载时绝不重启——重启会掐断 iSCSI 会话（见 docs/implementation.md §7.4.4）。
    clientUpdateWatch = startClientUpdateWatch(agent, bundledVersion, {
      onSwitching: handleSwitching
    })
    // 开机自启：等代理就绪后把系统自启项对齐到配置里的开关。这一步同时负责修复
    // "本体更新换了程序目录，自启项还指着已清理的旧 exe"（见 autostart.ts 的文件头）。
    void syncAutoStartFromAgent()
    app.on('activate', () => {
      if (BrowserWindow.getAllWindows().length === 0) void createWindow()
      else showWindow()
    })
  })

  // 关闭窗口时保留托盘（真正退出走托盘菜单或系统退出）。
  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin' && quitting) app.quit()
  })

  // 应用退出时停止守护，避免代理成为孤儿进程。
  app.on('will-quit', () => {
    clientUpdateWatch?.stop()
    clientUpdateWatch = null
    agent.stop()
  })
}

const gotLock = app.requestSingleInstanceLock()
if (gotLock) {
  startPrimaryInstance()
} else {
  void (async () => {
    const outcome = await takeoverOlderInstance()
    if (outcome.kind === 'takeover') {
      // 旧实例已结束：重启自身一次，让新进程干净地取得单实例锁。
      // 本进程已提权（requireAdministrator），创建同样要求的子进程不会再弹 UAC。
      process.env[TAKEOVER_DONE_ENV] = '1'
      app.relaunch()
      app.exit(0)
      return
    }
    if (outcome.kind === 'blocked') {
      // 本体更新拉起的进程只是"顺带试试"：接管不了就静默退出，旧实例继续正常使用。
      // 这里弹"已有一个客户端在运行"只会让用户莫名其妙（他并没有手动开第二个）。
      if (process.env.VAULT_CLIENT_HANDOFF === '1') {
        app.exit(0)
        return
      }
      await app.whenReady()
      dialog.showMessageBoxSync({
        type: 'warning',
        title: 'Vault',
        message: '已有一个 Vault 客户端在运行',
        detail:
          '无法自动接管运行中的实例（它的版本更高，或读不到版本信息）。\n\n' +
          `路径：${outcome.path}\n\n` +
          '请先在任务管理器里结束这些 Vault 进程，再启动本客户端。',
        buttons: ['确定'],
        noLink: true
      })
    }
    app.quit()
  })()
}
