/**
 * 开机自启。
 *
 * ## 为什么不能只靠 app.setLoginItemSettings
 *
 * Vault.exe 的清单是 `requireAdministrator`（挂载/卸载 VHD 需要管理员权限，见
 * electron-builder.yml 的 win.requestedExecutionLevel）。而 `setLoginItemSettings` 写的是
 * `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，登录时由 explorer 直接
 * CreateProcess 拉起 —— 遇到 requireAdministrator 必然弹 UAC 确认框。用户不点、点否、
 * 或者根本没看到（远程桌面/无人值守），客户端就不会启动。这正是"开关明明打开了，
 * 开机却没反应"最常见的原因。
 *
 * 所以主路径是**计划任务**：`-AtLogOn` + `RunLevel Highest` + `LogonType Interactive`，
 * 由任务计划程序在用户登录时直接以最高权限启动，**不经过 UAC 弹窗**。
 * `app.setLoginItemSettings` 只作为任务创建失败时的兜底（会弹 UAC，但总比不启动好）。
 *
 * ## 为什么每次启动都要校准
 *
 * 客户端本体自动更新会把整个程序目录换到 `%ProgramData%\Vault\client\<版本>\`
 * （见 clientUpdate.ts），`process.execPath` 随之改变。自启项若仍指向旧目录，而旧目录
 * 已被清理，下次开机就什么都不会发生 —— 表现为"更新一次之后自启就坏了"。
 * 因此 syncAutoStart 在每次启动（代理就绪后）比对"自启项目标"与当前 exe，
 * 不一致或目标文件已不存在就重建。
 */

import { app } from 'electron'
import { execFile } from 'node:child_process'
import fs from 'node:fs/promises'
import path from 'node:path'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)

/** 计划任务名。固定值：重装/换目录都复用同一个任务，不累积垃圾任务。 */
const TASK_NAME = 'VaultClient'

/**
 * PowerShell 超时。
 *
 * 比 agent.ts 的 runPowerShell（5s）宽：这里是冷启动 powershell.exe +
 * Register-ScheduledTask 的写操作，机器刚开机、磁盘繁忙时 5s 会误判成失败。
 */
const PS_TIMEOUT_MS = 20000

/** 自启项在系统里的实际状态。 */
export interface AutoStartState {
  /** 是否已登记开机自启。 */
  enabled: boolean
  /** 自启项指向的可执行文件；没有自启项时为空串。 */
  target: string
  /** 承载方式：计划任务 / 注册表 Run / 无。 */
  mode: 'task' | 'run' | 'none'
}

/** PowerShell 单引号字符串转义（单引号内 `'` 写成 `''`）。 */
function psQuote(value: string): string {
  return `'${value.replace(/'/g, "''")}'`
}

async function runPs(script: string): Promise<string> {
  const { stdout } = await execFileAsync(
    'powershell.exe',
    ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', script],
    { windowsHide: true, timeout: PS_TIMEOUT_MS, maxBuffer: 1 << 20 }
  )
  return stdout
}

async function fileExists(target: string): Promise<boolean> {
  try {
    return (await fs.stat(target)).isFile()
  } catch {
    return false
  }
}

/** 路径比较（Windows 大小写不敏感）。 */
function samePath(a: string, b: string): boolean {
  if (!a || !b) return false
  return path.resolve(a).toLowerCase() === path.resolve(b).toLowerCase()
}

/** 查询计划任务指向的 exe；任务不存在返回空串。 */
async function queryTaskTarget(): Promise<string> {
  const script = [
    `$ErrorActionPreference='Stop'`,
    `$t=Get-ScheduledTask -TaskName ${psQuote(TASK_NAME)} -ErrorAction Stop`,
    `$t.Actions | ForEach-Object { $_.Execute }`
  ].join('; ')
  const out = await runPs(script)
  const first = out
    .split(/\r?\n/)
    .map((line) => line.trim())
    .find((line) => line !== '')
  return first ?? ''
}

