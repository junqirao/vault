import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import type { ReactNode } from 'react'
import {
  Button,
  Card,
  Col,
  Input,
  Pagination,
  Progress,
  Row,
  Segmented,
  Skeleton,
  Space,
  Tooltip,
  Typography,
  message
} from 'antd'
import type { TableColumnsType } from 'antd'
import { AppstoreOutlined, ArrowLeftOutlined, BarsOutlined, FolderOutlined, SettingOutlined } from '@ant-design/icons'
import { useQueries, useQuery, useQueryClient } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { UsageBar, usageColor } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { AgentMountState, UploadState } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { isActiveUpload, useAgent, useAgentUsagePolling } from '../../hooks/useAgent'
import { useAuth } from '../../hooks/useAuth'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes, formatTime, repoUsage, userDiffUsage } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
import { RepoCard, RepoRow } from './RepoCards'
import { GroupMountSettingsModal, RepoMountSettingsModal } from './RepoMountSettings'
import { PerformanceMonitor } from './PerformanceMonitor'
import { StorageUsageCard } from '../storage/StorageUsage'
import { isPlaceholderRepoId, uploadPlaceholderRepo } from './uploadView'

export interface RepoListProps {
  isSuperAdmin: boolean
  onCreate: () => void
  onOpen: (repoId: string) => void
  /**
   * 打开「上传任务」页：进行中的上传就以库的身份摆在列表里（见 RepoListEntry），
   * 点它进这个页面看进度 / 取消。
   *
   * 可选：管理端看的是服务端全量库，本机上传与它无关，不传即不出现上传中的条目。
   */
  onOpenUpload?: (uploadId: string) => void
  /**
   * 打开客户端只读存储页（首页「存储」卡片标题后的箭头按钮）。
   * 不传时（管理端列表）不渲染该卡片。
   */
  onOpenStorages?: () => void
  /**
   * 打开「导入母盘」页：把存储根里已存在的 VHDX 文件原地接管成存储库。
   *
   * 可选，且只在管理端出现：这是换服务端时的迁移入口（见 DiskImport），
   * 用户端没有存储根的概念，放它只会让人误以为能"导入别人的盘"。
   */
  onImport?: () => void
}

/**
 * 列表里的一行：一个存储库，外加它此刻的上传进度（正在上传时才有）。
 *
 * 上传中的库**不另起一块区域**，就和其他库并排躺在同一个分组里，用同一套卡片零件渲染 ——
 * 只有状态不同（真实反馈："就放在存储库那里展示，和其他存储库一样的样式，只是状态不同"）。
 */
interface RepoListEntry {
  repo: RepoDTO
  upload?: UploadState
}

/** 用户端展示形式：卡片网格（默认，一眼扫过多块盘）/ 整页宽度的列表（一库一行，便于横向比对）。 */
type RepoViewMode = 'card' | 'list'

/**
 * 每页条数按形态分别取值：卡片一行 3~4 张（每页 8 张 = 两行上下），列表一行一库（每页 10 行）。
 * 两者都压在一屏上下，分页才有意义 —— 一页要滚三屏的话，用户只会直接找搜索框。
 */
const PAGE_SIZE: Record<RepoViewMode, number> = { card: 8, list: 10 }

/**
 * 展示形式的本地偏好键。
 *
 * 与客户端配置（`vault.client.config`，走 Electron IPC 写 config.json）**分开**：展示形式是纯界面偏好，
 * 只跟"这台机器的这个界面"有关，不值得为它多铺一条跨进程写盘路径；localStorage 在浏览器 dev 与
 * Electron 渲染进程两边都在，刷新/重开都还在。
 */
const VIEW_PREF_KEY = 'vault.repo.view'

/**
 * 当前所在分组的会话记忆键。
 *
 * 用户从分组里点进存储库详情后，RepoList 会被卸载；按「返回」回来时组件重建，
 * 组件内的 activeGroup 已归零，于是回到外面的大列表而不是原来那个分组。
 * 用 sessionStorage 记住"这次会话最后进入的分组"：返回时能落回分组，关掉客户端后
 * 自然清空（下次进来仍从总览开始）。空串是一个合法分组（未分组），null = 总览。
 */
const GROUP_PREF_KEY = 'vault.repo.group'

function readStoredGroup(): string | null {
  try {
    const raw = window.sessionStorage.getItem(GROUP_PREF_KEY)
    return raw === null ? null : raw
  } catch {
    return null
  }
}

function storeGroup(group: string | null): void {
  try {
    if (group === null) window.sessionStorage.removeItem(GROUP_PREF_KEY)
    else window.sessionStorage.setItem(GROUP_PREF_KEY, group)
  } catch {
    // 会话记忆失败不影响使用：返回时落到总览而已。
  }
}

/** 读取本地记住的展示形式：读不到、值不合法、存储不可用，一律退回默认的卡片。 */
function readStoredView(): RepoViewMode {
  try {
    const raw = window.localStorage.getItem(VIEW_PREF_KEY)
    return raw === 'list' || raw === 'card' ? raw : 'card'
  } catch {
    return 'card'
  }
}

/** 记住展示形式：localStorage 可能不可用（隐私模式、配额写满），写不进去就当没记住，绝不影响界面。 */
function storeView(view: RepoViewMode): void {
  try {
    window.localStorage.setItem(VIEW_PREF_KEY, view)
  } catch {
    // 忽略：偏好没记住不是错误，不值得打扰用户。
  }
}

/**
 * 用户端分组区块标题：标题 + 数量在左，`extra`（形态切换）贴右。
 *
 * 切换控件放在内容区第一个分组的标题行右侧，而不是页面头部工具栏：它只作用于这一块列表，
 * 与搜索/刷新/新建（页面级操作）不是一类东西；标题为空时（拿不到当前用户的分组兜底）只渲染右侧控件。
 */
function RepoGroupHeader({
  title,
  count,
  extra
}: {
  title: string
  count: number
  extra?: ReactNode
}): JSX.Element {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: spacing.sm,
        marginBottom: spacing.sm
      }}
    >
      {title ? (
        <Space align="baseline" size={spacing.xs}>
          <Typography.Text strong>{title}</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {count}
          </Typography.Text>
        </Space>
      ) : (
        <span />
      )}
      {extra}
    </div>
  )
}

