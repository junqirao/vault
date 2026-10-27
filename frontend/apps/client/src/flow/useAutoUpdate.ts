import { useEffect, useRef, useState } from 'react'

import { agentApi, applyWebLayer, isAgentUnavailableError, type UpdateInfo } from '@vault/ui'

/**
 * 两次自动检查之间的最小间隔（同一进程内）。
 *
 * 页面刷新 / 重登录 / 代理重启都会重新挂载本 hook，若不加节流，一次操作就会连打好几次
 * 更新接口；而这个间隔内刚查过，再查也不会有新结论。
 */
const AUTO_UPDATE_INTERVAL_MS = 5 * 60 * 1000

/**
 * 客户端长时间挂着时，也要周期性再看一眼（否则只在启动/代理重连时检查一次，
 * 用户开着几天都收不到新版本）。这里每 30 分钟自动再查一轮。
 */
const AUTO_UPDATE_POLL_MS = 30 * 60 * 1000

/**
 * "没得出明确结论"时的退避序列（代理刚起、服务端还没连上）。
 *
 * 用户要求的是全自动：不点设置页、也不等下次登录，代理/服务端一就绪就该把更新查掉。
 * 有了退避，界面先于代理就绪、或登录前先查一次失败，都不会漏掉这一轮。
 */
const AUTO_UPDATE_RETRY_DELAYS_MS = [15_000, 30_000, 60_000, 120_000, 300_000]

/** 单次检查内，代理未就绪时的重试序列（与免密登录同理：界面常常比代理先就绪）。 */
const CHECK_RETRY_DELAYS_MS = [0, 1000, 3000]

/**
 * 自动更新与自动挂载必须错开。
 *
 * 代理退出前会卸载全部挂载（见 cmd/vault-agent/main.go 的退出路径）：若不等待，
 * 用户刚登录、自动挂载还在跑，代理就被替换退出——表现成"一登录挂载就断开"。
 * 最多等这么久，等不到就本轮先只做资源热更，整包留到下一次。
 */
const MOUNT_IDLE_WAIT_MS = 60 * 1000
const MOUNT_POLL_MS = 3 * 1000

/** 等热更资源激活的上限与轮询间隔；激活后立刻切层，不需要用户手动重启。 */
const WEB_UPDATE_POLL_MS = 1000
const WEB_UPDATE_WAIT_MS = 120 * 1000

/** 本轮是否得出了明确结论。没结论（代理/服务端还没就绪）时由调用方安排重试。 */
type RunOutcome = 'done' | 'retry'

/**
 * 全自动更新：**应用启动即检查**，整包能装就装、装不了就热更渲染层并立即生效。
 *
 * 背景：更新此前只有设置页里一个「检查更新」按钮，用户必须自己想起来去点；改成"登录后
 * 自动检查"后仍然不够——用户的实际要求是"全自动"：不进设置页、不手动重启。
 *
 * 行为：
 *  1. 触发时机只要求**代理可用**，不要求登录进主界面：代理一就绪就查（界面通常比代理先
 *     起来，稍后代理就绪也会立即补上）；没得出明确结论时按退避序列自动重试；
 *  2. 整包更新可用、且**不会**破坏任何已登录服务端的兼容性时，直接下发 applyUpdate；
 *     代理自行替换 exe 并退出，主进程会重新拉起并在就绪后自动重载界面（见 electron/main.ts）；
 *  3. 整包这轮没装（没有更新 / 需用户确认 / 挂载未就绪）时做渲染层资源热更：后台下载并
 *     激活，**激活后立刻切层**（applyWebLayer），界面当场换成新版本，不需要手动重启；
 *  4. `breaks_servers` 非空时**不自动安装**：升级会让这些服务端在对方升级前无法协作，
 *     属于必须由用户确认的动作，留给设置页的手动入口处理。
 *
 * 是否执行由设置页「更新 → 自动更新」控制（默认开启，见 Config.AutoUpdate）：关掉后本
 * 流程直接结束，只能用设置页的「检查更新」手动执行。
 *
 * 全程不弹窗：这是后台流程，失败只意味着继续用旧版本，不该打断正在使用的界面。
 */
export function useAutoUpdate(agentAvailable: boolean): void {
  /** 一次流程未结束时不再叠加（checkUpdate 可能要几十秒）。 */
  const runningRef = useRef(false)
  const lastRunRef = useRef(0)
  /** 连续"没结论"的次数；重试成功后归零。 */
  const retryIndexRef = useRef(0)
  /** 仅用于安排重试：递增即触发下面的 effect 再跑一轮。 */
  const [attempt, setAttempt] = useState(0)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    // 代理状态变化会重跑本 effect：先把上一轮排的定时器撤掉，避免叠加。
    if (timerRef.current) {
      clearTimeout(timerRef.current)
      timerRef.current = null
    }
    if (!agentAvailable || runningRef.current) return
    // 重试轮不受节流限制：它本来就是"上一轮没得出结论"的补偿。
    const isRetry = retryIndexRef.current > 0
    if (!isRetry && Date.now() - lastRunRef.current < AUTO_UPDATE_INTERVAL_MS) return

    runningRef.current = true
    const startedAt = Date.now()

    void (async (): Promise<void> => {
      const outcome = await runAutoUpdate()
      if (outcome === 'done') {
        // 只有得出明确结论才计入节流；"没结论"必须继续重试，否则这一轮更新就被吞掉了。
        lastRunRef.current = startedAt
        retryIndexRef.current = 0
        // 长时间挂着也要能收到新版本：过一段时间自动再查一轮。
        timerRef.current = setTimeout(() => {
          timerRef.current = null
          setAttempt((value) => value + 1)
        }, AUTO_UPDATE_POLL_MS)
        return
      }
      const index = Math.min(retryIndexRef.current, AUTO_UPDATE_RETRY_DELAYS_MS.length - 1)
      retryIndexRef.current += 1
      timerRef.current = setTimeout(() => {
        timerRef.current = null
        setAttempt((value) => value + 1)
      }, AUTO_UPDATE_RETRY_DELAYS_MS[index])
    })().finally(() => {
      runningRef.current = false
    })
  }, [agentAvailable, attempt])

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    []
  )
}

