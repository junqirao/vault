import { formatBytes } from '../../utils/format'

/** 一行"下载中"进度条需要的最小数据（百分比 + 阶段文字 + 已下载/总量/速度）。 */
export interface SteamUpdateProgress {
  percent: number
  /** 阶段文案：下载中 / 校验中 / 准备中 … */
  label: string
  /** 明细：`1.2 GB / 4.5 GB · 120 MB/s`（没有字节数据时为空串）。 */
  value: string
}

/** 进度来源：游戏管理页的 GameDTO 与普通用户目录的 GameCatalogItemDTO 都满足。 */
export interface SteamUpdateSource {
  status: string
  progress_percent: number
  progress_phase: string
  progress_stage: string
  progress_bytes_done: number
  progress_bytes_total: number
  progress_speed: number
}

/** SteamCMD 内部阶段 → 翻译键后缀；未识别的阶段回退到 pipeline phase。 */
const UPDATE_STAGE_KEYS: Record<string, string> = {
  verifying: 'verifying',
  validating: 'verifying',
  committing: 'committing',
  preallocating: 'preallocating',
  reconfiguring: 'reconfiguring'
}

/**
 * 把 Steam 游戏的更新状态归一成"一条下载进度"。
 *
 * 游戏分组页与存储库列表共用：两处必须显示同一个阶段、同一个百分比、同一句明细，
 * 否则同一个更新在两页看起来像两个任务。
 */
export function steamUpdateProgress(
  game: SteamUpdateSource,
  t: (key: string, args?: Record<string, unknown>) => string
): SteamUpdateProgress {
  const total = game.progress_bytes_total || 0
  const done = game.progress_bytes_done || 0
  const fallbackPercent = total > 0 ? (done / total) * 100 : 0
  const percent = Math.max(0, Math.min(100, game.progress_percent || fallbackPercent))
  const speed = game.progress_speed || 0
  const stageKey = UPDATE_STAGE_KEYS[(game.progress_stage || '').toLowerCase()]
  const label = stageKey
    ? t(`steam.games.updateStage.${stageKey}`)
    : t(`steam.games.updatePhase.${game.progress_phase || 'downloading'}`)
  const value = [
    total > 0 ? `${formatBytes(done)} / ${formatBytes(total)}` : '',
    speed > 0 ? `${formatBytes(speed)}/s` : ''
  ]
    .filter(Boolean)
    .join(' · ')
  return { percent, label, value }
}