/**
 * 配额总览：把**用户级配额**摊在列表最上面。
 *
 * 为什么需要单独一块：配额是用户级的一份（名下所有库共用），而卡片上的进度条分母是
 * 各库自己的容量 —— 用户此前只能逐个库估算，"我总共用了多少、还剩多少"没有任何地方
 * 能回答（/v1/users/{id} 只对超管开放）。这里是「我的存储库」页面上唯一的总账。
 *
 * 取不到时整块不渲染：老服务端没有 /v1/auth/me，或请求失败时，页面照常用，
 * 不能因为一块统计把整个列表带崩。
 */
function QuotaOverview({
  repos,
  embedded = false
}: {
  repos: RepoDTO[]
  /** 嵌在「用量」大模块里时不再套一层 Card。 */
  embedded?: boolean
}): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const meQuery = useQuery({ queryKey: ['me'], queryFn: () => api.me() })
  const user = meQuery.data
  if (!user) return null

  // quota_bytes = 0 表示不限（只受存储池容量约束），此时没有分母，不画进度条。
  const unlimited = user.quota_bytes <= 0
  const percent = unlimited ? 0 : Math.min(100, Math.round((user.used_bytes / user.quota_bytes) * 100))
  const creating = repos.filter((repo) => repo.state === 'creating').length

  const content = (
    <>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: spacing.xs, flexWrap: 'wrap' }}>
        <Typography.Text strong>{t('repo.quota.title')}</Typography.Text>
        <Typography.Text style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {`${t('repo.quota.used')} ${formatBytes(user.used_bytes)} / ${t('repo.quota.total')} ${
            unlimited ? t('common.unlimited') : formatBytes(user.quota_bytes)
          }`}
        </Typography.Text>
        {/* 不限配额时没有分母，百分比无意义（进度条也不画）。 */}
        {unlimited ? null : (
          <Typography.Text strong style={{ fontSize: fontSize.sm }}>
            {`${percent}%`}
          </Typography.Text>
        )}
      </div>
      {unlimited ? null : (
        <Progress
          percent={percent}
          showInfo={false}
          size="small"
          // 与库卡片同一条分档规则（见 components/UsageBar 的 usageColor）：配额也是"占比"，
          // 同一页上两处条子一套颜色，免得一条橙一条蓝看着像两种口径。
          strokeColor={usageColor(percent)}
          trailColor={palette.neutralFill}
          style={{ marginTop: spacing.xxs, marginBottom: 0 }}
        />
      )}
      <Space size={spacing.md} wrap style={{ marginTop: spacing.sm }}>
        {unlimited ? null : (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.quota.remaining')} {formatBytes(Math.max(0, user.quota_bytes - user.used_bytes))}
          </Typography.Text>
        )}
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.quota.repos')} {repos.length}
        </Typography.Text>
        {creating > 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.quota.creating')} {creating}
          </Typography.Text>
        ) : null}
      </Space>
    </>
  )

  if (embedded) {
    return (
      <div style={{ height: '100%', display: 'flex', flexDirection: 'column', justifyContent: 'center' }}>
        {content}
      </div>
    )
  }

  return (
    <Card styles={{ body: { padding: spacing.md } }} style={{ height: '100%' }}>
      {content}
    </Card>
  )
}

/** 首页右上角的「用量」大模块：存储 + 配额两个子模块合成一张卡片。 */
function UsageOverviewCard({
  onOpenStorages,
  repos
}: {
  onOpenStorages?: () => void
  repos: RepoDTO[]
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Card
      title={t('repo.usage.title')}
      style={{ width: '100%', height: '100%', display: 'flex', flexDirection: 'column' }}
      styles={{
        body: {
          padding: spacing.md,
          flex: 1,
          minHeight: 0,
          display: 'flex',
          flexDirection: 'column',
          gap: spacing.md
        }
      }}
    >
      {onOpenStorages ? (
        <div
          style={{
            flex: 1,
            minHeight: 0,
            display: 'flex',
            flexDirection: 'column',
            justifyContent: 'center',
            paddingBlock: spacing.xs
          }}
        >
          <StorageUsageCard onOpen={onOpenStorages} embedded />
        </div>
      ) : null}
      <div
        style={{
          flex: 1,
          minHeight: 0,
          display: 'flex',
          flexDirection: 'column',
          justifyContent: 'center',
          paddingBlock: spacing.xs
        }}
      >
        <QuotaOverview repos={repos} embedded />
      </div>
    </Card>
  )
}

/** 卡片 / 列表形态切换：只挂在分组标题行右侧（见 RepoGroupHeader 的 extra）。 */
function RepoViewSwitch({
  value,
  onChange
}: {
  value: RepoViewMode
  onChange: (next: RepoViewMode) => void
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Segmented
      value={value}
      onChange={(next) => onChange(next as RepoViewMode)}
      options={[
        {
          value: 'card',
          label: (
            <Space size={spacing.xs}>
              <AppstoreOutlined />
              {t('repo.view.card')}
            </Space>
          )
        },
        {
          value: 'list',
          label: (
            <Space size={spacing.xs}>
              <BarsOutlined />
              {t('repo.view.list')}
            </Space>
          )
        }
      ]}
    />
  )
}

interface RepoSectionProps {
  title: string
  items: RepoListEntry[]
  viewMode: RepoViewMode
  currentUserId: string
  /** 标题行右侧的附加内容（形态切换）；只由列表页交给第一个分组。 */
  headerExtra?: ReactNode
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
  onOpenUpload?: (uploadId: string) => void
}

/**
 * 一个分组区块：标题 + 当前页 + 分页。
 *
 * 分页**每个分组各自一份**：两个分组的含义不同（我拥有的 / 我参与的），
 * 合成一个跨组的分页器时，"第 2 页"读到什么全凭两组各有几个库，很难理解。
 */
