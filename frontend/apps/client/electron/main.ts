import crypto from 'node:crypto'
import { promises as fs } from 'node:fs'
import path from 'node:path'
import { BrowserWindow, Menu, Tray, app, dialog, ipcMain, nativeImage, shell } from 'electron'

import { AgentManager, listProcesses, runPowerShell, type AgentRequestOptions, type NamedProcess } from './agent'
import { syncAutoStart } from './autostart'
import {
  awaitStartupBootstrap,
  logClientUpdate,
  type StartupUpdateOutcome,
  type StartupUpdateStatus
} from './clientUpdate'
import { agentDataDir, resolveBundledVersion, resolveWebappEntry } from './webapp'

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
/**
 * 是否已进入"正常退出"流程（见 quitGracefully）。
 *
 * 比 quitting 更早置位：从用户点退出/关闭的那一刻起，窗口就不该再被托盘点击、
 * 第二个实例或 activate 事件叫回来。
 */
let quittingStarted = false
/**
 * 退出时最多等代理卸载挂载多久（毫秒）。
 *
 * 窗口在退出流程的第一步就隐藏了，这个等待不拖慢用户感知的关闭速度；给够时间只是
 * 为了让"退出时清理挂载"真正跑完（代理卸载是逐条 PowerShell，多库时会花几秒）。
 * 超时也照常退出，代理在后台继续收尾（见 AgentManager.shutdownAndWait）。
 */
const QUIT_UNMOUNT_WAIT_MS = 8000
/**
 * "启动即隐藏"的环境变量。
 *
 * 只在**本体更新**这条路上用：切换器（Vault-Updator.exe / 代理侧切换器）拉起新版本时
 * 命令行参数到不了新进程，由本进程通过环境变量把"这次是后台启动"传下去
 * （见 clientUpdate.ts 的 launchUpdator），否则自动更新一换版本，登录后又弹出窗口。
 */
const CLIENT_HIDDEN_ENV = 'VAULT_CLIENT_HIDDEN'

/**
 * 由更新器设置的"本次启动不要再检查更新"标记。
 *
 * 更新器刚完成检查/解压并启动新版本；新版本如果立刻又检查一次，可能与仍在等待它加载的
 * 旧更新器互相干扰（新版退出 → 旧更新器误判失败 → 回退旧版本，同时另一个更新器又拉起
 * 新版）。下一次用户正常启动时没有这个变量，会照常检查。
 */
const SKIP_UPDATE_ENV = 'VAULT_CLIENT_SKIP_UPDATE'

/** 自启项在命令行上传入的参数（与 autostart.ts 的 AUTOSTART_HIDDEN_ARG 一致）。 */
const CLIENT_HIDDEN_ARG = '--hidden'

/**
 * 这一次启动要不要只留托盘（不把窗口摆到桌面上）。
 *
 * **只有开机自启那一次**该藏起来：登录后客户端在后台完成自动登录与自动挂载，右下角
 * 托盘图标就够用。用户自己双击启动**必须出窗口** —— 区别只在命令行参数上，
 * 不看配置、不留持久状态。
 *
 * ⚠️ 这里曾经写成"默认隐藏，只有带 `--window` 才显示"，结果正常双击启动也不出窗口，
 * 用户看到的就是"点了图标什么都不发生"（真实反馈："正常打开的还是要出来窗口啊"）。
 * 判定方向必须是**白名单**：只有明确标记了"我是自启拉起来的"才隐藏。
 *
 * 例外/兜底：
 *   - 开发态（VITE_DEV_SERVER_URL）：调试时不希望每次都被藏起来；
 *   - 显式带 `--window` / `--show`：优先于 `--hidden`，排障时的逃生口；
 *   - 托盘创建失败（见 createTray）：没有托盘图标就没有任何入口，必须显示窗口；
 *   - 本体更新换版本后的新实例：靠 CLIENT_HIDDEN_ENV 继承（更新期间窗口本来就被藏了）。
 */
