/**
 * 客户端本体（Electron 程序目录）自动更新。
 *
 * 与渲染层热更（webapp.ts）是两条独立通道：
 *   - 渲染层热更只替换静态资源，不重启进程；
 *   - 本体更新替换**整个程序目录**（Vault.exe + resources/app.asar + Chromium 运行时），
 *     必须重启进程才能生效。
 *
 * 职责划分（与 internal/agent/updateclient.go 的冻结契约）：
 *   - 代理负责"下载 → 校验 → 解压 → 写激活指针"，落在
 *     `<ProgramData>\Vault\client\<版本>\` 与 `<client>\current.json`；
 *   - 本模块负责"什么时候切换"：启动时检查指针，运行期轮询代理状态；
 *   - **真正完成切换的是独立进程**（客户端目录里的 Vault-Updator.exe，见 cmd/vault-updator；
 *     它缺失时退回代理侧切换器，见 internal/agent/clientswitch.go）：本模块只负责拉起它，
 *     然后由主进程隐窗 → 停代理 → app.exit；updator 等本进程退出后启动新版本
 *     （失败则回退旧版本）。
 *     不再走"新旧进程抢单实例锁 + SIGKILL + relaunch"那条路 —— 那条路会闪屏、
 *     留下孤儿代理（界面一片 network.error），接管失败时还会静默退出。
 */

import { app } from 'electron'
import { spawn } from 'node:child_process'
import fs from 'node:fs/promises'
import path from 'node:path'

import { agentDataDir } from './webapp'

/** 本体根目录（与 Go 侧 clientRootDir() 一致）。 */
export function clientRootDir(): string {
  return path.join(agentDataDir(), 'client')
}

/** 激活指针 current.json 中我们关心的字段。 */
interface ClientPointer {
  version?: unknown
  dir?: unknown
}

/** 上一次切换尝试的记录（防重复拉起的节流依据）。 */
interface HandoffRecord {
  version?: unknown
  at?: unknown
}

/** 版本目录名只允许安全字符（与代理侧 / webapp.ts 的约束一致，防路径穿越）。 */
const SAFE_DIR_NAME = /^[A-Za-z0-9._-]+$/

/**
 * 同一版本两次切换请求之间的最短间隔。
 *
 * 切换器最多等 120 秒旧客户端退出；超过这个时间说明这一轮没成交（客户端没退），
 * 必须允许重新发起，否则用户看到的就是"包都就绪了却永远不更新"。这里只挡住
 * "几秒内重复拉起"，不再挡 2 分钟 —— 用户要的是就绪即装。
 */
const HANDOFF_DEDUPE_MS = 30 * 1000
/** 运行期查询代理状态的间隔。就绪判定在客户端本地读盘，2 秒一次代价可以忽略。 */
const POLL_INTERVAL_MS = 2 * 1000
/** 有新版本就绪但仍有活跃挂载时的复查间隔（挂载期间绝不重启）。 */
const MOUNT_RECHECK_MS = 15 * 1000
/**
 * 一轮比对完成后，隔多久重新向服务端比对一次。
 *
 * 不能是 30 分钟：现场的真实时序是"客户端 22:38 判 not_newer，服务端 22:48 发布
 * 新包"，而 30 分钟的退避让自动链路看起来完全没工作 —— 用户只能点进设置页手动
 * 触发。自动更新必须自己跟上发布节奏，30 秒一轮足够快，也不至于把服务端刷爆。
 */
const APPLY_RETRY_MS = 30 * 1000
/**
 * 代理/会话暂时不可用（未登录、会话建立中、服务端不可达）时的重试间隔。
 *
 * 这些状态通常几秒内自愈，用主退避会白白拖慢一轮。
 */
const APPLY_RETRY_FAST_MS = 5 * 1000

/**
 * 启动期更新门：只等"有没有更新"的结论的时间。
 *
 * 服务端不可达 / 代理还没起来时不能把用户按在启动页上：拿不到结论就先放行进界面，
 * 后台的 watch 会继续把这一轮跑完（见 startClientUpdateWatch）。
 */
