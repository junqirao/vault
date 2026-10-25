import type { ReactElement } from 'react'
import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Tooltip } from 'antd'

import { useApi } from '../../api/provider'
import type { RepoDTO } from '../../api/types'
import { UsageBar } from '../../components/UsageBar'
import { useI18n } from '../../i18n'
import { fontSize, palette, radius, spacing } from '../../tokens/palette'
import { formatBytes, repoUsage, userDiffUsage } from '../../utils/format'
import { isPlaceholderRepoId } from './uploadView'

/**
 * 明细气泡的宽度：刚好放下「22.8 GB / 24.9 GB」这种一行数值，不再撑成一块面板。
 *
 * 208 → 250（按反馈加宽 20%）：窄了一点之后「本机用量 · 未挂载」这类长标签会和右侧数值贴到一起，
 * 放宽两成后两列之间留出余量，长库名/大容量数字也不会挤行。
 * 250 + 左右内边距已超过 antd 的 `max-width: 250px`，所以外壳里把 root 的上限一并放开（见下）。
 */
const TIP_WIDTH = 250

/**
 * 共享库的差异盘用量（服务端口径）：**挂载与否都报它**，未挂载时也报得出
 * "分给我的那块差异盘用了多少"。
 *
 * 为什么不用本机实测（真实反馈 + 截图："差异盘其实只有几十 M 但是硬是展示的母盘的用量"）：
 * 共享库挂载到本机的那个卷是**整块盘** —— 差异盘派生自母盘，母盘的数据在卷里全看得见，
 * 所以卷内"已用"是 23 GB 这种全盘数据量，跟差异盘自己吃掉的 36 MB 完全不是一回事。
 * 差异盘实际占多少，只有 `physical_bytes` 知道（见 utils/format 的 diffDiskUsage；
 * 也**不能**用 used_bytes，那是按标称容量计费的配额口径，同样会把进度条顶到 100%）。
 *
 * 查询 key 与详情页「磁盘」页签、分组卡片的聚合共用（`['repo-disks', id]`），
 * 同一页面上多处使用只会发一次请求。`enabled` 由调用方控制：独享库没有差异盘，不必发。
 * 服务端 physical_bytes 每 5 分钟采样一次，这里按 60s 轮询一次即可跟上（不必更密）。
 */
export function useRepoDiffUsage(
  repo: RepoDTO | undefined,
  enabled: boolean,
  userId?: string
): { usage?: { used: number; total: number }; loading: boolean } {
  const api = useApi()
  const active = Boolean(enabled && repo && repo.mode === 'shared' && !isPlaceholderRepoId(repo.id))
  // 同一库的所有差异盘属于不同用户（每块一份容量），必须先用分配记录定位"我的那块"，
  // 不能把整库的 diff 盘加总 —— 否则分母会变成 N × 容量（见 utils/format 的 userDiffUsage）。
  const disks = useQuery({
    queryKey: ['repo-disks', repo?.id],
    queryFn: () => api.listRepoDisks(repo!.id),
    enabled: active,
    refetchInterval: active ? 60_000 : false
  })
  const allocations = useQuery({
    queryKey: ['repo-allocations', repo?.id],
    queryFn: () => api.listAllocations(repo!.id),
    enabled: active,
    refetchInterval: active ? 60_000 : false
  })
  const usage = useMemo(
    () => (active ? userDiffUsage(disks.data?.items, allocations.data?.items, userId) : undefined),
    [active, disks.data, allocations.data, userId]
  )
  return { usage, loading: active && (disks.isPending || allocations.isPending) }
}

/** 用量展示的最小形状：本机实测（AgentMountVolumeUsage）与服务端差异盘用量都能喂进来。 */
export type RepoUsageVolume = { used_bytes?: number; total_bytes?: number }

/** 把差异盘用量转成 repoUsage 认的形状（本机实测与它同构，共用一个计算入口）。 */
export function diffUsageAsVolume(diff: { used: number; total: number } | undefined): RepoUsageVolume | undefined {
  return diff ? { used_bytes: diff.used, total_bytes: diff.total } : undefined
}

/**
 * 用量悬浮说明的外壳（卡片与详情页共用同一个 Tooltip，两处看到的是同一份明细）。
 *
 * **必须自定义白底**：antd 的 Tooltip 默认是深色气泡，而明细里的正文用的是全站深色文字
 * （palette.textPrimary / textSecondary）——不改底色就是黑底黑字，什么都看不清（真实反馈）。
 * 气泡改成白底 + 轻阴影 + 与卡片同档圆角，与页面其余浮层一个观感；
 * `color` 同时也把箭头染白，不然箭头与气泡之间会有一条深色缺口。
 */