function wantsHiddenStart(argv: readonly string[], env: NodeJS.ProcessEnv): boolean {
  if (argv.some((arg) => arg === '--window' || arg === '--show')) return false
  if (argv.some((arg) => arg === CLIENT_HIDDEN_ARG)) return true
  return env[CLIENT_HIDDEN_ENV] === '1'
}

let startHidden = !DEV_SERVER_URL && wantsHiddenStart(process.argv, process.env)

// 把"这次是后台启动"写回环境变量：本机代理（agent.ts 的 spawn 没传 env）等子进程靠继承，
// 它们再拉起客户端（切换器 / 代理侧切换器）时不至于把这个语义弄丢。只写不删：值为 '1' 才是
// 隐藏，其余一律显示，残留的 '0' 无害。
if (startHidden) process.env[CLIENT_HIDDEN_ENV] = '1'

/**
 * 本次启动是否由更新器刚刚拉起。
 *
 * 为 true 时：跳过重复的更新检查，并**强制加载随包内置的渲染层**（不采用 %ProgramData%
 * 下的热更层）。更新器已经用整包保证前后端一致，这里再叠加一层历史热更资源只会让
 * "界面版本"与"程序目录版本"重新分叉。
 */
const launchedByUpdater = process.env[SKIP_UPDATE_ENV] === '1'
if (launchedByUpdater) delete process.env[SKIP_UPDATE_ENV]
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
 * 启动期更新门是否已经跑过。
 *
 * 只在**首次建窗**时等更新：窗口关掉再从托盘打开、或热更重载界面时都不该再让用户等一次。
 */
let startupUpdateGateDone = false

/** 本地代理守护器（单例）。 */
const agent = new AgentManager()
/**
 * 是否已经拉起过本地代理。
 *
 * createWindow 可能在"关窗到托盘 → 再打开"时被再次调用；AgentManager.start() 不是幂等的
 * （会再 spawn 一个进程，和现有代理抢 Global\VaultAgent）。因此只允许真正的首次启动拉起。
 */
let agentStarted = false

/** 拉起本地代理（仅首次生效）。 */
function ensureAgentStarted(): void {
  if (agentStarted) return
  agentStarted = true
  agent.start()
}

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

/**
 * 把桌面 / 开始菜单 / 任务栏里指向旧版 Vault.exe 的快捷方式改到**当前版本**。
 *
 * 为什么必须做：整包更新会把程序换到 `%ProgramData%\Vault\client\<版本>\`，但用户桌面上的
 * `Vault.lnk` 仍指向最初的 `C:\Program Files\vault-client\Vault.exe`。用户每次从旧快捷方式
 * 启动，都要先跑旧版本、再由更新器切到新版本 —— 表现就是"每次打开都闪几下才到新版本"
 * （真实反馈）。这里一次性把快捷方式校准到当前 exe，之后启动直达最新版。
 *
 * 只改**文件名是 Vault.exe、且路径看起来属于本客户端**的快捷方式，避免误改同名的其它程序。
 */