const STARTUP_CHECK_GRACE_MS = 10 * 1000
/**
 * 启动期等代理就绪的上限。
 *
 * 代理是异步拉起的，正常 1~2 秒就可用；超过这个时间还没起来（降级模式、程序缺失、
 * 或正在被更新器替换），就不要再把用户按在启动页上 —— 直接放行进界面，watch 会接着跟。
 */
const STARTUP_AGENT_WAIT_MS = 5 * 1000
/**
 * 启动期更新门：已确认在下载/校验/解压时额外给的时间。
 *
 * 勾了自动更新就应当等它装完再进界面 —— 否则用户刚登录就看到"版本不一致，建议重启"
 * 或者界面被更新流程整个重启掉。超出这个时间同样先放行，避免网络慢时永远进不去。
 */
const STARTUP_PREPARE_BUDGET_MS = 3 * 60 * 1000
/** 启动期轮询间隔（比运行期密：这段时间用户正对着启动页等）。 */
const STARTUP_POLL_MS = 500

/** 本模块需要的代理能力（避免直接依赖 AgentManager 的具体实现）。 */
export interface ClientUpdateAgent {
  request(options: {
    method: string
    path: string
    body?: unknown
    timeoutMs?: number
  }): Promise<{ status: number; payload?: unknown }>
  activeMountCount(): Promise<number>
}

/** 代理返回的客户端本体更新状态（见 updateclient.go 的 ClientUpdateState）。 */
interface ClientUpdateState {
  state?: unknown
  ready_version?: unknown
  error?: unknown
  received_bytes?: unknown
  total_bytes?: unknown
}

function handoffRecordPath(): string {
  return path.join(app.getPath('appData'), 'Vault', 'client-handoff.json')
}

async function readJson(file: string): Promise<unknown> {
  try {
    return JSON.parse(await fs.readFile(file, 'utf8')) as unknown
  } catch {
    return null
  }
}

async function writeJson(file: string, value: unknown): Promise<void> {
  try {
    await fs.mkdir(path.dirname(file), { recursive: true })
    await fs.writeFile(file, JSON.stringify(value, null, 2), 'utf8')
  } catch {
    // 记录写不进去只影响节流，不影响更新本身：忽略。
  }
}

/**
 * 关键事件落盘到 `<ProgramData>\Vault\logs\client.log`。
 *
 * 打包后的主进程 console 完全不可见。这次"安装包已经就绪、客户端却永远不切换"
 * 在机器上没留下任何痕迹，只能靠日志定位 —— 所以把状态迁移记下来。
 */
export function logClientUpdate(message: string): void {
  const line = `${new Date().toISOString()} ${message}\n`
  console.info(`[vault] ${message}`)
  const logsDir = path.join(agentDataDir(), 'logs')
  void fs
    .mkdir(logsDir, { recursive: true })
    .then(() => fs.appendFile(path.join(logsDir, 'client.log'), line, 'utf8'))
    .catch(() => undefined)
}

/** 把代理返回的 error 字段变成可读文本。 */
function describeState(error: unknown): string {
  return typeof error === 'string' && error.trim() !== '' ? error.trim() : '无错误信息'
}

/** 语义化版本比较（`a > b`）。任一方不可解析时返回 false（保守：不切换）。 */
export function isNewerVersion(a: string, b: string): boolean {
  const parse = (value: string): number[] | null => {
    const m = /^(\d+)\.(\d+)\.(\d+)/.exec(value.trim())
    if (!m) return null
    return [Number(m[1]), Number(m[2]), Number(m[3])]
  }
  const va = parse(a)
  const vb = parse(b)
  if (!va || !vb) return false
  for (let i = 0; i < 3; i += 1) {
    if (va[i] !== vb[i]) return va[i] > vb[i]
  }
  return false
}

/**
 * 目录是否是**完整可运行**的客户端程序目录。
 *
 * 判据与代理侧 isCompleteClientDir / build.py 的 is_complete_app_dir 一致：
 * 必备文件 + 最小文件数。少一个就判不合格 —— 把一个跑不起来的目录交给进程，
 * 用户重启后就没有界面了。
 */
async function isCompleteClientDir(dir: string): Promise<boolean> {
  try {
    const [exe, asar] = await Promise.all([
      fs.stat(path.join(dir, 'Vault.exe')),
      fs.stat(path.join(dir, 'resources', 'app.asar'))
    ])
    if (!exe.isFile() || !asar.isFile()) return false
  } catch {
    return false
  }
  return true
}