/** 跑一轮"检查 → 安装/热更"；返回 'retry' 时由调用方安排退避重试。 */
async function runAutoUpdate(): Promise<RunOutcome> {
  // 设置里的「自动更新」关掉后就不再自作主张；用户仍可用设置页的「检查更新」手动执行。
  // 读不到配置（代理刚启动）时按默认开启继续——后面的检查本身也会失败并触发重试。
  try {
    const cfg = await agentApi.getConfig()
    if (cfg.auto_update === false) return 'done'
  } catch {
    // 忽略：按默认（开启）继续。
  }

  let info: UpdateInfo
  try {
    info = await checkUpdateWithRetry()
  } catch (error) {
    // 更新源不可达、代理刚起都属正常软失败：退避后自动再试，不影响正在使用的界面。
    console.warn(`[auto-update] check failed: ${describe(error)}`)
    return 'retry'
  }

  // 只有"有新版本"或"确已最新"才算结论；server_unreachable / no_manifest 之类等下次再试。
  if (!info.available && info.reason !== 'not_newer') {
    console.info(`[auto-update] 暂无结论（${info.reason ?? 'unknown'}），稍后重试`)
    return 'retry'
  }

  if (info.available && (info.breaks_servers?.length ?? 0) === 0) {
    // 先等自动挂载这一轮结束，再让代理退出替换（见 MOUNT_IDLE_WAIT_MS 的说明）。
    if (await waitMountsIdle(Date.now() + MOUNT_IDLE_WAIT_MS)) {
      try {
        // 代理替换 exe 后退出，主进程重新拉起并在就绪后自动重载界面——全程无需用户操作。
        await agentApi.applyUpdate()
        return 'done'
      } catch (error) {
        console.warn(`[auto-update] apply failed: ${describe(error)}`)
      }
    }
  }

  await hotUpdateWebLayer()
  return 'done'
}

/**
 * 热更渲染层，并在激活后立刻切层。
 *
 * applyWebUpdate 是异步的（202 立即返回，下载/校验/解压/激活都在代理后台），所以这里轮询
 * 状态直到 activated；拿到 activated 后调 applyWebLayer，主进程会盖过渡页并加载新资源，
 * 当前页面随之被替换——这就是"不用手动重启"的那一步。
 *
 * 新层若启动即崩，主进程看门狗会回退内置资源并把该版本记为坏层（见 electron/main.ts）。
 */
async function hotUpdateWebLayer(): Promise<void> {
  try {
    await agentApi.applyWebUpdate()
  } catch (error) {
    // 没有可热更资源时代理以 failed 结束、接口本身仍返回 202，这里的 catch 只覆盖真实异常。
    console.warn(`[auto-update] web update failed: ${describe(error)}`)
    return
  }

  const deadline = Date.now() + WEB_UPDATE_WAIT_MS
  while (Date.now() < deadline) {
    await sleep(WEB_UPDATE_POLL_MS)
    try {
      const { web_update: state } = await agentApi.getWebUpdate()
      if (state.state === 'activated') {
        console.info('[auto-update] 渲染层资源已激活，立即切换（无需重启）')
        await applyWebLayer()
        return
      }
      if (state.state === 'idle' || state.state === 'failed') return
    } catch {
      return
    }
  }
}

/** 代理未就绪时按 CHECK_RETRY_DELAYS_MS 重试，其余错误直接抛出（重试结果也一样）。 */
async function checkUpdateWithRetry(): Promise<UpdateInfo> {
  let lastError: unknown = null
  for (const delay of CHECK_RETRY_DELAYS_MS) {
    if (delay > 0) await sleep(delay)
    try {
      return await agentApi.checkUpdate()
    } catch (error) {
      lastError = error
      if (!isAgentUnavailableError(error)) break
    }
  }
  throw lastError instanceof Error ? lastError : new Error(String(lastError))
}

/** 轮询到没有 mounting 状态的挂载为止；超时或查询失败返回 false（本轮不升级）。 */
async function waitMountsIdle(deadline: number): Promise<boolean> {
  for (;;) {
    try {
      const state = await agentApi.state()
      if (!state.mounts.some((mount) => mount.state === 'mounting')) return true
    } catch {
      return false
    }
    if (Date.now() >= deadline) return false
    await sleep(MOUNT_POLL_MS)
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/** 把错误压成一行日志（避免把整个 ApiError 对象打到控制台）。 */
function describe(error: unknown): string {
  if (error instanceof Error) return error.message
  return String(error)
}