export function RepoUsageTooltip({
  repo,
  usage,
  loading = false,
  children
}: {
  repo: RepoDTO
  /**
   * 展示用的用量：一般是本机实测（未挂载 / 取不到时为空）；
   * 共享库未挂载时由调用方填入服务端的差异盘用量（见 useRepoDiffUsage）。
   */
  usage?: RepoUsageVolume
  /** 用量正在取数：本机那条画转圈（与卡片上的条同一时刻、同一状态）。 */
  loading?: boolean
  /** 触发元素：卡片上的用量行、详情页的「已用」格子。 */
  children: ReactElement
}): JSX.Element {
  return (
    <Tooltip
      title={<RepoUsageTip repo={repo} usage={usage} loading={loading} />}
      // 从**左侧**弹出（反馈）：气泡在用量那格的左边展开，不盖住卡片标题与库名。
      // 贴近屏幕左缘时 antd 会自动翻到右侧（autoAdjustOverflow 默认开），不用另做处理。
      placement="left"
      mouseEnterDelay={0.3}
      color={palette.bgElevated}
      styles={{
        // antd 给 .ant-tooltip 固定了 max-width: 250px，会把这 250 的内容区连同内边距一起压扁；
        // 放开上限，宽度完全由内容（TIP_WIDTH）决定。
        root: { maxWidth: 'none' },
        body: {
          padding: `${spacing.xs}px ${spacing.sm}px`,
          background: palette.bgElevated,
          color: palette.textPrimary,
          borderRadius: radius.md,
          boxShadow: '0 6px 16px rgba(0, 0, 0, 0.12)'
        }
      }}
    >
      {children}
    </Tooltip>
  )
}

/**
 * 用量明细：卡片上那条进度条讲的是**这块盘实际用了多少**，其余服务端口径的数字放在这里。
 *
 * 为什么必须分层（真实反馈："进度条应展示本地用量，hover 时才在 tooltips 中展示母盘用量
 * 进度条，以及一些相关的用量信息"）：
 *   - 卡片上一行只装得下一个数，多塞一个就变成"同一个库三个数"，而且都不是用户想问的；
 *   - 但把服务端口径整个拿掉也不行：用户要判断"服务端那边到底占了多大"时无处可看。
 * 所以默认只留一行，其余收进悬浮说明 —— 要看的时候 hover 一下，不看也不打断视线。
 *
 * 用量行**按库模式换口径**（真实反馈 + 截图："差异盘其实只有几十 M 但是硬是展示的母盘的
 * 用量"）：
 *   - **共享库**：第一行报**分给我的那块差异盘**（由调用方填入服务端的 `physical_bytes`，
 *     见 useRepoDiffUsage），第二行照旧报母盘物理占用（22.8 GB / 24.9 GB）。
 *     本机挂载的那个卷虽然是差异盘，但它是**整块盘**：差异盘派生自母盘，母盘的数据在卷里
 *     全看得见，所以卷内"已用"是 23 GB 这种全盘数据量 —— 那正是"快要用完了"的假象来源，
 *     因此共享库不看本机实测。
 *   - **独享库**：报本机实测用量，另附母盘物理占用（独享库自己那块盘就是"母盘"）。
 *
 * **尺寸克制**（真实反馈："不要那么大"）：字号一律 12，条用 6px 细条且不叠百分数
 * （数值已经写在行内），长段解释文字全部去掉 —— 气泡里只留数字与短标签，一眼扫完。
 * 每条都复用 UsageBar：同一个页面里两条"用量"长得不一样，只会让人怀疑它们量的不是一回事。
 *
 * 独享库未挂载时仍写 `-` 并置灰、标签后标「未挂载」（口径见 utils/format 的 repoUsage）：
 * 那一刻确实没有本机实测值，也不该拿"已分配容量"冒充用量。
 */