function RepoSection({
  title,
  items,
  viewMode,
  currentUserId,
  headerExtra,
  onOpen,
  onConfigure,
  onOpenUpload
}: RepoSectionProps): JSX.Element {
  const [page, setPage] = useState(1)
  const pageSize = PAGE_SIZE[viewMode]
  // 切换形态会换掉每页条数（卡片 8 / 列表 10），当前页可能已经越界；
  // 回到第 1 页最不容易迷失（越界本身另有兜底，见下面的 current）。
  useEffect(() => setPage(1), [viewMode])

  const pageCount = Math.max(1, Math.ceil(items.length / pageSize))
  const current = Math.min(page, pageCount)
  const visible = items.slice((current - 1) * pageSize, current * pageSize)

  return (
    <div>
      {title || headerExtra ? <RepoGroupHeader title={title} count={items.length} extra={headerExtra} /> : null}
      {viewMode === 'card' ? (
        <Row gutter={[spacing.md, spacing.md]}>
          {visible.map(({ repo, upload }) => (
            <Col key={repo.id} xs={24} sm={12} xl={8} xxl={6}>
              <RepoCard
                repo={repo}
                upload={upload}
                onOpenUpload={onOpenUpload}
                currentUserId={currentUserId}
                onOpen={onOpen}
                onConfigure={onConfigure}
              />
            </Col>
          ))}
        </Row>
      ) : (
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          {visible.map(({ repo, upload }) => (
            <RepoRow
              key={repo.id}
              repo={repo}
              upload={upload}
              onOpenUpload={onOpenUpload}
              currentUserId={currentUserId}
              onOpen={onOpen}
              onConfigure={onConfigure}
            />
          ))}
        </Space>
      )}
      {items.length > pageSize ? (
        <div style={{ display: 'flex', justifyContent: 'center', marginTop: spacing.md }}>
          <Pagination
            size="small"
            current={current}
            pageSize={pageSize}
            total={items.length}
            showSizeChanger={false}
            onChange={setPage}
          />
        </div>
      ) : null}
    </div>
  )
}

/** 加载骨架：按当前形态出骨架（卡片网格 / 列表行），避免加载完"跳一下"。 */
function RepoListSkeleton({ viewMode }: { viewMode: RepoViewMode }): JSX.Element {
  if (viewMode === 'card') {
    return (
      <Row gutter={[spacing.md, spacing.md]}>
        {Array.from({ length: 8 }).map((_, index) => (
          <Col key={index} xs={24} sm={12} xl={8} xxl={6}>
            <Card style={{ height: '100%' }} styles={{ body: { padding: spacing.md } }}>
              <Skeleton active paragraph={{ rows: 3 }} />
            </Card>
          </Col>
        ))}
      </Row>
    )
  }
  return (
    <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
      {Array.from({ length: 6 }).map((_, index) => (
        <Card key={index} styles={{ body: { padding: `${spacing.sm}px ${spacing.md}px` } }}>
          <Skeleton active paragraph={{ rows: 1 }} />
        </Card>
      ))}
    </Space>
  )
}

/** 把库按 meta.group 归类；空分组单独归为一组（未分组）。 */
function groupKeyOf(repo: RepoDTO): string {
  return (repo.group ?? '').trim()
}

interface RepoGroupBucket {
  key: string
  title: string
  items: RepoListEntry[]
}

/** 按分组聚合条目，供"总览"页渲染分组卡片；未分组排在最后。 */
function buildGroups(entries: RepoListEntry[], ungroupedLabel: string): RepoGroupBucket[] {
  const map = new Map<string, RepoListEntry[]>()
  for (const entry of entries) {
    const key = groupKeyOf(entry.repo)
    const list = map.get(key)
    if (list) list.push(entry)
    else map.set(key, [entry])
  }
  const buckets = Array.from(map.entries()).map(([key, items]) => ({
    key,
    // 分组名**没有**变化：key 就是服务端 meta.group 原值（空串 = 未分组）。
    title: key || ungroupedLabel,
    items
  }))
  buckets.sort((a, b) => {
    if (a.key === '') return 1
    if (b.key === '') return -1
    return a.key.localeCompare(b.key)
  })
  return buckets
}

/**
 * 分组批量操作的状态放在**模块级**而不是组件内：批量挂载/卸载可能跑几十秒，
 * 用户切到别的页面再回来时组件已经卸载重建，组件内的 busy/progress 会归零 ——
 * 看起来像"操作消失了"，其实后台还在跑。这里按分组 key 保存进度，任何页面重新挂载
 * 都能接着显示，也不会因为重新挂载而重复触发同一批操作。
 */
interface GroupOpState {
  busy: boolean
  /**
   * 操作开始时固定的类型：挂载过程中已挂载数量会一格格增长，
   * 如果按钮继续用 `mountedCount > 0` 判断，就会从"正在挂载"变成"正在卸载"。
   */
  kind: 'mount' | 'unmount' | null
  progress: { done: number; total: number }
}

const GROUP_OP_IDLE: GroupOpState = { busy: false, kind: null, progress: { done: 0, total: 0 } }

/**
 * 分组批量挂载 / 卸载的并发上限。
 *
 * 为什么不"一次全部并发"：代理侧每次挂载/卸载都要起 PowerShell 做 iSCSI 登录、磁盘上线、
 * 盘符或目录分配，几十个库同时开跑会把发起端与磁盘管理接口压满，反而更慢、更容易超时
 * （启动时的自动挂载同样是小并发，见 internal/agent/automount.go 的 autoMountConcurrency）。
 * 取 4：墙钟时间从"逐个串行"压到约 N/4，又能让前几个库的进度立刻反馈到界面上。
 */
const GROUP_OP_CONCURRENCY = 4

/**
 * 以固定并发跑完一批任务。
 *
 * 与逐条串行保持同一套语义：单个任务失败**不中断**整批（成败由 task 自己记账），
 * 全部结束后才 resolve。
 */
async function forEachConcurrent<T>(
  items: T[],
  limit: number,
  task: (item: T) => Promise<void>
): Promise<void> {
  if (items.length === 0) return
  // 游标由所有 worker 共享：取下标与自增之间没有 await，不会被并发打断。
  let cursor = 0
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    for (;;) {
      const index = cursor
      cursor += 1
      if (index >= items.length) return
      await task(items[index])
    }
  })
  await Promise.all(workers)
}

const groupOps = new Map<string, GroupOpState>()
const groupOpListeners = new Set<() => void>()

function setGroupOp(key: string, state: GroupOpState): void {
  groupOps.set(key, state)
  for (const listener of groupOpListeners) listener()
}

function subscribeGroupOps(listener: () => void): () => void {
  groupOpListeners.add(listener)
  return () => {
    groupOpListeners.delete(listener)
  }
}

function useGroupOpState(key: string): GroupOpState {
  return useSyncExternalStore(
    subscribeGroupOps,
    () => groupOps.get(key) ?? GROUP_OP_IDLE,
    () => GROUP_OP_IDLE
  )
}

/**
 * 分组整体挂载 / 卸载：按组内每个库的分配调本机代理（**并发**执行，见 GROUP_OP_CONCURRENCY）。
 *
 * 为什么在客户端做而不是服务端：挂载是"这台机器"的行为（盘符、本地会话、缓存代理都在
 * 本机），服务端只下发目标；分组只是一个标签。因此这里按组遍历，每个库都复用单库挂载那一套
 * 语义（没有分配先建、再交给代理），只是把"逐个等待"换成"最多 N 个同时在跑"。
 */