async function repairClientShortcuts(): Promise<void> {
  if (process.platform !== 'win32') return
  const currentExe = app.getPath('exe')
  const currentDir = path.dirname(currentExe)
  const psLiteral = (value: string): string => `'${value.replace(/'/g, "''")}'`
  const script = [
    `$ErrorActionPreference='SilentlyContinue'`,
    `$current=${psLiteral(currentExe)}`,
    `$cwd=${psLiteral(currentDir)}`,
    `$roots=@([Environment]::GetFolderPath('Desktop'),[Environment]::GetFolderPath('CommonDesktopDirectory'),(Join-Path $env:APPDATA 'Microsoft\\Windows\\Start Menu\\Programs'),(Join-Path $env:ProgramData 'Microsoft\\Windows\\Start Menu\\Programs'),(Join-Path $env:APPDATA 'Microsoft\\Internet Explorer\\Quick Launch\\User Pinned\\TaskBar'))`,
    `$roots=$roots | Where-Object { $_ -and (Test-Path -LiteralPath $_) }`,
    `$sh=New-Object -ComObject WScript.Shell`,
    `$n=0`,
    `foreach($root in $roots){`,
    `  Get-ChildItem -LiteralPath $root -Filter *.lnk -Recurse -ErrorAction SilentlyContinue | ForEach-Object {`,
    `    try {`,
    `      $l=$sh.CreateShortcut($_.FullName)`,
    `      $t=[string]$l.TargetPath`,
    `      if([string]::IsNullOrWhiteSpace($t)){ return }`,
    `      if([IO.Path]::GetFileName($t) -ne 'Vault.exe'){ return }`,
    `      if([string]::Equals($t,$current,[StringComparison]::OrdinalIgnoreCase)){ return }`,
    `      $lower=$t.ToLowerInvariant()`,
    `      $prod=''`,
    `      try { $prod=[string](Get-Item -LiteralPath $t -ErrorAction SilentlyContinue).VersionInfo.ProductName } catch { }`,
    `      if(-not (($prod -eq 'Vault') -or $lower.Contains('vault-client') -or $lower.Contains('\\vault\\client\\'))){ return }`,
    `      $l.TargetPath=$current; $l.WorkingDirectory=$cwd; $l.Save(); $n++`,
    `    } catch { return }`,
    `  }`,
    `}`,
    `$n`
  ].join('; ')
  try {
    const out = (await runPowerShell(script, 20000)).trim()
    const updated = Number.parseInt(out, 10)
    if (Number.isFinite(updated) && updated > 0) {
      console.info(`[vault] 已校准 ${updated} 个客户端快捷方式 → ${currentExe}`)
    }
  } catch (error) {
    // 快捷方式修不修不影响本次运行：下一次启动还会再试。
    console.warn(`[vault] 校准客户端快捷方式失败：${error instanceof Error ? error.message : String(error)}`)
  }
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
 * 通知更新器"新版本界面已经加载完成"。
 *
 * 更新器启动新版本后会等这份凭据：只看到进程存活不够（可能起来了但白屏），
 * 没有凭据就回退旧版本（见 internal/agent/bootstrap.go 的 waitForBootOK）。
 */
async function markBootOK(version: string): Promise<void> {
  try {
    const dir = path.join(agentDataDir(), 'update')
    await fs.mkdir(dir, { recursive: true })
    await fs.writeFile(
      path.join(dir, 'boot-ok.json'),
      JSON.stringify({ version, at: Date.now() }),
      'utf8'
    )
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    console.warn(`[vault] 写入启动成功凭据失败（不影响本进程）：${reason}`)
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
    const result = await agent.request(parsed)
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
    await quitGracefully()
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

/**
 * 正常退出的唯一入口：窗口立刻消失 → 请代理卸载全部挂载 → 退出应用。
 *
 * 为什么不能直接 app.quit()：will-quit 会把代理子进程 stop() 掉，而代理的卸载是在
 * `/agent/shutdown` 返回之后才在后台进行的（见 cmd/vault-agent/main.go）——不等它，
 * "退出时清理挂载"就被砍在半路（大多数情况下连第一块盘都清不完）。
 *
 * 关闭速度：窗口在第一步就隐藏，用户感知的"关掉了"只取决于这一步；卸载在后台跑，
 * 最多等 QUIT_UNMOUNT_WAIT_MS，等不到就照常退出，由代理自己把剩下的收尾做完。
 */
async function quitGracefully(): Promise<void> {
  if (quittingStarted) return
  quittingStarted = true
  try {
    // 先隐藏：用户看不到窗口被拆掉，也就没有"退出得慢"的观感。
    mainWindow?.hide()
  } catch {
    // 窗口可能已销毁：退出流程不能因此中断。
  }
  try {
    // 托盘图标也一并收掉：进程还要在后台停留最多 QUIT_UNMOUNT_WAIT_MS 用于卸载挂载，
    // 图标留着会让用户以为"点了退出却没退"。
    tray?.destroy()
  } catch {
    // 无托盘环境（见 createTray）：忽略。
  }
  tray = null
  quitting = true
  try {
    await agent.shutdownAndWait(QUIT_UNMOUNT_WAIT_MS)
  } finally {
    app.quit()
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
    await quitGracefully()
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
      sandbox: true,
      /*
       * 把**正在运行的客户端本体版本**交给渲染层（见 preload.ts 的 appVersion）。
       *
       * 渲染层自己打包时也会带一份 __APP_VERSION__，但那是"构建这份界面时的版本"：
       * 渲染层资源热更（client_web）可以在**不重启本体**的前提下把界面换成更新的版本
       * （见 resolveWebappEntry），于是界面里的 __APP_VERSION__ 会比安装目录里的
       * Vault.exe / Vault-Agent.exe 高一个版本。关于页再用它当"内部版本"，就会显示成
       * "客户端已更新、只有本地服务还是旧的"——真实工单即由此而来。
       *
       * additionalArguments 会原样进入 preload 的 process.argv，随窗口创建固定下来；
       * 版本号在进程生命周期内不会变，不需要额外 IPC。
       */
      additionalArguments: [`--vault-app-version=${encodeURIComponent(bundledVersion || app.getVersion())}`],
      // 窗口可能长时间藏在托盘里：不能让 Chromium 把隐藏窗口的定时器节流掉，
      // 否则渲染层的会话保活 / 挂载状态刷新会变成"一分钟才动一次"。
      backgroundThrottling: false
    }
  })

  // 自启那一次隐藏（见 startHidden）：界面照样加载，只是不显示 —— 自动登录、会话推送、
  // 自动挂载都发生在渲染层与本地代理里，不需要窗口可见；用户双击启动则照常显示。
  mainWindow.once('ready-to-show', () => {
    if (!startHidden) mainWindow?.show()
  })

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
    // 开发态没有打包目录里的独立更新器，也不应该在每次热重载时检查更新；
    // 但本地服务仍要起来（界面所有功能都依赖它）。
    ensureAgentStarted()
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
  if (!startupUpdateGateDone && launchedByUpdater) {
    startupUpdateGateDone = true
    // 这次是更新器刚拉起的新版本：它已经确认过没有更新，直接进界面。
    logClientUpdate('本次启动由更新器接管，跳过重复的启动期更新检查')
  }

  if (!startupUpdateGateDone) {
    startupUpdateGateDone = true
    await showLayerLoading()
    setBootStatus('checking')
    let outcome: StartupUpdateOutcome = 'none'
    try {
      outcome = await awaitStartupBootstrap(bundledVersion, {
        onStatus: setBootStatus,
        onSwitching: handleSwitching
      })
    } catch (error) {
      // 更新门本身出问题绝不能把用户锁在启动页上：记日志，照常进界面。
      const reason = error instanceof Error ? error.message : String(error)
      logClientUpdate(`启动期更新门异常，直接进入界面：${reason}`)
    }
    if (outcome === 'switched') {
      // 正常路径下 onSwitching 里已经 app.exit 了，走不到这里；保留是为了兜底
      // （onSwitching 未生效时，也要把"隐窗 → 停代理 → 退出"补完，不能停在过渡页不动）。
      logClientUpdate('启动期已发起版本更新切换，等待更新器启动新版本')
      await handleSwitching()
      return
    }
    logClientUpdate(`启动期更新门结束（${outcome}），启动本地服务并加载界面`)
  }

  // ★ 顺序契约：**本地服务必须晚于更新检查**。
  // 更新器此时已经确认"没有可安装的版本"（或不可用），当前程序目录就是最终版本；
  // 从这里开始启动 Vault-Agent，前端与本地服务必然同版本，不会再出现更新中途的版本错位。
  ensureAgentStarted()
  await loadRendererLayer()
  // 界面已成功加载：告诉（可能还在等待的）更新器"新版本真的起来了"。
  // 正常手动启动时没有更新器在等，这份凭据只是覆盖写入，无害。
  await markBootOK(bundledVersion)
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
  if (launchedByUpdater) {
    // 刚完成整包更新：以随包内置的界面为准，忽略之前遗留的渲染层热更资源。
    await mainWindow.loadFile(BUNDLED_RENDERER)
    loadedRendererFile = BUNDLED_RENDERER
    loadedLayerActivatedAt = 0
    console.info(`[vault] 更新后加载内置渲染层（版本 ${bundledVersion}）`)
    return
  }
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
            void quitGracefully()
          }
        }
      ])
    )
    tray.on('click', () => showWindow())
  } catch {
    // 无图形环境（如 CI）时静默跳过托盘，不影响窗口本身。
    tray = null
    // 没有托盘 = 用户没有任何入口把隐藏的窗口叫出来：必须显示窗口。
    // 窗口可能还没建好（createTray 先于 createWindow）：只撤销"隐藏启动"，
    // 由 ready-to-show 正常把窗口显示出来。
    startHidden = false
    if (mainWindow) showWindow()
  }
}