/**
 * 读取激活指针，返回"可以切换过去"的新版本目录；没有则 null。
 *
 * 只接受**严格新于当前版本**的指针：否则刚切过去的新版本自己会读到自己，
 * 又拉着自己重启，形成无限循环。
 */
async function resolveReadyClient(currentVersion: string): Promise<{ version: string; exe: string } | null> {
  const pointer = (await readJson(path.join(clientRootDir(), 'current.json'))) as ClientPointer | null
  const version = typeof pointer?.version === 'string' ? pointer.version.trim() : ''
  if (version === '' || !SAFE_DIR_NAME.test(version) || version.includes('..')) return null
  if (!isNewerVersion(version, currentVersion)) return null
  const dir = path.resolve(clientRootDir(), version)
  // 双保险：解析后的目录必须仍在 client 根目录的下一层。
  if (path.dirname(dir) !== path.resolve(clientRootDir())) return null
  if (!(await isCompleteClientDir(dir))) return null
  return { version, exe: path.join(dir, 'Vault.exe') }
}

/** 独立 updator 的路径：与 Vault.exe 同级（electron-builder 的 extraFiles 放的）。 */
function updatorExePath(): string {
  return path.join(path.dirname(app.getPath('exe')), 'Vault-Updator.exe')
}

/**
 * 拉起独立 updator 进程；返回 false 表示该文件不可用（调用方走代理侧兜底）。
 *
 * 它必须是**独立进程**：Windows 不允许替换正在运行的 exe，本进程退出后也没有代码
 * 能再启动新版本 —— 这两件事只能由第三个进程完成。updator 只做"等本进程退出 →
 * 启动新版本 → 失败回退"，与客户端版本无关，所以旧版本目录里带的那份同样能用。
 */
async function launchUpdator(ready: { version: string; exe: string }): Promise<boolean> {
  const exe = updatorExePath()
  try {
    await fs.stat(exe)
  } catch {
    // 老包里没有这个文件（或被杀软删掉）：交给代理侧兜底，不算错误。
    logClientUpdate('客户端目录中没有 Vault-Updator.exe，改用代理侧切换器')
    return false
  }

  const args = [
    '-pid', String(process.pid),
    '-target', ready.exe,
    '-version', ready.version,
    '-timeout', '120s',
    // 回退目标必须是"当前正在运行的 exe"：新版起不来时把用户捞回来。
    '-fallback', app.getPath('exe')
  ]
  try {
    const child = spawn(exe, args, {
      cwd: path.dirname(exe),
      detached: true,
      stdio: 'ignore',
      windowsHide: true
    })
    // 不等待：updator 必须独立于本进程存活，本进程随后就会退出。
    child.unref()
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    logClientUpdate(`拉起独立 updator 失败（改用代理侧切换器）：${reason}`)
    return false
  }
  logClientUpdate(`已拉起独立 updator ${exe}：等待本进程退出后启动 ${ready.version}`)
  return true
}

/**
 * 请求代理拉起**独立的切换器进程**来完成本体切换。
 *
 * 为什么不再由本进程 spawn 新客户端：老做法是新旧进程抢单实例锁，新进程接管后 SIGKILL
 * 旧进程、再 `app.relaunch()` 重启自己。那条路上：旧窗口来不及隐藏就消失（闪屏）、
 * 新进程又重启一次（第二段闪屏）、旧代理成为孤儿占着 `Global\VaultAgent`（界面一片
 * network.error，只能手动点"重启服务"）、接管失败时新进程静默退出（看起来"根本没更新"）。
 *
 * 现在交给切换器（代理 exe 的临时副本，见 internal/agent/clientswitch.go）：
 *
 *	本进程：请求切换 → 隐藏窗口 → 让代理退出 → 立即退出（app.exit）
 *	切换器：等本进程退出 → 启动新版本 exe（失败则回退旧版本）→ 自己退出
 *
 * 首选路径是客户端目录里的独立 exe（`Vault-Updator.exe`，见 cmd/vault-updator）；
 * 该文件缺失时退回"请求代理拉起切换器副本"的兜底路径（见 internal/agent/clientswitch.go）。
 *
 * 两条路径的时序完全一致：
 *
 *	本进程：拉起切换器 → 隐藏窗口 → 让代理退出 → 立即退出（app.exit）
 *	切换器：等本进程退出 → 启动新版本 exe（失败则回退旧版本）→ 自己退出
 *
 * 切换器拿到的是当前正在运行的程序作为回退目标，新版本起不来时用户仍有界面。
 */