function useGroupMount(groupKey: string, repos: RepoDTO[], currentUserId: string) {
  const api = useApi()
  const agent = useAgent()
  const { active } = useServerConfig()
  const queryClient = useQueryClient()
  const { t } = useI18n()
  const op = useGroupOpState(groupKey)

  const mountAll = useCallback(async (): Promise<void> => {
    if (groupOps.get(groupKey)?.busy) return
    const targets = repos.filter((repo) => repo.state === 'active' && !isPlaceholderRepoId(repo.id))
    if (targets.length === 0) return
    setGroupOp(groupKey, { busy: true, kind: 'mount', progress: { done: 0, total: targets.length } })
    let ok = 0
    let fail = 0
    // 进度用本地计数：并发下不能再"读全局进度再 +1"，否则几次完成会读到同一个旧值而丢格。
    let done = 0
    await forEachConcurrent(targets, GROUP_OP_CONCURRENCY, async (repo) => {
      try {
        const allocations = await api.listAllocations(repo.id)
        let allocation = allocations.items.find(
          (item) => item.user_id === currentUserId && item.state !== 'released'
        )
        if (!allocation && repo.owner_id === currentUserId) {
          allocation = await api.allocate(repo.id, currentUserId)
        }
        if (!allocation) throw new Error(t('repo.card.notAllocated'))
        await agent.mount({
          allocation_id: allocation.id,
          repo_id: repo.id,
          repo_name: repo.name,
          repo_group: repo.group,
          // 只有「挂载分组」这一入口才允许分组配置覆盖组内库自己的配置。
          use_group_config: true,
          server_key: active?.key,
          server_url: active?.baseUrl
        })
        ok += 1
      } catch {
        fail += 1
      }
      done += 1
      setGroupOp(groupKey, { busy: true, kind: 'mount', progress: { done, total: targets.length } })
    })
    // 并发下每个 mount 各自 probe 一次，快照可能被先发起、后返回的那次覆盖成旧值：
    // 收尾再取一次，保证挂载列表与 N 个库的最终状态一致。
    await agent.refresh()
    await queryClient.invalidateQueries({ queryKey: ['repo-allocations'] })
    await queryClient.invalidateQueries({ queryKey: ['leases'] })
    setGroupOp(groupKey, GROUP_OP_IDLE)
    if (fail > 0) message.warning(t('repo.group.mountPartial', { ok, fail }))
    else message.success(t('repo.group.mountDone', { count: ok }))
  }, [active?.baseUrl, active?.key, agent, api, currentUserId, groupKey, queryClient, repos, t])

  const unmountAll = useCallback(async (): Promise<void> => {
    if (groupOps.get(groupKey)?.busy) return
    const mounted = agent.state?.mounts ?? []
    const targets = repos
      .map((repo) => mountOfRepo(mounted, repo))
      .filter((item): item is NonNullable<typeof item> => Boolean(item))
    if (targets.length === 0) return
    setGroupOp(groupKey, { busy: true, kind: 'unmount', progress: { done: 0, total: targets.length } })
    let ok = 0
    let fail = 0
    let done = 0
    await forEachConcurrent(targets, GROUP_OP_CONCURRENCY, async (item) => {
      try {
        await agent.unmount({ allocation_id: item.allocation_id })
        ok += 1
      } catch {
        fail += 1
      }
      done += 1
      setGroupOp(groupKey, { busy: true, kind: 'unmount', progress: { done, total: targets.length } })
    })
    // 同 mountAll：并发 probe 可能互相覆盖，收尾再取一次最终快照。
    await agent.refresh()
    await queryClient.invalidateQueries({ queryKey: ['repo-allocations'] })
    await queryClient.invalidateQueries({ queryKey: ['leases'] })
    setGroupOp(groupKey, GROUP_OP_IDLE)
    if (fail > 0) message.warning(t('repo.group.unmountPartial', { ok, fail }))
    else message.success(t('repo.group.unmountDone', { count: ok }))
  }, [agent, groupKey, queryClient, repos, t])

  return { busy: op.busy, kind: op.kind, progress: op.progress, mountAll, unmountAll }
}

/** 某条挂载记录是否属于这个库（repo_id 优先，旧记录缺 repo_id 时退回库名匹配）。 */
function mountOfRepo(mounts: AgentMountState[], repo: RepoDTO): AgentMountState | undefined {
  return mounts.find(
    (mount) => mount.repo_id === repo.id || (mount.repo_id === '' && mount.repo_name === repo.name)
  )
}

/** 分组卡片里的一行紧凑指标：标签 + 数值 + 短条，三者横向排布。 */
function GroupMetric({
  label,
  value,
  percent,
  color,
  loading
}: {
  label: string
  value: string
  percent: number
  color?: string
  loading?: boolean
}): JSX.Element {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xxs, flexShrink: 0 }}>
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs, whiteSpace: 'nowrap' }}>
        {label}
      </Typography.Text>
      <Typography.Text
        style={{ fontSize: fontSize.xs, fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap' }}
      >
        {value}
      </Typography.Text>
      {/* 64 → 77（+20%）：分组卡片里这根条之前太短，几乎看不出占比差异 */}
      <UsageBar percent={percent} color={color} loading={loading} style={{ width: 77, marginTop: 0 }} />
    </div>
  )
}