/** 注册（覆盖）计划任务。 */
async function createTask(exe: string): Promise<void> {
  const script = [
    `$ErrorActionPreference='Stop'`,
    `$exe=${psQuote(exe)}`,
    `$dir=Split-Path -Parent $exe`,
    // 触发用户必须是**交互式登录的那个用户**，不能用进程身份：
    // 企业环境常见"标准用户登录 + 用管理员凭据提权"，此时进程身份是那个管理员账户，
    // 拿它做 -AtLogOn 会导致标准的日常用户登录后客户端根本不启动（自启"看起来无效"）。
    // Win32_ComputerSystem.UserName 返回的是真正的登录用户，取不到再退回进程身份。
    `$me=(Get-CimInstance Win32_ComputerSystem -ErrorAction SilentlyContinue).UserName`,
    `if (-not $me) { $me=[System.Security.Principal.WindowsIdentity]::GetCurrent().Name }`,
    `$action=New-ScheduledTaskAction -Execute $exe -WorkingDirectory $dir`,
    `$trigger=New-ScheduledTaskTrigger -AtLogOn -User $me`,
    // AllowStartIfOnBatteries：笔记本只用电池时也启动（默认会跳过，等于"自启无效"）。
    // MultipleInstances IgnoreNew：同一用户重复登录/快速切换时不要叠进程。
    `$settings=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero)`,
    `$principal=New-ScheduledTaskPrincipal -UserId $me -LogonType Interactive -RunLevel Highest`,
    `Register-ScheduledTask -TaskName ${psQuote(TASK_NAME)} -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force | Out-Null`
  ].join('; ')
  await runPs(script)
}

/** 删除计划任务（不存在不算失败）。 */
async function removeTask(): Promise<void> {
  const script = [
    `$ErrorActionPreference='Stop'`,
    `$t=Get-ScheduledTask -TaskName ${psQuote(TASK_NAME)} -ErrorAction SilentlyContinue`,
    `if ($null -ne $t) { Unregister-ScheduledTask -TaskName ${psQuote(TASK_NAME)} -Confirm:$false }`
  ].join('; ')
  await runPs(script)
}

/** 清掉 HKCU Run 里的自启项（无论有没有）。 */
function clearRunEntry(): void {
  try {
    app.setLoginItemSettings({ openAtLogin: false })
  } catch {
    // 注册表写入失败不影响计划任务这条主路径。
  }
}

/** 读取系统里的自启状态。 */
export async function queryAutoStart(): Promise<AutoStartState> {
  if (process.platform !== 'win32') return { enabled: false, target: '', mode: 'none' }

  try {
    const target = await queryTaskTarget()
    if (target !== '') return { enabled: true, target, mode: 'task' }
  } catch {
    // 任务不存在（或 Get-ScheduledTask 不可用）：继续看兜底的 Run 项。
  }

  try {
    const run = app.getLoginItemSettings()
    if (run.openAtLogin) {
      // Windows 的 LoginItemSettings 没有 path 字段：条目在 launchItems 里。
      const item = run.launchItems?.find((entry) => entry.enabled)
      return { enabled: true, target: item?.path ?? '', mode: 'run' }
    }
  } catch {
    // 忽略：按"未启用"处理。
  }

  return { enabled: false, target: '', mode: 'none' }
}

/**
 * 打开/关闭开机自启，并返回系统的实际状态。
 *
 * 关闭时**两条路径都清**：用户可能先由主路径（任务）登记、后来失败回退成 Run 项，
 * 只删一条会留下"关了还自启"的鬼影。
 */