async function requestSwitch(
  agent: ClientUpdateAgent,
  ready: { version: string; exe: string }
): Promise<boolean> {
  const version = ready.version
  /*
   * 上一轮新版本启动失败、切换器回退了旧版本时，会通过环境变量告知失败版本。
   * 必须跳过：否则旧版本启动 → 发现新版就绪 → 切换 → 新版又起不来 → 再回退，
   * 用户看到的就是窗口无限循环重启。换到下一个版本号后这个标记自然失效。
   */
  const blocked = (process.env.VAULT_CLIENT_UPDATE_BLOCKED ?? '').trim()
  if (blocked !== '' && blocked === version) {
    logClientUpdate(`版本 ${version} 上次启动失败已被回退，本轮不再自动切换`)
    return false
  }

  const record = (await readJson(handoffRecordPath())) as HandoffRecord | null
  if (record?.version === version && typeof record.at === 'number' && Date.now() - record.at < HANDOFF_DEDUPE_MS) {
    // 最近刚请求过同一版本又没退成功：先别重复拉切换器，交给上一轮的结果。
    logClientUpdate(`同一版本 ${version} 刚请求过切换，本轮跳过`)
    return false
  }

  // 首选：客户端目录里的独立 updator（Vault-Updator.exe）。
  if (await launchUpdator(ready)) {
    await writeJson(handoffRecordPath(), { version, at: Date.now() })
    return true
  }

  // 兜底：updator 缺失或启动失败时，请代理复制自身为切换器（两者逻辑完全相同）。
  try {
    const result = await agent.request({
      method: 'POST',
      path: '/agent/update/client/switch',
      body: {
        client_pid: process.pid,
        target_version: version,
        // 回退目标必须是"当前正在运行的 exe"：切换器在新版起不来时用它把用户捞回来。
        fallback_exe: app.getPath('exe')
      },
      timeoutMs: 15000
    })
    if (result.status !== 200 && result.status !== 202) {
      logClientUpdate(`请求切换器失败（HTTP ${result.status}），继续使用当前版本`)
      return false
    }
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    logClientUpdate(`请求切换器失败（继续使用当前版本）：${reason}`)
    return false
  }

  await writeJson(handoffRecordPath(), { version, at: Date.now() })
  logClientUpdate(`已请求切换器接管：${version}（切换器会在本进程退出后启动新版本）`)
  return true
}

/**
 * 启动时检查本体更新，必要时拉起新版本进程。
 *
 * 返回 true 表示切换器已接手（调用方随后应通过 onSwitching 退出本进程）。
 */
export async function maybeHandoffOnStartup(
  agent: ClientUpdateAgent,
  currentVersion: string,
  options: SwitchOptions = {}
): Promise<boolean> {
  let ready: { version: string; exe: string } | null = null
  try {
    ready = await resolveReadyClient(currentVersion)
  } catch {
    return false
  }
  if (!ready) return false
  logClientUpdate(`检测到已就绪的客户端本体更新：${ready.version}（当前 ${currentVersion}）`)
  if (!(await requestSwitch(agent, ready))) return false
  await options.onSwitching?.()
  return true
}

/** 从代理返回体里取出 client_update 状态。 */
function readClientUpdateState(payload: unknown): ClientUpdateState | null {
  if (typeof payload !== 'object' || payload === null) return null
  const value = (payload as { client_update?: unknown }).client_update
  return typeof value === 'object' && value !== null ? (value as ClientUpdateState) : null
}

/**
 * 运行期守护：请求代理准备本体更新，就绪且无活跃挂载时切换到新版本。
 *
 * 为什么必须检查挂载：本体更新要重启客户端，重启期间 iSCSI 会话必然短暂断开。
 * 有挂在用的盘时切换 = 打断用户正在做的写入（见 docs/implementation.md §7.4.4）。
 *
 * 返回守护句柄（应用退出时 stop，代理状态变化时 nudge）。
 */