/** 分组总览卡片：组名 + 两个紧凑指标（已挂载 / 差异盘占用）+ 挂载/卸载 + 分组设置。 */
function RepoGroupCard({
  bucket,
  currentUserId,
  onOpen,
  onSettings
}: {
  bucket: RepoGroupBucket
  currentUserId: string
  onOpen: () => void
  onSettings: () => void
}): JSX.Element {
  const { t } = useI18n()
  const api = useApi()
  const agent = useAgent()
  const repos = bucket.items.map((entry) => entry.repo)
  const { busy, kind, progress, mountAll, unmountAll } = useGroupMount(bucket.key, repos, currentUserId)
  const mounts = agent.state?.mounts ?? []
  const mountedRepos = repos.filter((repo) => mountOfRepo(mounts, repo) !== undefined)
  const mountedCount = mountedRepos.length
  const activeCount = repos.filter((repo) => repo.state === 'active').length

  // 「已挂载」报**空间**而不是个数（反馈："已挂载 1/3 展示空间，而不是数量，直接展示已挂载空间大小"）：
  // 这一格要回答的是"我这台机器上现在挂着多大一块盘"，1/3 这种计数看不出体量。
  // 容量取 capacity_bytes（VHDX 标称容量，见 RepoDTO）：挂载后本机那个卷就是这个大小，
  // 与旁边「已使用空间」在同一量纲上，两条条也才能对比。
  const mountedCapacity = mountedRepos.reduce((sum, repo) => sum + (repo.capacity_bytes || 0), 0)
  const totalCapacity = repos.reduce((sum, repo) => sum + (repo.capacity_bytes || 0), 0)

  // 差异盘占用按盘聚合：repo.used_bytes 是"已分配容量"的计费口径，
  // 只有 listRepoDisks 的 diff 盘 physical_bytes 才是真正吃掉的空间。
  // 只有共享库有差异盘（独享库自己就是母盘），独享库与占位库不查。
  const diffQueryEnabled = (repo: RepoDTO): boolean =>
    repo.mode === 'shared' && !isPlaceholderRepoId(repo.id)
  const diskQueries = useQueries({
    queries: repos.map((repo) => ({
      queryKey: ['repo-disks', repo.id],
      queryFn: () => api.listRepoDisks(repo.id),
      enabled: diffQueryEnabled(repo)
    }))
  })
  // **只算"我当前被分配到的那块盘"**（真实反馈："分组已使用空间的上限应当只算用户当前被
  // 分配到的盘的大小"）：同一共享库的每块差异盘属于**一个用户**，把整组 diff 盘无差别加总，
  // 分母会变成「组内差异盘数 × 容量」—— 别人也有一份盘时，上限凭空翻倍，用户对着自己
  // 那块 24.9 GB 的盘看到 49.9 GB 的上限根本对不上。口径与单库卡片 / 详情页完全一致
  // （见 RepoUsageTip 的 useRepoDiffUsage + utils/format 的 userDiffUsage），
  // 所以这里同样要先取分配记录、用它定位我的 disk_id。
  const allocationQueries = useQueries({
    queries: repos.map((repo) => ({
      queryKey: ['repo-allocations', repo.id],
      queryFn: () => api.listAllocations(repo.id),
      enabled: diffQueryEnabled(repo)
    }))
  })
  const diffUsage = useMemo(() => {
    let used = 0
    let total = 0
    repos.forEach((_, index) => {
      const mine = userDiffUsage(
        diskQueries[index]?.data?.items,
        allocationQueries[index]?.data?.items,
        currentUserId
      )
      if (!mine) return
      used += mine.used
      total += mine.total
    })
    return { used, total }
  }, [allocationQueries, currentUserId, diskQueries, repos])
  // 只统计**真正发出去的**查询：被 enabled 关掉的查询在 TanStack v5 里也是 pending，
  // 用 isPending 会让"组里有独享库/上传占位"的分组永远转圈（isLoading = pending && fetching）。
  const diffLoading =
    diskQueries.some((query) => query.isLoading) || allocationQueries.some((query) => query.isLoading)

  // 操作开始后按固定的 kind 出文案：挂载过程中 mountedCount 会增长，
  // 不能再用它判断"这一次到底是在挂载还是卸载"。
  const activeKind = kind ?? (mountedCount > 0 ? 'unmount' : 'mount')
  // 条的比例同样按容量算（分母 = 分组内全部库的容量）：全组都挂上就是 100%。
  const mountedPercent = totalCapacity > 0 ? Math.min(100, Math.round((mountedCapacity / totalCapacity) * 100)) : 0
  const diffPercent = diffUsage.total > 0 ? Math.min(100, Math.round((diffUsage.used / diffUsage.total) * 100)) : 0
  // 组内没有一块"我的盘"（全独享库 / 我在这些共享库上还没分配）时不写 `0 B / 0 B`：
  // 那是"上限未知"，不是"上限为 0"，与用量格写 `-` 的既有口径一致。
  const diffValue =
    diffLoading || diffUsage.total <= 0
      ? '-'
      : `${formatBytes(diffUsage.used)} / ${formatBytes(diffUsage.total)}`

  return (
    <Card
      hoverable
      onClick={onOpen}
      style={{ height: '100%' }}
      styles={{
        body: {
          padding: `${spacing.sm}px ${spacing.md}px`,
          display: 'flex',
          alignItems: 'center',
          gap: spacing.sm,
          height: '100%',
          // 与「列表形态」的库行同高：库行是 16px 标题 + 12px 次要信息（含 4px 间距）
          // 再加大小的上下内边距；分组卡片沿用同一套节奏，切视图时纵向节奏一致。
          minHeight: 48
        }
      }}
    >
      <div
        style={{
          width: 32,
          height: 32,
          borderRadius: 8,
          background: palette.neutralFill,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          flexShrink: 0
        }}
      >
        <FolderOutlined style={{ color: palette.accent, fontSize: 16 }} />
      </div>

      <div style={{ minWidth: 0, flex: '1 1 120px' }}>
        <Typography.Text strong ellipsis style={{ display: 'block', fontSize: fontSize.lg }}>
          {bucket.title}
        </Typography.Text>
        <Typography.Text
          type="secondary"
          ellipsis
          style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}
        >
          {t('repo.group.repoCount', { count: repos.length })} · {t('repo.group.activeCount', { count: activeCount })}
        </Typography.Text>
      </div>

      <GroupMetric
        label={t('repo.group.mountedStorage')}
        // 直接报已挂载的容量（口径见上面的 mountedCapacity），不再报「1 / 3」这种计数：
        // 个数留在悬浮说明里（标题下面的文案已经写了仓库数与可用数），这一行留给体量。
        value={formatBytes(mountedCapacity)}
        percent={mountedPercent}
        // 已挂载容量不是"用量"，不随占比变色：一路主色蓝。
        color={palette.accent}
      />
      <GroupMetric
        label={t('repo.group.usedSpace')}
        value={diffValue}
        percent={diffPercent}
        loading={diffLoading}
      />

      {/* 动作放最右：挂载/卸载一个按钮，设置只留图标、不要边框。 */}
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xxs, flexShrink: 0 }}>
        <Button
          size="small"
          type={activeKind === 'unmount' ? 'default' : 'primary'}
          loading={busy}
          disabled={repos.length === 0 || (activeKind === 'mount' && activeCount === 0)}
          onClick={(event) => {
            event.stopPropagation()
            void (activeKind === 'unmount' ? unmountAll() : mountAll())
          }}
        >
          {busy && progress.total > 0
            ? `${t(activeKind === 'unmount' ? 'repo.group.unmounting' : 'repo.group.mounting')} ${progress.done}/${progress.total}`
            : activeKind === 'unmount'
              ? t('repo.group.unmountAll')
              : t('repo.group.mountAll')}
        </Button>
        {/* 未分组没有分组名，无法承载分组配置；不给设置入口。 */}
        {bucket.key ? (
          <Tooltip title={t('repo.group.settings')}>
            <Button
              type="text"
              size="small"
              icon={<SettingOutlined />}
              aria-label={t('repo.group.settings')}
              onClick={(event) => {
                event.stopPropagation()
                onSettings()
              }}
            />
          </Tooltip>
        ) : null}
      </div>
    </Card>
  )
}