export async function applyAutoStart(enabled: boolean): Promise<AutoStartState> {
  if (process.platform !== 'win32') return { enabled: false, target: '', mode: 'none' }
  const exe = process.execPath

  if (!enabled) {
    try {
      await removeTask()
    } catch (error) {
      console.warn(`[vault] 删除自启计划任务失败：${messageOf(error)}`)
    }
    clearRunEntry()
    return { enabled: false, target: '', mode: 'none' }
  }

  try {
    await createTask(exe)
    // 任务建成功就清掉 Run 项：两条路径同时存在会重复拉起（虽被单实例锁挡住，
    // 但会多一次 UAC 弹窗和一次无效启动）。
    clearRunEntry()
    console.info(`[vault] 开机自启已登记（计划任务 ${TASK_NAME} → ${exe}）`)
    return { enabled: true, target: exe, mode: 'task' }
  } catch (error) {
    console.warn(`[vault] 创建自启计划任务失败，改用 HKCU Run 兜底：${messageOf(error)}`)
  }

  try {
    app.setLoginItemSettings({ openAtLogin: true, path: exe, args: [] })
    const run = app.getLoginItemSettings()
    if (run.openAtLogin) {
      console.info(`[vault] 开机自启已登记（HKCU Run → ${exe}；启动时会弹 UAC）`)
      return { enabled: true, target: exe, mode: 'run' }
    }
  } catch (error) {
    console.warn(`[vault] 写入自启注册表失败：${messageOf(error)}`)
  }

  return { enabled: false, target: '', mode: 'none' }
}

/**
 * 上一次成功对齐的 (开关, exe) 组合与结果。
 *
 * 用于跳过重复对齐：配置轮询与 IPC 之后都会调用 syncAutoStart，若每次都跑一次
 * PowerShell，开销完全没必要。
 */
let lastSyncedKey = ''
let lastSyncedState: AutoStartState | null = null

/**
 * 把系统自启项对齐到配置里的开关值。
 *
 * - 开启：自启项必须存在、指向**当前 exe**，且该文件真实存在；否则重建。
 * - 关闭：系统里不该有任何自启项；否则清掉。
 *
 * 失败时不写 lastSyncedKey，下次调用会再试一次（例如代理刚起来时被 UAC 拒绝）。
 */
export async function syncAutoStart(enabled: boolean): Promise<AutoStartState> {
  if (process.platform !== 'win32') return { enabled: false, target: '', mode: 'none' }
  const exe = process.execPath
  const key = `${enabled ? 1 : 0}|${exe.toLowerCase()}`
  if (key === lastSyncedKey && lastSyncedState) return lastSyncedState

  const state = await queryAutoStart()

  if (!enabled) {
    if (state.mode === 'none') {
      lastSyncedKey = key
      lastSyncedState = state
      return state
    }
    const cleared = await applyAutoStart(false)
    if (cleared.mode === 'none') {
      lastSyncedKey = key
      lastSyncedState = cleared
    }
    return cleared
  }

  // 已开启且已经指向当前 exe、文件也确实在 —— 不必重建。
  if (state.enabled && samePath(state.target, exe) && (await fileExists(state.target))) {
    if (state.mode === 'task') {
      // 任务模式下顺手清掉可能残留的 Run 项（两条路径并存会多一次无效启动 + UAC）。
      clearRunEntry()
      lastSyncedKey = key
      lastSyncedState = state
      return state
    }
    // Run 兜底模式：每次启动试一次升级成计划任务。任务启动不弹 UAC，比 Run 可靠；
    // 升级失败也无妨，applyAutoStart 会把 Run 项按原样重写一遍（幂等）。
    console.info('[vault] 自启当前是 HKCU Run 兜底模式，尝试升级为计划任务')
  }

  if (!state.enabled) {
    console.info(`[vault] 配置要求开机自启，登记自启项 → ${exe}`)
  } else {
    const missing = state.target !== '' && !(await fileExists(state.target))
    if (missing || !samePath(state.target, exe)) {
      console.info(
        `[vault] 自启项需要校准：${state.target || '(空)'} → ${exe}` +
          (missing ? '（原目标已不存在，通常是本体更新换了目录）' : '')
      )
    }
  }

  const applied = await applyAutoStart(true)
  if (applied.enabled) {
    lastSyncedKey = key
    lastSyncedState = applied
  }
  return applied
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