export interface ClientUpdateWatch {
  /** 停掉所有定时器（应用退出时调用）。 */
  stop: () => void
  /**
   * 立刻补一次检查，不等退避定时器。
   *
   * 由代理状态变化触发（就绪 / 重连成功 / 会话建立）：这几件事一发生，"能不能更新"
   * 的答案就变了。用户要的是条件一具备就开始装，而不是等一个与自己无关的定时器。
   */
  nudge: () => void
}

export function startClientUpdateWatch(
  agent: ClientUpdateAgent,
  currentVersion: string,
  options: SwitchOptions = {}
): ClientUpdateWatch {
  let stopped = false
  let pollTimer: NodeJS.Timeout | null = null
  let applyTimer: NodeJS.Timeout | null = null
  /** 是否有 apply 请求在飞：定时器与 nudge 可能同时命中，重复请求只会白打一次服务端。 */
  let applyInFlight = false
  /** 上一次成功请求代理准备的时间；用于"只轮询不过问"时定期补一次 apply。 */
  let lastApplyAt = 0
  /** 上一次记录过的代理上报版本（避免每轮刷一条同样的日志）。 */
  let lastLoggedReady = ''

  const clearTimers = (): void => {
    if (pollTimer) clearTimeout(pollTimer)
    if (applyTimer) clearTimeout(applyTimer)
    pollTimer = null
    applyTimer = null
  }

  const stop = (): void => {
    stopped = true
    clearTimers()
  }

  const scheduleApply = (delayMs: number): void => {
    if (stopped) return
    if (applyTimer) clearTimeout(applyTimer)
    applyTimer = setTimeout(() => {
      applyTimer = null
      void requestApply()
    }, delayMs)
  }

  const requestApply = async (): Promise<void> => {
    if (stopped || applyInFlight) return
    applyInFlight = true
    try {
      let result: { status: number; payload?: unknown }
      try {
        result = await agent.request({
          method: 'POST',
          path: '/agent/update/client/apply',
          // current_version 必须是**客户端版本**：代由此判断是否真的需要更新
          // （代理自身可能已经比客户端新，拿代理版本当基准会永远判成"无需更新"）。
          body: { current_version: currentVersion },
          timeoutMs: 20000
        })
      } catch {
        // 代理还没起来（或正在重启）：几秒后重来，别把这一轮丢掉。
        scheduleApply(APPLY_RETRY_FAST_MS)
        return
      }
      if (stopped) return
      if (result.status !== 202 && result.status !== 200) {
        // 代理刚起来/正在重启：稍后再试（不要在这里放弃）。
        scheduleApply(APPLY_RETRY_FAST_MS)
        return
      }
      lastApplyAt = Date.now()
      schedulePoll(500)
    } finally {
      applyInFlight = false
    }
  }

  const schedulePoll = (delayMs: number): void => {
    if (stopped) return
    if (pollTimer) clearTimeout(pollTimer)
    pollTimer = setTimeout(() => {
      pollTimer = null
      void poll()
    }, delayMs)
  }

  const poll = async (): Promise<void> => {
    if (stopped) return
    let result: { status: number; payload?: unknown }
    try {
      result = await agent.request({ method: 'GET', path: '/agent/update/client', timeoutMs: 15000 })
    } catch {
      schedulePoll(APPLY_RETRY_FAST_MS)
      return
    }
    if (stopped) return
    if (result.status !== 200) {
      // 代理不可用（重启中）：退避后重来，别把这一轮更新丢掉。
      schedulePoll(APPLY_RETRY_FAST_MS)
      return
    }
    const state = readClientUpdateState(result.payload)
    const phase = typeof state?.state === 'string' ? state.state : ''
    const reported = typeof state?.ready_version === 'string' ? state.ready_version.trim() : ''

    // 切换判定只认**磁盘激活指针**，不看代理上报的 ready_version。
    //
    // 代理侧的内存态在进程重启后要从 current.json 重新播种（见 updateclient.go 的
    // snapshot）。播种失败、或状态与磁盘短暂不一致时，代理会一直报 ready_version 为空，
    // 而安装包其实早已下载、校验、解压完成 —— 现场就是"包就绪了，客户端永远不切换，
    // 只有用户自己点进设置页手动触发才有反应"。
    //
    // current.json 是"解压完成且校验通过"的唯一权威凭据，直接读它不会漏。
    const ready = await resolveReadyClient(currentVersion)
    if (ready) {
      // 挂载查询失败按"有挂载"处理：宁可晚一轮，也不能在正在写入的盘上重启客户端。
      const busy = await agent.activeMountCount().catch(() => 1)
      if (busy === 0) {
        logClientUpdate(`检测到已就绪的客户端 ${ready.version}（当前 ${currentVersion}），发起切换`)
        if (await requestSwitch(agent, ready)) {
          // 切换器会在本进程退出后启动新版本；这里停止轮询并交给主进程收尾。
          stop()
          await options.onSwitching?.()
          return
        }
      }
      schedulePoll(MOUNT_RECHECK_MS)
      return
    }

    if (phase === 'failed') {
      /*
       * "没有可用更新"不是终点：新版本可能就在几分钟后发布。按原因分档重试 ——
       * 会话/服务端没就绪是几秒内自愈的临时态，抓一次失败就等 30 分钟，用户看到的
       * 就是"自动更新根本不工作，非得点进设置页点一下"（现场日志里的真实时序：
       * 22:38 判 not_newer，22:48 服务端发了新包，而上一版要等 30 分钟才重新比对）。
       */
      const reason = describeState(state?.error)
      const transient = reason === 'no_session' || reason === 'server_unreachable'
      logClientUpdate(
        `客户端本体更新未成功（${reason}），${transient ? '条件具备后立即重试' : '稍后重新比对'}`
      )
      scheduleApply(transient ? APPLY_RETRY_FAST_MS : APPLY_RETRY_MS)
      return
    }

    if (reported !== '' && reported !== lastLoggedReady) {
      lastLoggedReady = reported
      logClientUpdate(`代理上报就绪版本 ${reported}，但磁盘激活指针尚不可用（继续等待）`)
    }
    // 长时间没有可切换的版本（例如代理那边一直 idle）时，定期重新请求准备，
    // 避免"只轮询、再也不过问代理"导致新发布的包永远不被下载。
    if (Date.now() - lastApplyAt > APPLY_RETRY_MS) {
      scheduleApply(0)
      return
    }
    // downloading / verifying / extracting：继续看进度。
    schedulePoll(POLL_INTERVAL_MS)
  }

  /**
   * 代理状态变化时的插队入口。
   *
   * 只在"正在等退避"时插队：已经轮到轮询 / 已在下载就不打扰，那一轮结果马上就到。
   */
  const nudge = (): void => {
    if (stopped) return
    if (applyTimer) {
      clearTimeout(applyTimer)
      applyTimer = null
      void requestApply()
      return
    }
    // 两个定时器都不在（上一轮刚收尾）：补一轮，别让"代理刚就绪"这个时间点被浪费。
    if (!pollTimer) void requestApply()
  }

  // 立刻开始第一轮，不再预留"等代理握手"的 5 秒：请求失败会按 APPLY_RETRY_FAST_MS
  // 自动重试，那是可恢复的；而空等的这几秒是用户能直接感知到的"更新没开始"。
  scheduleApply(0)

  return { stop, nudge }
}