/** 分组下钻后的内容：返回 + 组名 + 批量挂载/卸载 + 分组设置 + 库列表。 */
function RepoGroupDetail({
  bucket,
  currentUserId,
  viewMode,
  onViewModeChange,
  onBack,
  onOpen,
  onConfigure,
  onOpenUpload,
  onSettings
}: {
  bucket: RepoGroupBucket
  currentUserId: string
  viewMode: RepoViewMode
  onViewModeChange: (next: RepoViewMode) => void
  onBack: () => void
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
  onOpenUpload?: (uploadId: string) => void
  onSettings: () => void
}): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  // 防误触：进入分组的那一次点击如果是双击的第二下（或手速很快），会正好落在
  // 刚出现的「返回」按钮上，表现为"一点进去就弹回总览"。这里忽略挂载后极短时间内的返回。
  const mountedAt = useRef(Date.now())
  const repos = bucket.items.map((entry) => entry.repo)
  const { busy, kind, progress, mountAll, unmountAll } = useGroupMount(bucket.key, repos, currentUserId)
  const mounts = agent.state?.mounts ?? []
  const mountedCount = repos.filter((repo) => mountOfRepo(mounts, repo) !== undefined).length
  const activeCount = repos.filter((repo) => repo.state === 'active').length
  // 与总览卡片同一处修复：操作期间固定 kind，避免按钮文案随 mountedCount 翻转。
  const activeKind = kind ?? (mountedCount > 0 ? 'unmount' : 'mount')

  return (
    <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.sm, flexWrap: 'wrap' }}>
        <Button
          icon={<ArrowLeftOutlined />}
          onClick={() => {
            if (Date.now() - mountedAt.current < 500) return
            onBack()
          }}
        >
          {t('repo.group.back')}
        </Button>
        <FolderOutlined style={{ color: palette.accent }} />
        <Typography.Text strong>{bucket.title}</Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {bucket.items.length}
        </Typography.Text>
        <span style={{ marginLeft: 'auto' }}>
          <Space wrap>
            <Button
              type={activeKind === 'unmount' ? 'default' : 'primary'}
              size="small"
              loading={busy}
              disabled={repos.length === 0 || (activeKind === 'mount' && activeCount === 0)}
              onClick={() => void (activeKind === 'unmount' ? unmountAll() : mountAll())}
            >
              {busy && progress.total > 0
                ? `${t(activeKind === 'unmount' ? 'repo.group.unmounting' : 'repo.group.mounting')} ${progress.done}/${progress.total}`
                : activeKind === 'unmount'
                  ? t('repo.group.unmountAll')
                  : t('repo.group.mountAll')}
            </Button>
            {bucket.key ? (
              <Tooltip title={t('repo.group.settings')}>
                <Button
                  type="text"
                  size="small"
                  icon={<SettingOutlined />}
                  aria-label={t('repo.group.settings')}
                  onClick={onSettings}
                />
              </Tooltip>
            ) : null}
            <RepoViewSwitch value={viewMode} onChange={onViewModeChange} />
          </Space>
        </span>
      </div>
      <RepoSection
        title=""
        items={bucket.items}
        viewMode={viewMode}
        currentUserId={currentUserId}
        onOpen={onOpen}
        onConfigure={onConfigure}
        onOpenUpload={onOpenUpload}
      />
    </Space>
  )
}

