/**
 * 客户端版本更新（启动期，经独立更新器 Vault-Updator.exe 完成）。
 *
 * 与旧实现的关键区别：**外壳在检查更新之前不启动本地服务**。
 *
 *   Vault.exe 启动
 *     └─ 显示启动页 → 拉起 Vault-Updator.exe bootstrap
 *          ├─ 自己读配置找更新源（更新接口匿名，不需要登录/代理）
 *          ├─ 拉清单 → 验签 → 版本单调性 → 下载 → sha256 → 解压 → 激活
 *          └─ 就绪后等 Vault.exe 退出 → 启动新版本（整个应用重启）
 *     └─ 只有确认"没有更新 / 更新不可用"之后，外壳才启动 Vault-Agent.exe
 *
 * client 产物是**整个程序目录**（Vault.exe + app.asar + Vault-Agent.exe +
 * Vault-Updator.exe）。前端与后端来自同一个已签名包：要么一起换、要么都不换，
 * 不会再出现"前端换了、本地服务还是旧的"或反过来的版本不一致。
 *
 * 本模块只负责"进程编排 + 把更新器事件翻译成启动页状态"，不碰下载/解压逻辑。
 */

import { app } from 'electron'
import { spawn, type ChildProcess } from 'node:child_process'
import fs from 'node:fs/promises'
import path from 'node:path'

import { agentDataDir } from './webapp'

/** 本体根目录（与 Go 侧 clientRootDir() 一致，仅用于日志/排障）。 */
export function clientRootDir(): string {
  return path.join(agentDataDir(), 'client')
}

/**
 * 一次启动期更新最多等多久。
 *
 * 更新器自己会在检查阶段很快给出结论；这段时间主要留给下载/解压。超时后放行进界面，
 * 避免网络极慢时把用户永久按在启动页上（下一个启动会继续检查，已下载的分片可续传）。
 */
const BOOTSTRAP_OVERALL_TIMEOUT_MS = 15 * 60 * 1000
/** 更新器进程没有输出任何事件的最长静默期：防止它卡死时一直占着启动页。 */
const BOOTSTRAP_IDLE_TIMEOUT_MS = 3 * 60 * 1000

/**
 * 关键事件落盘到 `<ProgramData>\Vault\logs\client.log`。
 *
 * 打包后的主进程 console 完全不可见；更新失败时这份日志是唯一现场。
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

/** 独立更新器的路径：与 Vault.exe 同级（electron-builder 的 extraFiles 放的）。 */
function updatorExePath(): string {
  return path.join(path.dirname(app.getPath('exe')), 'Vault-Updator.exe')
}

/** 启动页要展示的状态（文案表在 public/loading.html 的 BOOT 里）。 */
export type StartupUpdateStatus = 'checking' | 'downloading' | 'preparing' | 'restarting'

/** 启动期更新门的结果。 */
export type StartupUpdateOutcome =
  /** 没有需要装的更新（或更新不可用）：正常启动本地服务并进界面。 */
  | 'none'
  /** 已拉起新版本进程，本进程稍后会被接管结束。 */
  | 'switched'
  /** 等超时了：先放行进界面，保持当前版本。 */
  | 'timeout'
  /** 客户端目录里没有 Vault-Updator.exe（开发态/极端裁剪包）：直接放行。 */
  | 'unavailable'

/** 切换器接手后的收尾动作，必须由主进程实现（本模块不碰进程生命周期）。 */
export interface SwitchOptions {
  /**
   * 更新器已确认新版本解压完成，主进程应当：隐藏窗口 → 停掉可能存在的代理 → `app.exit(0)`。
   *
   * 正常启动路径下代理此时**还没有启动**；这里仍然调用 shutdown 只是兜底
   * （例如启动页之前有别的代码拉起了代理），确保新进程启动时没有旧代理占着互斥体。
   */
  onSwitching?: () => void | Promise<void>
}

export interface StartupUpdateOptions extends SwitchOptions {
  onStatus?: (status: StartupUpdateStatus, percent?: number) => void
}

/** 更新器写出的 JSON Lines 事件（见 cmd/vault-updator 与 internal/agent/bootstrap.go）。 */
interface BootstrapEvent {
  event?: unknown
  phase?: unknown
  version?: unknown
  from_version?: unknown
  exe?: unknown
  reason?: unknown
  message?: unknown
  received_bytes?: unknown
  total_bytes?: unknown
}

/** 把未知值压成安全字符串。 */
function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