/** 启动期更新门的结果。 */
export type StartupUpdateOutcome =
  /** 没有需要装的更新（或用户关了自动更新）：正常进界面。 */
  | 'none'
  /** 已拉起新版本进程，本进程稍后会被接管结束。 */
  | 'switched'
  /** 等超时了：先放行进界面，后台继续。 */
  | 'timeout'

/** 启动页要展示的状态（文案表在 public/loading.html 的 BOOT 里）。 */
export type StartupUpdateStatus = 'checking' | 'downloading' | 'preparing' | 'restarting'

/** 切换器接手后的收尾动作，必须由主进程实现（本模块不碰进程生命周期）。 */
export interface SwitchOptions {
  /**
   * 切换器已接手，主进程应当：隐藏窗口 → 停掉代理 → `app.exit(0)`。
   *
   * 顺序很重要：先隐藏窗口（不要让用户看到窗口被拆掉），再让代理优雅退出
   * （释放 `Global\VaultAgent`，否则新客户端起不来代理，界面会一片 network.error），
   * 最后退出自己（单实例锁释放，切换器才敢启动新版本）。
   */
  onSwitching?: () => void | Promise<void>
}

export interface StartupUpdateOptions extends SwitchOptions {
  onStatus?: (status: StartupUpdateStatus, percent?: number) => void
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/** 读设置里的「自动更新」开关；代理还没起来时就等它（最多到 deadline）。 */
async function autoUpdateEnabled(agent: ClientUpdateAgent, deadline: number): Promise<boolean> {
  for (;;) {
    try {
      const result = await agent.request({ method: 'GET', path: '/agent/config', timeoutMs: 3000 })
      if (result.status === 200) {
        // GET 返回 Config 本身（字段在顶层），PATCH 才包一层 config —— 两种都认。
        const plain = (result.payload as { auto_update?: unknown } | null)?.auto_update
        const nested = (result.payload as { config?: { auto_update?: unknown } } | null)?.config
          ?.auto_update
        const value = typeof plain === 'boolean' ? plain : nested
        // 只有显式 false 才跳过：读不到字段（老代理）时按默认开启处理。
        if (typeof value === 'boolean') return value
      }
    } catch {
      // 代理尚未就绪：稍后再问。
    }
    if (Date.now() >= deadline) return true
    await sleep(300)
  }
}

/**
 * 启动期更新门：在**全屏启动页**里把客户端本体更新装完，然后才进登录/主界面。
 *
 * 为什么需要它：原来的做法是"先让界面跑起来，运行期 watch 再找机会切换"。于是用户会
 * 先进到主界面，过一会儿窗口被整个重启（或顶栏提示"版本不一致，建议重启"）——
 * 观感就是"更新把我从页面里踢了出去"。勾了自动更新的用户期望的是：开机后停在启动页，
 * 看着它把新版本装好，然后直接进到新版本里。
 *
 * 判定仍然只认磁盘激活指针（理由同 poll）；这里额外做的是**等**：
 *  - 磁盘上已就绪 → 直接拉起新进程接管（写作 `mv` 级的瞬时操作）；
 *  - 尚未就绪 → 请求代理准备，并轮询下载/校验/解压进度，就绪后再切；
 *  - 超时/失败/有挂载 → 放行进界面，后台 watch 继续（绝不因此卡住开机）。
 */
export async function awaitStartupClientUpdate(
  agent: ClientUpdateAgent,
  currentVersion: string,
  options: StartupUpdateOptions = {}
): Promise<StartupUpdateOutcome> {
  const notify = options.onStatus ?? ((): void => undefined)

  // ① 磁盘上已经有就绪的新版本：不用过问代理，直接请切换器接手。
  //    这一条覆盖"上次已经下好、只是没来得及重启"的场景（也是现场最常见的）。
  const immediate = await resolveReadyClient(currentVersion)
  if (immediate && (await agent.activeMountCount().catch(() => 1)) === 0) {
    notify('restarting')
    if (await requestSwitch(agent, immediate)) {
      await options.onSwitching?.()
      return 'switched'
    }
  }

  // ② 用户关掉了「自动更新」：立刻放行，设置页的手动入口照旧可用。
  if (!(await autoUpdateEnabled(agent, Date.now() + STARTUP_AGENT_WAIT_MS))) return 'none'

  const startedAt = Date.now()
  let applySent = false
  /** 上一次尝试 apply 的时间：代理没就绪时别每 500ms 打一次。 */
  let lastApplyTryAt = 0
  let lastStatus: StartupUpdateStatus | null = null
  let lastPercent = -1

  const report = (status: StartupUpdateStatus, percent?: number): void => {
    if (status === lastStatus && (percent === undefined || percent === lastPercent)) return
    lastStatus = status
    if (typeof percent === 'number') lastPercent = percent
    notify(status, percent)
  }

  for (;;) {
    // 每轮都读盘：就绪判定只认 current.json（理由见 poll 的注释）。
    const ready = await resolveReadyClient(currentVersion)
    if (ready) {
      // 有挂载在忙就绝不重启（会掐断 iSCSI 写入）：放行进界面，后台 watch 择机切换。
      if ((await agent.activeMountCount().catch(() => 1)) !== 0) return 'none'
      report('restarting')
      if (await requestSwitch(agent, ready)) {
        await options.onSwitching?.()
        return 'switched'
      }
      // 请求失败（例如刚试过同一版本）：放行，别把用户留在启动页。
      return 'none'
    }

    let state: ClientUpdateState | null = null
    try {
      const result = await agent.request({ method: 'GET', path: '/agent/update/client', timeoutMs: 5000 })
      if (result.status === 200) state = readClientUpdateState(result.payload)
    } catch {
      // 代理还没就绪：下一轮再试。
    }
    const phase = typeof state?.state === 'string' ? state.state : ''

    // 代理始终没起来（读不到任何状态）：别把启动页当挡箭牌，放行进界面。
    if (phase === '' && Date.now() - startedAt >= STARTUP_AGENT_WAIT_MS) {
      logClientUpdate('启动期代理未就绪，跳过本体更新等待（由运行期 watch 继续）')
      return 'none'
    }

    /*
     * 无论代理上报什么状态都要请求一次准备：
     * snapshot 会拿 current.json 播种成 `ready`，而那份指针通常指的是**正在运行的版本**，
     * 于是"看起来已就绪"其实只是自己。只有 apply 才会让代理去服务端比对清单，
     * 漏掉这一步的话启动页会一直等到超时、永远发现不了新版本。
     * apply 自身幂等（代理在跑同一轮时直接回当前状态），重复请求无副作用。
     */
    if (!applySent && Date.now() - lastApplyTryAt >= 2000) {
      lastApplyTryAt = Date.now()
      try {
        const result = await agent.request({
          method: 'POST',
          path: '/agent/update/client/apply',
          body: { current_version: currentVersion },
          timeoutMs: 15000
        })
        if (result.status === 202 || result.status === 200) {
          applySent = true
          // 上面读到的是 apply **之前**的状态（可能是上次残留的 failed / 当前版本播种的
          // ready）：直接进下一轮，用这一轮自己的结论判断，别被旧状态误导着提前放行。
          await sleep(STARTUP_POLL_MS)
          continue
        }
      } catch {
        // 忽略：由下面的预算兜底。
      }
    }

    if (phase === 'failed') {
      const reason = describeState(state?.error)
      /*
       * no_session / server_unreachable 不是"无需更新"，而是"现在还问不到"：
       * 开机时代理刚拉起，登录会话往往还没建立。上一版在这里直接放行进界面，
       * 于是启动页永远拿不到结论、自动更新看起来"根本没跑"（现场日志里每台机器
       * 第一轮都是 no_session）。宽限期内继续重试，问到了再决定。
       */
      const transient = reason === 'no_session' || reason === 'server_unreachable'
      if (!transient || Date.now() - startedAt >= STARTUP_CHECK_GRACE_MS) {
        logClientUpdate(`启动期检查：无需更新的结论（${reason}）`)
        return 'none'
      }
      // 重新发一次 apply：会话可能刚好在这一轮建立，读旧状态只会一直得到 failed。
      applySent = false
      lastApplyTryAt = 0
      report('checking')
      await sleep(STARTUP_POLL_MS)
      continue
    }

    if (phase === 'downloading') {
      const received = typeof state?.received_bytes === 'number' ? state.received_bytes : 0
      const total = typeof state?.total_bytes === 'number' ? state.total_bytes : 0
      report('downloading', total > 0 ? Math.min(99, Math.floor((received / total) * 100)) : 0)
    } else if (phase === 'verifying' || phase === 'extracting') {
      report('preparing')
    } else {
      report('checking')
    }

    // 已经在下载/校验/解压就多给些时间：用户勾了自动更新就是想让它装完。
    const preparing = phase === 'downloading' || phase === 'verifying' || phase === 'extracting'
    const budget = preparing ? STARTUP_PREPARE_BUDGET_MS : STARTUP_CHECK_GRACE_MS
    if (Date.now() - startedAt >= budget) {
      logClientUpdate(`启动期等待本体更新超时（${phase || 'unknown'}），先放行进界面`)
      return 'timeout'
    }

    await sleep(STARTUP_POLL_MS)
  }
}