export function RepoUsageTip({
  repo,
  usage,
  loading = false
}: {
  repo: RepoDTO
  /**
   * 展示用的用量：一般是本机实测（未挂载 / 取不到时为空）；
   * 共享库未挂载时由调用方填入服务端的差异盘用量（见 useRepoDiffUsage）。
   */
  usage?: RepoUsageVolume
  /** 用量正在取数：本机那条画转圈（与卡片上的条同一时刻、同一状态）。 */
  loading?: boolean
}): JSX.Element {
  const { t } = useI18n()
  const capacity = repo.capacity_bytes && repo.capacity_bytes > 0 ? repo.capacity_bytes : 0
  const shared = repo.mode === 'shared'

  // 共享库第一行只报**分给我的那块差异盘**（真实反馈 + 截图："差异盘其实只有几十 M 但是
  // 硬是展示的母盘的用量"）：数值由调用方用服务端的 physical_bytes 填进来（见
  // useRepoDiffUsage）。本机挂载的那个卷是整块盘，母盘的内容也在里面 —— 卷内"已用"是
  // 23 GB 的全盘数据量，拿它当差异盘用量就会让用户以为快满了。
  // 第二行**两种模式都画母盘**：共享库的母盘虽然是全组共用，但它同样是这个库的存储开销，
  // 用户要看的正是 22.8 GB / 24.9 GB 那个数（真实反馈："hover 里也没有展示母盘的用量"）。
  const { percent, usedText, totalText, measured } = repoUsage(repo, usage, true)
  const label = shared
    ? t('repo.usageTip.diff')
    : measured || loading
      ? t('repo.usageTip.local')
      : `${t('repo.usageTip.local')} · ${t('repo.mount.notMounted')}`

  // 母盘行（两种模式都画）：0 表示采样任务还没量到（每 5 分钟一次），不是"盘是空的"——写 `-`。
  const parentUsed = repo.parent_used_bytes && repo.parent_used_bytes > 0 ? repo.parent_used_bytes : 0
  const parentKnown = parentUsed > 0
  const parentPercent = capacity > 0 && parentKnown ? Math.min(100, Math.round((parentUsed / capacity) * 100)) : 0
  const parentText =
    capacity > 0
      ? `${parentKnown ? formatBytes(parentUsed) : '-'} / ${formatBytes(capacity)}`
      : parentKnown
        ? formatBytes(parentUsed)
        : '-'

  // 剩余可用按"本机还能写多少"算：容量 - 已用。没有数值时不知道，写 `-`。
  const remaining = measured && capacity > 0 ? Math.max(0, capacity - (usage?.used_bytes ?? 0)) : undefined

  return (
    <div style={{ width: TIP_WIDTH, fontSize: fontSize.xs, color: palette.textPrimary }}>
      <div style={{ fontWeight: 600, marginBottom: spacing.xxs }}>{t('repo.usageTip.title')}</div>
      <TipRow
        label={label}
        value={totalText ? `${usedText} / ${totalText}` : usedText}
        percent={percent}
        muted={!measured}
        loading={loading}
      />
      <TipRow label={t('repo.usageTip.parent')} value={parentText} percent={parentPercent} muted={!parentKnown} />
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          gap: spacing.xs,
          marginTop: spacing.xs,
          paddingTop: spacing.xxs,
          borderTop: `1px solid ${palette.border}`,
          color: palette.textTertiary
        }}
      >
        <span>{`${t('repo.usageTip.remaining')} ${remaining === undefined ? '-' : formatBytes(remaining)}`}</span>
        {/* 共享库的「服务端已分配」就是上面那条差异盘用量，再报一遍等于同一行写两次；
            右格改报差异盘上限（共享数量），那是另一个维度、且用户判断"还能再分给几个人"要用。 */}
        <span>
          {shared
            ? `${t('repo.card.shareCount')} ${repo.max_diff_disks}`
            : `${t('repo.usageTip.allocated')} ${formatBytes(repo.used_bytes ?? 0)}`}
        </span>
      </div>
    </div>
  )
}

/**
 * 一条"标签 + 数值 + 细进度条"：本机用量与母盘用量各占一组。
 *
 * 数值用 tabular-nums：两个 GB 数上下对齐，扫一眼就能比出大小，不必逐字读。
 */
function TipRow({
  label,
  value,
  percent,
  muted,
  loading
}: {
  label: string
  value: string
  percent: number
  muted?: boolean
  loading?: boolean
}): JSX.Element {
  return (
    <div style={{ marginTop: spacing.xxs }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        <span style={{ color: palette.textSecondary }}>{label}</span>
        <span style={{ color: palette.textPrimary, fontVariantNumeric: 'tabular-nums' }}>{value}</span>
      </div>
      <UsageBar percent={percent} muted={muted} loading={loading} height={6} showPercent={false} />
    </div>
  )
}