/** 把未知值压成安全数字。 */
function number(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

/**
 * 在启动页里等一次完整的版本更新。
 *
 * 返回 'switched' 时 onSwitching 已经执行（进程通常已经退出）；其余结果都表示
 * "继续用当前版本启动"，调用方随后才应启动本地服务。
 */
export async function awaitStartupBootstrap(
  currentVersion: string,
  options: StartupUpdateOptions = {}
): Promise<StartupUpdateOutcome> {
  const exe = updatorExePath()
  try {
    const info = await fs.stat(exe)
    if (!info.isFile()) throw new Error('not a file')
  } catch {
    logClientUpdate(`未找到独立更新器 ${exe}，跳过启动期更新`)
    return 'unavailable'
  }

  const notify = options.onStatus ?? ((): void => undefined)
  const startedAt = Date.now()
  let lastEventAt = Date.now()
  let settled = false
  /** 是否已经收到 ready：此后进程退出不再按"更新失败"处理。 */
  let sawReady = false
  let child: ChildProcess | null = null
  let buffer = ''
  let timer: NodeJS.Timeout | null = null
  let resolveOutcome: (outcome: StartupUpdateOutcome) => void = () => undefined

  const outcome = new Promise<StartupUpdateOutcome>((resolve) => {
    resolveOutcome = resolve
  })

  const finish = (value: StartupUpdateOutcome): void => {
    if (settled) return
    settled = true
    if (timer) {
      clearInterval(timer)
      timer = null
    }
    resolveOutcome(value)
  }

  /** 超时/放弃时结束更新器进程（它自己会清理未完成的解压目录）。 */
  const abort = (reason: string): void => {
    logClientUpdate(`启动期更新中止：${reason}`)
    try {
      child?.kill()
    } catch {
      // 进程可能已退出：忽略。
    }
    finish(reason === 'timeout' ? 'timeout' : 'none')
  }

  const handleEvent = (event: BootstrapEvent): void => {
    lastEventAt = Date.now()
    const kind = text(event.event)
    if (kind === 'status') {
      const phase = text(event.phase)
      if (phase === 'downloading') {
        const received = number(event.received_bytes)
        const total = number(event.total_bytes)
        notify('downloading', total > 0 ? Math.min(99, Math.floor((received / total) * 100)) : 0)
        return
      }
      if (phase === 'verifying' || phase === 'extracting') {
        notify('preparing')
        return
      }
      if (phase === 'restarting') {
        notify('restarting')
        return
      }
      notify('checking')
      return
    }
    if (kind === 'none') {
      logClientUpdate(`启动期检查：无需更新（${text(event.reason) || 'unknown'}）`)
      finish('none')
      return
    }
    if (kind === 'error') {
      // 更新失败不阻断启动：当前版本仍然完整可用，下一次启动会重新检查。
      logClientUpdate(
        `启动期更新失败（${text(event.reason) || 'unknown'}）：${text(event.message) || '无详细信息'}`
      )
      finish('none')
      return
    }
    if (kind === 'ready') {
      const version = text(event.version)
      sawReady = true
      logClientUpdate(`新版本 ${version} 已就绪，交给更新器重启整个应用`)
      notify('restarting')
      // 更新器现在正在等本进程退出；先让主进程完成隐窗/停止代理/退出。
      void (async (): Promise<void> => {
        try {
          await options.onSwitching?.()
        } catch (error) {
          const reason = error instanceof Error ? error.message : String(error)
          logClientUpdate(`切换收尾动作失败（更新器会等待超时）：${reason}`)
        }
        finish('switched')
      })()
    }
  }

  const consume = (chunk: string): void => {
    buffer += chunk
    const lines = buffer.split(/\r?\n/)
    buffer = lines.pop() ?? ''
    for (const line of lines) {
      const trimmed = line.trim()
      if (trimmed === '') continue
      try {
        handleEvent(JSON.parse(trimmed) as BootstrapEvent)
      } catch {
        // 非 JSON 行（理论上不该出现）只记日志，不影响解析后续事件。
        logClientUpdate(`更新器输出无法解析：${trimmed.slice(0, 300)}`)
      }
    }
  }

  try {
    // detached：Electron 退出后更新器必须继续存活，才能启动新版本。
    // Windows 下 detached 会为**控制台程序**另建控制台窗口，所以 Vault-Updator.exe
    // 必须以 GUI 子系统链接（见 build.py 的 GUI_SUBSYSTEM_TARGETS），从根上不创建控制台；
    // windowsHide 再兜一层。stdio 用管道：启动页需要实时进度。
    child = spawn(
      exe,
      ['bootstrap', '-pid', String(process.pid), '-current-version', currentVersion, '-json'],
      {
        cwd: path.dirname(exe),
        detached: true,
        windowsHide: true,
        stdio: ['ignore', 'pipe', 'pipe']
      }
    )
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    logClientUpdate(`拉起独立更新器失败，跳过启动期更新：${reason}`)
    return 'unavailable'
  }

  child.stdout?.setEncoding('utf8')
  child.stdout?.on('data', consume)
  child.stderr?.setEncoding('utf8')
  child.stderr?.on('data', (chunk: string) => {
    // 更新器的 stderr 只用于人类可读提示；截断后写入 client.log，避免无限增长。
    const line = chunk.trim()
    if (line !== '') logClientUpdate(`更新器：${line.slice(0, 500)}`)
  })
  child.on('error', (error) => {
    logClientUpdate(`更新器进程错误：${error.message}`)
    finish('unavailable')
  })
  child.on('exit', (code) => {
    if (settled) return
    if (sawReady) {
      // ready 之后进程退出只可能是"已经启动新版本 / 等待旧进程超时"：
      // 不能把它翻回 none，否则主进程会在切换收尾过程中又去启动本地服务。
      finish('switched')
      return
    }
    logClientUpdate(`更新器提前退出（code=${code ?? 'null'}）且未给出结论`)
    finish('none')
  })

  timer = setInterval(() => {
    if (settled) return
    const now = Date.now()
    if (now - startedAt >= BOOTSTRAP_OVERALL_TIMEOUT_MS) {
      abort('timeout')
      return
    }
    if (now - lastEventAt >= BOOTSTRAP_IDLE_TIMEOUT_MS) {
      abort('idle_timeout')
    }
  }, 1000)

  return outcome
}
