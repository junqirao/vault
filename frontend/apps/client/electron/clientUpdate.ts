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
 *   - **真正完成切换的是现有的实例接管逻辑**（main.ts 的 takeoverOlderInstance）：
 *     这里只把新版本进程拉起来，新进程凭"版本更高 + 目录不同"接管并结束旧进程。
 *     这样即使新版本起不来，旧进程和旧窗口都还在，用户不会黑屏。
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

/** 同一版本的最短重试间隔：新版本启动即崩溃时不至于每次开机都再来一次。 */
const HANDOFF_DEDUPE_MS = 10 * 60 * 1000
/** 运行期查询代理状态的间隔。 */
const POLL_INTERVAL_MS = 5 * 1000
/** 有新版本就绪但仍有活跃挂载时的复查间隔（挂载期间绝不重启）。 */
const MOUNT_RECHECK_MS = 30 * 1000
/** 一轮准备失败后，隔多久再重新请求代理准备。 */
const APPLY_RETRY_MS = 30 * 60 * 1000

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

/**
 * 拉起新版本进程（不退出自己）。
 *
 * 关键：这里**不**调用 app.quit()/app.exit()。新进程会因为"已有实例在运行"走到
 * main.ts 的 takeoverOlderInstance：它版本更高且目录不同，于是结束本进程并接管。
 * 好处是旧窗口一直可用 —— 新版本若在启动早期崩溃，本进程根本不会被结束，用户无感。
 */
async function launchNewClient(version: string, exe: string): Promise<boolean> {
  const record = (await readJson(handoffRecordPath())) as HandoffRecord | null
  if (record?.version === version && typeof record.at === 'number' && Date.now() - record.at < HANDOFF_DEDUPE_MS) {
    // 刚试过同一版本：不再拉第二次（防止"启动即崩"演变成每次开机弹两个进程）。
    return false
  }
  await writeJson(handoffRecordPath(), { version, at: Date.now() })

  try {
    const child = spawn(exe, [], {
      cwd: path.dirname(exe),
      detached: true,
      stdio: 'ignore',
      env: { ...process.env, VAULT_CLIENT_HANDOFF: '1' }
    })
    child.unref()
    console.info(`[vault] 已拉起新的客户端进程以完成本体更新：${version}`)
    return true
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    console.warn(`[vault] 拉起新客户端失败（继续使用当前版本）：${reason}`)
    return false
  }
}

/**
 * 启动时检查本体更新，必要时拉起新版本进程。
 *
 * 返回 true 表示已发起切换（本进程稍后会被新进程接管结束）。
 */
export async function maybeHandoffOnStartup(currentVersion: string): Promise<boolean> {
  let ready: { version: string; exe: string } | null = null
  try {
    ready = await resolveReadyClient(currentVersion)
  } catch {
    return false
  }
  if (!ready) return false
  console.info(`[vault] 检测到已就绪的客户端本体更新：${ready.version}（当前 ${currentVersion}）`)
  return launchNewClient(ready.version, ready.exe)
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
 * 返回停止函数（应用退出时调用）。
 */
export function startClientUpdateWatch(agent: ClientUpdateAgent, currentVersion: string): () => void {
  let stopped = false
  let pollTimer: NodeJS.Timeout | null = null
  let applyTimer: NodeJS.Timeout | null = null

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
    if (stopped) return
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
      // 代理还没起来（或正在重启）：稍后再问，别把这一轮丢掉。
      scheduleApply(60 * 1000)
      return
    }
    if (stopped) return
    if (result.status !== 202 && result.status !== 200) {
      // 代理刚起来/正在重启：稍后再试（不要在这里放弃）。
      scheduleApply(60 * 1000)
      return
    }
    schedulePoll(1000)
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
      schedulePoll(10 * 1000)
      return
    }
    if (stopped) return
    if (result.status !== 200) {
      // 代理不可用（重启中）：退避后重来，别把这一轮更新丢掉。
      schedulePoll(10 * 1000)
      return
    }
    const state = readClientUpdateState(result.payload)
    const readyVersion = typeof state?.ready_version === 'string' ? state.ready_version.trim() : ''
    const phase = typeof state?.state === 'string' ? state.state : ''

    if (readyVersion !== '' && isNewerVersion(readyVersion, currentVersion)) {
      // 已经有更新版本就绪：等挂载清空后切换，期间不重复请求代理准备。
      if ((await agent.activeMountCount()) === 0) {
        // 直接以磁盘上的激活指针为准（它才是"解压完成且校验通过"的唯一凭据，
        // 代理状态只是提示；两者短暂不一致时不该卡住切换）。
        const ready = await resolveReadyClient(currentVersion)
        if (ready) {
          const started = await launchNewClient(ready.version, ready.exe)
          if (started) {
            // 新进程会接管并结束本进程；这里只需停止轮询。
            stop()
            return
          }
        }
      }
      schedulePoll(MOUNT_RECHECK_MS)
      return
    }

    if (phase === 'failed') {
      // 没有可用更新（not_newer / not_available）或下载失败：隔一段时间再问。
      scheduleApply(APPLY_RETRY_MS)
      return
    }

    // downloading / verifying / extracting：继续看进度。
    schedulePoll(POLL_INTERVAL_MS)
  }

  // 首次请求延后一点：等代理完成握手与自动挂载恢复，避免和启动期的请求挤在一起。
  scheduleApply(5000)

  return stop
}