/** 存储库列表：管理端表格；用户端卡片/列表两种形态（可切换 + 分组分页）。 */
export function RepoList({
  isSuperAdmin,
  onCreate,
  onOpen,
  onOpenUpload,
  onOpenStorages,
  onImport
}: RepoListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { user } = useAuth()
  const agent = useAgent()
  const [keyword, setKeyword] = useState('')
  // 展示形式只影响用户端（管理端是一张运维表，形态切换对它没有意义）。
  // 初值取本地记住的那一份：用户上次选了列表，刷新/重开客户端仍是列表。
  const [viewMode, setViewMode] = useState<RepoViewMode>(readStoredView)
  /** 切换形态：即时生效并记住（下次进来直接是这一种）。 */
  const changeView = (next: RepoViewMode): void => {
    setViewMode(next)
    storeView(next)
  }
  // 挂载配置弹窗只服务当前点开的这一个库（弹窗在列表这一层只保留一份，避免每张卡都挂一个）。
  const [settingsRepo, setSettingsRepo] = useState<RepoDTO | null>(null)
  // 分组挂载设置弹窗（同一次只开一个分组）。
  const [settingsGroup, setSettingsGroup] = useState<string | null>(null)
  // 用户端的分组下钻：null = 分组总览；否则是当前进入的分组 key（空串 = 未分组）。
  // 初值取会话记忆：从详情页返回时能落回原来的分组（见 GROUP_PREF_KEY）。
  const [activeGroup, setActiveGroupState] = useState<string | null>(readStoredGroup)
  const setActiveGroup = useCallback((next: string | null): void => {
    setActiveGroupState(next)
    storeGroup(next)
  }, [])

  // 缓存用量与命中情况会随读写持续变化：页面在时按 10s 轮询一次（离开页面即停）。
  // 管理端是运维表、不展示缓存，跳过轮询。单个轮询器放在这一层：放到卡片里会变成每张卡一个。
  const refreshCache = agent.refreshCache
  useEffect(() => {
    if (!agent.available || isSuperAdmin) return
    void refreshCache()
    const timer = window.setInterval(() => void refreshCache(), 10000)
    return () => window.clearInterval(timer)
  }, [agent.available, isSuperAdmin, refreshCache])

  // 卡片上的「已用」取本机挂载卷的实测值（见 repoUsage）：同样按 10s 轮询，同一个轮询器
  // 也只放这一层。管理端是运维表（看的是服务端口径），不轮询。
  useAgentUsagePolling(!isSuperAdmin)

  const reposQuery = useQuery({
    queryKey: ['repos', isSuperAdmin ? 'all' : 'mine'],
    queryFn: () => api.listRepos({ limit: 200 })
  })

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const ownerName = useMemo(() => {
    const map = new Map<string, string>()
    for (const item of usersQuery.data?.items ?? []) map.set(item.id, item.username)
    return map
  }, [usersQuery.data])

  // 进行中的上传（本机代理的实时快照，含速率与 ETA）：它们就以库的身份混在列表里（见 entries）。
  // 管理端不参与：上传是本机代理的行为，而它看的是服务端全量库。
  const activeUploads = useMemo(
    () => (isSuperAdmin || !onOpenUpload ? [] : agent.uploads.filter(isActiveUpload)),
    [agent.uploads, isSuperAdmin, onOpenUpload]
  )

  // 上传刚收工的那一拍**立刻**补一次：真库记录正是这一刻才在服务端出现的，而列表手里还是旧快照。
  // 不补的话，占位卡先消失、真库要等下一次查询才回来 —— 中间那几秒列表里空着一块，看着像库被
  // 弄丢了。判据用"活跃集合里少了人"：只在上传真正结束时触发一次，不跟着进度事件走（进度事件
  // 每秒都有，跟着它走就等于每秒打一次列表接口）。
  const activeUploadIds = useMemo(
    () => activeUploads.map((upload) => upload.upload_id || upload.local_dir),
    [activeUploads]
  )
  const lastActiveIds = useRef(activeUploadIds)
  const refetchRepos = reposQuery.refetch
  useEffect(() => {
    const finished = lastActiveIds.current.some((id) => !activeUploadIds.includes(id))
    lastActiveIds.current = activeUploadIds
    if (finished) void refetchRepos()
  }, [activeUploadIds, refetchRepos])

  const entries = useMemo<RepoListEntry[]>(() => {
    const items = reposQuery.data?.items ?? []
    const byName = new Map(activeUploads.map((upload) => [upload.repo_name, upload]))

    // 服务端已经有这个库（上传到了建库阶段）：用**服务端记录**承载上传进度，
    // 这样点进去有真 id、状态也以服务端为准 —— 不另造一张同名卡片。
    const merged: RepoListEntry[] = items.map((repo) => {
      const upload = byName.get(repo.name)
      // 仍然是 active 的库不算"上传中"：它此刻能正常挂载使用（这是重传覆盖还没轮到重建
      // 母盘的场景），给它贴上「上传中」只会让人以为库不能用了 —— 这次覆盖上传的进度去
      // 「上传任务」页看（新建页发起后就在那儿）。
      const live = agent.repoEvents[repo.id]
      let next = repo
      if (live) {
        // SSE 的存储库事件：只在"进度类"事件上原地合并。
        // 状态切换（state_changed / moved / move_canceled）不直接改本地快照，而是由下面的
        // effect 触发一次列表刷新 —— 事件可能丢失/滞后，服务端记录永远比旧事件可信。
        if (live.action === 'move_started' || live.action === 'move_progress') {
          next = {
            ...repo,
            state: (live.state as RepoDTO['state'] | undefined) ?? repo.state,
            move: live.move ?? repo.move
          }
        } else if (live.action === 'prepare_progress') {
          next = {
            ...repo,
            state: (live.state as RepoDTO['state'] | undefined) ?? repo.state,
            prepare: live.prepare ?? repo.prepare
          }
        }
      }
      return { repo: next, upload: upload && next.state !== 'active' ? upload : undefined }
    })

    // 服务端还没有的（扫描 / 建会话 / 传数据三个阶段）：合成一条，让它和其他库一样出现在
    // 列表里 —— 发起上传的人必须马上在列表里看到"这块库正在生成"。
    const known = new Set(items.map((repo) => repo.name))
    for (const upload of activeUploads) {
      if (known.has(upload.repo_name)) continue
      merged.push({ repo: uploadPlaceholderRepo(upload, user?.id ?? ''), upload })
    }

    const lowered = keyword.trim().toLowerCase()
    if (!lowered) return merged
    return merged.filter((entry) => entry.repo.name.toLowerCase().includes(lowered))
  }, [reposQuery.data, keyword, activeUploads, user?.id, agent.repoEvents])

  // 状态类事件只触发一次列表刷新（事件本身可能丢/滞后，不直接改本地快照）。
  // 进度类事件（move_progress / prepare_progress）不在这里，靠上面的 entries 合并实时更新。
  const repoStateEventKey = useMemo(
    () =>
      Object.values(agent.repoEvents)
        .filter((event) => event.action === 'state_changed' || event.action === 'moved' || event.action === 'move_canceled')
        .map((event) => `${event.repo_id}:${event.action}:${event.state ?? ''}`)
        .sort()
        .join('|'),
    [agent.repoEvents]
  )
  useEffect(() => {
    if (repoStateEventKey) void refetchRepos()
  }, [repoStateEventKey, refetchRepos])

  // 记忆的分组可能已经被改名/删空：这时落回总览，别停在一个不存在的分组里。
  //
  // 用 entries（含**上传中的占位库**）判断，而不是只看服务端已存在的记录：
  // 未分组的上传占位库同样属于「未分组」这个分组，只看服务端列表会把它误判成
  // "分组不存在"，于是点进未分组又被立刻弹回总览。
  useEffect(() => {
    if (isSuperAdmin || activeGroup === null || reposQuery.isLoading) return
    const exists = entries.some((entry) => groupKeyOf(entry.repo) === activeGroup)
    if (!exists) setActiveGroup(null)
  }, [activeGroup, isSuperAdmin, reposQuery.isLoading, entries, setActiveGroup])

  const columns = useMemo<TableColumnsType<RepoDTO>>(() => {
    const base: TableColumnsType<RepoDTO> = [
      { title: t('field.name'), dataIndex: 'name', key: 'name' },
      { title: t('field.mode'), dataIndex: 'mode', key: 'mode', width: 200, render: (value: string) => repoModeLabel(value) },
      { title: t('field.group'), dataIndex: 'group', key: 'group', render: (value?: string) => value || '-' },
      {
        // 管理端表格看的是**服务端口径**（已分配磁盘总量，分母 = 库容量，见 repoUsage）：
        // 运维要的是"这块库分配出去多大"，与客户端卡片的本机实测值是两回事。
        title: t('repo.used'),
        key: 'size',
        render: (_value, repo) => repoUsage(repo).text
      },
      {
        title: t('common.status'),
        dataIndex: 'state',
        key: 'state',
        width: 110,
        render: (value: string) => <StatusTag group="repo" value={value} />
      },
      {
        title: t('common.createdAt'),
        dataIndex: 'created_at',
        key: 'created_at',
        width: 170,
        render: (value: number) => formatTime(value)
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 100,
        render: (_value, repo) => (
          <Button type="link" size="small" onClick={() => onOpen(repo.id)}>
            {t('repo.detail.title')}
          </Button>
        )
      }
    ]
    if (isSuperAdmin) {
      base.splice(2, 0, {
        title: t('field.owner'),
        dataIndex: 'owner_id',
        key: 'owner_id',
        render: (value: string) => ownerName.get(value) ?? value
      })
    }
    return base
  }, [isSuperAdmin, onOpen, ownerName, t])

  const renderUserRepos = (): JSX.Element => {
    if (reposQuery.isLoading) return <RepoListSkeleton viewMode={viewMode} />

    // 顶部总览：左侧 2/3 性能指标，右侧 1/3 存储 + 配额上下排列。
    // 存储/配额卡沿用原来的整卡点击与文案，只改宽度与位置。
    const overview = (quotaRepos: RepoDTO[]): JSX.Element => (
      <Row gutter={[spacing.md, spacing.md]} style={{ alignItems: 'stretch' }}>
        <Col xs={24} lg={16} style={{ display: 'flex' }}>
          <PerformanceMonitor />
        </Col>
        <Col xs={24} lg={8} style={{ display: 'flex' }}>
          <UsageOverviewCard onOpenStorages={onOpenStorages} repos={quotaRepos} />
        </Col>
      </Row>
    )

    // 上传中的占位库已经算在 entries 里，所以这里不必再单独判一次"有没有在上传"。
    if (entries.length === 0) {
      return (
        <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
          {overview([])}
          <EmptyState
            title={t('repo.empty')}
            description={t('repo.empty.desc')}
            action={{ label: t('common.create'), onClick: onCreate }}
          />
        </Space>
      )
    }

    const userId = user?.id ?? ''
    const groups = buildGroups(entries, t('repo.group.ungrouped'))
    const current = activeGroup === null ? undefined : groups.find((bucket) => bucket.key === activeGroup)

    // 下钻视图：只列当前分组的库。分组被删除/改名（服务端数据变了）时回退到总览。
    if (current) {
      return (
        <RepoGroupDetail
          key={`${current.key}-${keyword}`}
          bucket={current}
          currentUserId={userId}
          viewMode={viewMode}
          onViewModeChange={changeView}
          onBack={() => setActiveGroup(null)}
          onOpen={onOpen}
          onConfigure={setSettingsRepo}
          onOpenUpload={onOpenUpload}
          onSettings={() => setSettingsGroup(current.key)}
        />
      )
    }

    return (
      <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
        {/* 顶部总览：性能指标（左 2/3） + 存储/配额（右 1/3）。 */}
        {overview(entries.filter((entry) => !isPlaceholderRepoId(entry.repo.id)).map((entry) => entry.repo))}
        <div style={{ display: 'flex', alignItems: 'baseline', gap: spacing.xs }}>
          <Typography.Title level={5} style={{ margin: 0 }}>
            {t('repo.group.title')}
          </Typography.Title>
        </div>
        <Row gutter={[spacing.md, spacing.md]}>
          {groups.map((bucket) => (
            <Col
              key={bucket.key || '__ungrouped__'}
              xs={24}
              sm={24}
              xl={groups.length === 1 ? 24 : 12}
              xxl={groups.length === 1 ? 24 : 12}
            >
              <RepoGroupCard
                bucket={bucket}
                currentUserId={userId}
                onOpen={() => setActiveGroup(bucket.key)}
                onSettings={() => setSettingsGroup(bucket.key)}
              />
            </Col>
          ))}
        </Row>
      </Space>
    )
  }

  return (
    <PageShell
      title={t(isSuperAdmin ? 'page.repos.title' : 'page.myRepos.title')}
      extra={
        <>
          {/* 展示形式切换已移入内容区第一个分组标题行右侧（只服务用户端，见 RepoViewSwitch）。 */}
          <Input.Search
            allowClear
            placeholder={t('common.search')}
            style={{ width: 220 }}
            onChange={(event) => setKeyword(event.target.value)}
          />
          <Button onClick={() => void reposQuery.refetch()}>{t('common.refresh')}</Button>
          {/* 导入放在「新建」左边：它是低频的迁移动作，位置上与主操作区分开，
              免得日常建库时手滑点进来。 */}
          {isSuperAdmin && onImport ? <Button onClick={onImport}>{t('repo.import')}</Button> : null}
          <Button type="primary" onClick={onCreate}>
            {t('common.create')}
          </Button>
        </>
      }
    >
      {reposQuery.error ? <ErrorNotice error={reposQuery.error} /> : null}
      {/* 每库独立的挂载配置：卡片「配置」图标打开，只改被点的这一个库 */}
      {settingsRepo ? (
        <RepoMountSettingsModal repo={settingsRepo} open onClose={() => setSettingsRepo(null)} />
      ) : null}
      {/* 分组挂载设置：一次只开一个分组（含"使用分组配置"与作用的库勾选）。 */}
      {settingsGroup !== null ? (
        <GroupMountSettingsModal
          group={settingsGroup}
          // 组内的库：勾选列表默认全选。上传占位条目不是真库，别混进去。
          repos={entries
            .filter((entry) => !isPlaceholderRepoId(entry.repo.id))
            .filter((entry) => (entry.repo.group ?? '').trim() === settingsGroup)
            .map((entry) => entry.repo)}
          open
          onClose={() => setSettingsGroup(null)}
        />
      ) : null}
      {isSuperAdmin ? (
        <DataTable<RepoDTO>
          columns={columns}
          rows={entries.map((entry) => entry.repo)}
          rowKey={(repo) => repo.id}
          loading={reposQuery.isLoading}
          empty={
            <EmptyState
              title={t('repo.empty')}
              description={t('repo.empty.desc')}
              action={{ label: t('common.create'), onClick: onCreate }}
            />
          }
        />
      ) : (
        renderUserRepos()
      )}
    </PageShell>
  )
}