function showWindow(): void {
  // 已经进入退出流程：窗口正在/已经被收起，托盘点击与第二个实例都不该再把它叫出来。
  if (quittingStarted) return
  // 用户主动要窗口：此后的重建（关闭 → 托盘 → 再打开）都照常显示。
  startHidden = false
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
  // 用户又点了一次图标（或计划任务的第二条触发）→ 把窗口叫出来。
  //
  // 但**自启拉起的那一次**不算"用户要窗口"：登录时若已有实例在跑（例如 Run 项与计划
  // 任务同时触发、或本体更新刚换完版本），第二条命令同样带 `--hidden`，不能因此弹窗。
  app.on('second-instance', (_event, argv) => {
    if (wantsHiddenStart(argv, {})) return
    showWindow()
  })
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
    // ★ 本地代理**不在**这里启动：它必须等 createWindow 里的启动期更新门确认
    // "没有可安装的新版本"之后才启动（见 createWindow 末尾）。这是版本更新与本地服务
    // 解耦的关键——更新过程中不会有旧代理占着 Global\VaultAgent，也不会有旧代理被误认成
    // 新版本的一部分。
    agent.onChanged(() => broadcastAgentInfo())
    // 托盘先于窗口创建：自启那一次是**隐藏**启动的（见 startHidden），而启动期更新门可能
    // 让 createWindow 停留数秒 —— 托盘图标这时是用户唯一能看到的"客户端已经起来了"。
    createTray()
    await createWindow()
    if (quitting) return
    // 把用户桌面/开始菜单/任务栏里指向旧目录的快捷方式校准到当前版本：
    // 否则用户每次从旧快捷方式启动，都要先跑旧版再切换（表现为"闪几下"）。
    void repairClientShortcuts()
    // 开机自启：等代理就绪后把系统自启项对齐到配置里的开关。这一步同时负责修复
    // "版本更新换了程序目录，自启项还指着已清理的旧 exe"（见 autostart.ts 的文件头）。
    void syncAutoStartFromAgent()
    app.on('activate', () => showWindow())
  })

  // 关闭窗口时保留托盘（真正退出走托盘菜单或系统退出）。
  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin' && quitting) app.quit()
  })

  // 拦截**所有正常退出**（托盘退出 / 关闭对话框 / 系统注销 / 代码里的 app.quit）：
  // 第一次先拦下来做挂载清理，清理完 quitGracefully 里的 app.quit 再进来时才放行。
  // app.exit()（本体更新切换器）刻意不经过这里：那条路不卸载挂载，由重启后的自动挂载恢复。
  app.on('before-quit', (event) => {
    if (quittingStarted) return
    event.preventDefault()
    void quitGracefully()
  })

  // 应用退出时停止守护，避免代理成为孤儿进程。
  app.on('will-quit', () => {
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
