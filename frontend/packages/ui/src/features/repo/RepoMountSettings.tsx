import { useEffect, useState } from 'react'
import { Alert, Button, Checkbox, Input, Modal, Pagination, Popconfirm, Radio, Space, Switch, Typography, message } from 'antd'

import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { agentApi, selectDirectory, supportsDirectoryPicker } from '../../api/agentClient'
import type { AgentMountMode } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { effectiveMountDir, trimTrailingSeparators } from '../../utils/mountPath'

/** 设置弹窗的宽度：原来的 520 放下分组里的库勾选列表太挤，宽 30%。 */
const SETTINGS_MODAL_WIDTH = 676

/**
 * 「选择存储库」弹窗每页几行。
 *
 * 取 8：一屏能扫完、又不至于翻页太勤。列表高度按它固定（见 GroupReposPicker 的 minHeight），
 * 翻到只有一两行的末页时弹窗不会跟着塌下去。
 */
const REPO_PAGE_SIZE = 8

/** 单个存储库挂载配置的表单状态与保存动作（卡片弹窗与详情页 tab 共用一份）。 */
export interface RepoMountSettingsState {
  mode: AgentMountMode
  dir: string
  autoMount: boolean
  /** 是否为该库启用本地 iSCSI 读缓存代理（默认开启，用户可关闭）。 */
  cacheEnabled: boolean
  /** 是否为该库启用直连模式（跳过本机代理，默认关闭）。 */
  directMode: boolean
  setMode: (mode: AgentMountMode) => void
  setDir: (dir: string) => void
  setAutoMount: (value: boolean) => void
  setCacheEnabled: (value: boolean) => void
  setDirectMode: (value: boolean) => void
  saving: boolean
  error: unknown
  /** 本地代理不可用：控件置灰（配置存在本机，没有代理就落不下去）。 */
  disabled: boolean
  /** 本地代理是否可用（与 disabled 分开：跟随分组时字段置灰，但清缓存这类动作仍可用）。 */
  available: boolean
  /**
   * 该库跟随的分组名（空串 = 不跟随）。
   *
   * 分组勾了「使用分组配置」且勾选了本库时，这里显示的是**分组**的配置并整片置灰：
   * 生效的就是分组那份（代理侧 groupOverride 同样是分组优先），在本库里改不会有效果。
   */
  governedByGroup: string
  /** 保存成功返回 true（弹窗据此决定关不关）。 */
  save: () => Promise<boolean>
  /**
   * 目录模式最终挂载点的提示文案（已按上下文渲染好，空串表示不展示）。
   *
   * 单库：父目录 + 一层与**库名同名**的子目录；分组：父目录下"与各库同名"的子目录说明。
   */
  previewLabel: string
  /**
   * 该库此刻的缓存占用（L1 + L2，字节）；`null` 表示本库在门户上**没有缓存目标**
   * （没挂载 / 走了直连 / 未启用缓存），此时不显示"清空缓存"入口。
   */
  cacheUsedBytes: number | null
  /** 该库分到的缓存上限（L1 + L2，字节）；只有存在缓存目标时才有意义。 */
  cacheLimitBytes: number
  /** 清空缓存请求进行中。 */
  clearing: boolean
  /** 清空该库的缓存内容（两级都清，不断会话）；结果按"清到了没有"如实提示。 */
  clearCache: () => Promise<void>
}

/**
 * 本库的挂载配置：形态 / 目录 / 启动后自动挂载，**每个库各自独立**。
 *
 * 取值优先级：分组配置（该分组勾了「使用分组配置」且勾选了本库）→ 本库存过的配置 →
 * 代理的默认挂载根（最后这层只用来**预填**）。预填不等于隐式继承：用户点一次保存，
 * 这个库就有了自己的显式表态。
 *
 * 跟随分组时整片置灰并显示分组的值：生效的确实是分组那份（代理侧 resolveMode 里分组优先），
 * 显示库自己的旧值会让人以为改了就能生效。
 *
 * 目录模式下，这里填的目录是**父目录**：真正挂载到它下面的**与存储库同名**的子目录
 * （代理侧规则，见 mountDirLeaf）。界面必须把这件事说清楚 —— 否则用户会去填写的目录里
 * 找文件，而文件其实在下一级（真实反馈）。
 */
export function useRepoMountSettings(repoId: string, repoName: string, repoGroup?: string): RepoMountSettingsState {
  const agent = useAgent()
  const { t } = useI18n()
  const config = agent.config
  const saved = config?.repo_mounts?.[repoId]

  // 该库是否跟随分组配置：分组勾了「使用分组配置」，且 use_repos 为空（组内全选）或含本库。
  const groupKey = (repoGroup ?? '').trim()
  const groupPref = groupKey !== '' ? config?.group_mounts?.[groupKey] : undefined
  const groupGoverns =
    Boolean(groupPref?.use_for_repos) &&
    ((groupPref?.use_repos?.length ?? 0) === 0 || (groupPref?.use_repos ?? []).includes(repoId))
  const governedByGroup = groupGoverns ? groupKey : ''
  // 生效值：跟随分组时用分组那份，否则本库配置优先（空串/无条目落到代理默认）。
  const effective = groupGoverns ? groupPref : saved

  const effectiveMode: AgentMountMode = effective?.mount_mode || config?.default_mount_mode || 'letter'
  const effectiveDir = effective?.mount_dir || config?.default_mount_dir || ''
  const effectiveAuto = effective ? effective.auto_mount : Boolean(config?.auto_mount)
  // 缓存**默认开启**：没有该库条目、或条目里没写，一律按开启；只有显式 false 才关闭。
  const effectiveCache = effective?.cache_enabled ?? true
  // 直连模式**默认关闭**：默认所有库都经本机代理（未开缓存也走纯透传，从而可监控）。
  const effectiveDirect = Boolean(effective?.direct_mode)

  const [mode, setMode] = useState<AgentMountMode>(effectiveMode)
  const [dir, setDir] = useState(effectiveDir)
  const [autoMount, setAutoMount] = useState(effectiveAuto)
  const [cacheEnabled, setCacheEnabled] = useState(effectiveCache)
  const [directMode, setDirectMode] = useState(effectiveDirect)
  const [saving, setSaving] = useState(false)
  const [clearing, setClearing] = useState(false)
  const [error, setError] = useState<unknown>(null)

  // 本库此刻在门户上的缓存目标：targets 按**分配**注册，挂载记录给出本库这次的分配，
  // 两者对上才有目标（GET /agent/cache 的说明：未启用/回退直连的库不出现在 targets 里）。
  // 找不到就代表本库没有缓存内容可清 —— 界面不显示清空入口，而不是摆一个点了没反应的按钮。
  const mountAllocationId = (agent.state?.mounts ?? []).find((item) => item.repo_id === repoId)?.allocation_id
  const cacheTarget = mountAllocationId
    ? agent.cache?.targets.find((item) => item.allocation_id === mountAllocationId)
    : undefined
  const cacheUsedBytes = cacheTarget ? cacheTarget.l1_used_bytes + cacheTarget.l2_used_bytes : null
  const cacheLimitBytes = cacheTarget ? cacheTarget.l1_limit_bytes + cacheTarget.l2_limit_bytes : 0

  // 代理配置是异步到齐的（也可能刚保存完被 refresh 覆盖）：生效值一变就重填表单，
  // 否则用户会看到一份"还没加载出配置"的空表单，并把空值当默认值存下去。
  useEffect(() => {
    setMode(effectiveMode)
    setDir(effectiveDir)
    setAutoMount(effectiveAuto)
    setCacheEnabled(effectiveCache)
    setDirectMode(effectiveDirect)
  }, [repoId, effectiveMode, effectiveDir, effectiveAuto, effectiveCache, effectiveDirect])

  // 打开这份设置时主动取一次缓存状态：用量随读写一直在变，而**详情页这一层没有轮询器**
  // （10s 轮询放在库列表那一层，离开列表就停了），只靠进入本页前的快照可能已经过时，
  // 甚至还没有（直接刷新详情页）。取一次就够，本页不轮询（与「客户端设置」同样的做法）。
  const refreshCache = agent.refreshCache
  useEffect(() => {
    if (!agent.available) return
    void refreshCache()
  }, [agent.available, refreshCache])

  // 预览按用户**正在编辑**的父目录算：改了输入框提示立刻跟着变，不用先保存再猜。
  const previewDir = effectiveMountDir(dir.trim() || config?.default_mount_dir || '', repoName)
  const previewLabel = previewDir === '' ? '' : t('repo.settings.dirEffective', { path: previewDir })

  const save = async (): Promise<boolean> => {
    // 跟随分组时本库配置不生效：不允许在这里"保存一份没人用的配置"（按钮已置灰，这里兜底）。
    if (governedByGroup !== '') return false
    setError(null)
    setSaving(true)
    try {
      await agentApi.setRepoMountPref(repoId, {
        mount_mode: mode,
        // 目录一直存着（此后切回目录模式不用重填）；空串 = 用默认挂载根。
        // 存的是**父目录**，真正挂载点由代理在它下面加一层与库名同名的目录。
        mount_dir: dir.trim(),
        auto_mount: autoMount,
        cache_enabled: cacheEnabled,
        direct_mode: directMode
      })
      // 刷新全局配置快照：卡片、详情页读的都是它，否则界面还显示上一次的值。
      await agent.refresh()
      message.success(t('repo.settings.saved'))
      return true
    } catch (err) {
      setError(err)
      return false
    } finally {
      setSaving(false)
    }
  }

  /**
   * 清空本库的缓存内容（L1 + L2 两级），**不**改配置、不断会话：清完门户目标还在，
   * 后续读照常重新填充（见 POST /agent/cache/clear）。
   *
   * 只传 repo_id：目标按分配注册而界面手里只有库 ID，映射由代理按挂载记录自己做。
   */
  const clearCache = async (): Promise<void> => {
    setError(null)
    setClearing(true)
    try {
      const result = await agentApi.clearCache({ repo_id: repoId })
      // 清完用量立刻回落（否则界面还显示清空前的占用，看起来像没生效）。
      await agent.refreshCache()
      // 没清到东西不是失败：本库此刻没有缓存目标（没挂载/走了直连），如实说，不谎报"已清空"。
      if (result.cleared) message.success(t('repo.settings.clearCacheDone'))
      else message.info(t('repo.settings.clearCacheNone'))
    } catch (err) {
      setError(err)
    } finally {
      setClearing(false)
    }
  }

  return {
    mode,
    dir,
    autoMount,
    cacheEnabled,
    directMode,
    setMode,
    setDir,
    setAutoMount,
    setCacheEnabled,
    setDirectMode,
    saving,
    error,
    disabled: !agent.available || governedByGroup !== '',
    available: agent.available,
    governedByGroup,
    save,
    previewLabel,
    cacheUsedBytes,
    cacheLimitBytes,
    clearing,
    clearCache
  }
}

/** 各字段的展示：弹窗与详情页 tab 用同一份，避免两处配置长得不一样。 */
function RepoMountFields({ state }: { state: RepoMountSettingsState }): JSX.Element {
  const { t } = useI18n()
  const { mode, dir, autoMount, cacheEnabled, directMode, disabled, previewLabel } = state

  return (
    <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
      {state.governedByGroup !== '' ? (
        // 跟随分组：生效的是分组那份配置，这里的控件整体置灰（改这里不会生效，如实说清楚）。
        <Alert
          type="info"
          showIcon
          message={t('repo.settings.governedByGroup', { group: state.governedByGroup })}
        />
      ) : disabled ? (
        <Alert type="warning" showIcon message={t('settings.agent.notAvailable')} />
      ) : (
        <Alert type="info" showIcon message={t('repo.settings.defaultsHint')} />
      )}

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.mountMode')}
        </Typography.Text>
        <Radio.Group
          value={mode}
          disabled={disabled}
          onChange={(event) => state.setMode(event.target.value as AgentMountMode)}
        >
          <Radio.Button value="letter">{t('settings.mountMode.letter')}</Radio.Button>
          <Radio.Button value="directory">{t('settings.mountMode.directory')}</Radio.Button>
        </Radio.Group>
      </div>

      {/* 目录只在目录模式下有意义（与挂载对话框一致）：盘符模式由系统分配，没有目录可填 */}
      {mode === 'directory' ? (
        <div>
          <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
            {t('repo.settings.mountDir')}
          </Typography.Text>
          <Space.Compact style={{ width: '100%' }}>
            <Input
              value={dir}
              disabled={disabled}
              readOnly={supportsDirectoryPicker()}
              placeholder={t('agent.mount.dialog.dirHint')}
              onChange={(event) => state.setDir(event.target.value)}
            />
            {supportsDirectoryPicker() ? (
              <Button
                disabled={disabled}
                onClick={() => {
                  void selectDirectory().then((selection) => {
                    if (selection && !selection.canceled && selection.path) state.setDir(selection.path)
                  })
                }}
              >
                {t('sourceDir.browse')}
              </Button>
            ) : null}
          </Space.Compact>
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
            {t('repo.settings.dirHint')}
          </Typography.Text>
          {/* 把"实际挂载到哪"直接算给用户看：目录名会自动多一级，光靠文字说明仍会有人填错 */}
          {previewLabel ? (
            <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
              {previewLabel}
            </Typography.Text>
          ) : null}
        </div>
      ) : null}

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.autoMount')}
        </Typography.Text>
        <Switch checked={autoMount} disabled={disabled} onChange={state.setAutoMount} />
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
          {t('repo.settings.autoMountHint')}
        </Typography.Text>
      </div>

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.cacheEnabled')}
        </Typography.Text>
        {/* 直连模式开着时缓存没有意义（根本不经过代理）：置灰并说明，避免用户白开 */}
        <Switch
          checked={cacheEnabled && !directMode}
          disabled={disabled || directMode}
          onChange={state.setCacheEnabled}
        />
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
          {t('repo.settings.cacheHint')}
        </Typography.Text>
        {/*
          清空缓存的入口：只在**本库此刻真有缓存目标**时才出现（见 state.cacheUsedBytes）。
          没有目标（没挂载 / 走了直连 / 未启用缓存）时本机没有任何本库的缓存内容，
          显示一个点了也清不掉东西的按钮只会让人以为清过了。
          它是动作而不是配置：跟随分组时也照常可用（disabled 含分组锁定，这里只看代理在不在）。
        */}
        {state.cacheUsedBytes !== null ? (
          <Space size={spacing.sm} style={{ marginTop: spacing.sm }} wrap>
            <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
              {t('repo.settings.cacheUsage', {
                used: formatBytes(state.cacheUsedBytes),
                limit: formatBytes(state.cacheLimitBytes)
              })}
            </Typography.Text>
            <Popconfirm
              title={t('repo.settings.clearCacheConfirm')}
              description={t('repo.settings.clearCacheConfirmHint')}
              okText={t('common.confirm')}
              cancelText={t('common.cancel')}
              onConfirm={() => void state.clearCache()}
            >
              {/* 用文字按钮而不是实心按钮：这行是"看用量 + 顺手清一下"的次要动作，
                  实心按钮会把旁边的缓存开关比下去；全站的动作入口也都是 type="link"。 */}
              <Button type="link" size="small" loading={state.clearing} disabled={!state.available}>
                {t('repo.settings.clearCache')}
              </Button>
            </Popconfirm>
          </Space>
        ) : null}
      </div>

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.directMode')}
        </Typography.Text>
        <Switch checked={directMode} disabled={disabled} onChange={state.setDirectMode} />
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
          {t('repo.settings.directHint')}
        </Typography.Text>
      </div>

      {state.error ? <ErrorNotice error={state.error} /> : null}
    </Space>
  )
}

/** 分组挂载设置的状态：与单库设置同一套字段，外加"使用分组配置"与作用的库。 */
export interface GroupMountSettingsState extends RepoMountSettingsState {
  /** 为 true 时，本分组的配置覆盖（勾选的）组内库自己的挂载配置。 */
  useForRepos: boolean
  setUseForRepos: (value: boolean) => void
  /** 勾选遵循分组配置的库 ID；**空数组 = 组内全选**（默认，新加入分组的库也算）。 */
  useRepos: string[]
  setUseRepos: (ids: string[]) => void
  /** 组内库是否全选（空数组即全选）。 */
  allReposSelected: boolean
  /** 切换全选。 */
  toggleAllRepos: (checked: boolean) => void
}

/**
 * 分组挂载设置：同一套"形态 / 目录 / 自动挂载 / 缓存 / 直连"字段，按**分组名**保存。
 *
 * 分组名与存储库分组（meta.group）同名；未分组的库没有分组配置。
 * 「使用分组配置」打开后，还会记下**哪些库**跟随（默认全选，空数组即全选）。
 */
export function useGroupMountSettings(group: string): GroupMountSettingsState {
  const agent = useAgent()
  const { t } = useI18n()
  const config = agent.config
  const saved = config?.group_mounts?.[group]

  const effectiveMode: AgentMountMode = saved?.mount_mode || config?.default_mount_mode || 'letter'
  const effectiveDir = saved?.mount_dir || config?.default_mount_dir || ''
  const effectiveAuto = saved ? saved.auto_mount : Boolean(config?.auto_mount)
  const effectiveCache = saved?.cache_enabled ?? true
  const effectiveDirect = Boolean(saved?.direct_mode)
  const effectiveUseForRepos = Boolean(saved?.use_for_repos)
  // 空 = 全选：不用把当前组的库 ID 全存一遍，将来新加入组的库自动包含在内。
  const savedUseReposKey = (saved?.use_repos ?? []).join('|')

  const [mode, setMode] = useState<AgentMountMode>(effectiveMode)
  const [dir, setDir] = useState(effectiveDir)
  const [autoMount, setAutoMount] = useState(effectiveAuto)
  const [cacheEnabled, setCacheEnabled] = useState(effectiveCache)
  const [directMode, setDirectMode] = useState(effectiveDirect)
  const [useForRepos, setUseForRepos] = useState(effectiveUseForRepos)
  const [useRepos, setUseRepos] = useState<string[]>(saved?.use_repos ?? [])
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    setMode(effectiveMode)
    setDir(effectiveDir)
    setAutoMount(effectiveAuto)
    setCacheEnabled(effectiveCache)
    setDirectMode(effectiveDirect)
    setUseForRepos(effectiveUseForRepos)
  }, [group, effectiveMode, effectiveDir, effectiveAuto, effectiveCache, effectiveDirect, effectiveUseForRepos])

  // 勾选列表按库 ID 存（改库名不会让已勾选的库失效）；配置里换了列表就重填。
  useEffect(() => {
    setUseRepos(savedUseReposKey === '' ? [] : savedUseReposKey.split('|'))
  }, [group, savedUseReposKey])

  const allReposSelected = useRepos.length === 0
  /**
   * 一键全选/取消全选。
   *
   * 全选存空数组（含将来新加入组的库）；取消全选在保存语义里**表达不出来**（空数组 = 全选），
   * 而"一个库都不勾"等价于"这份配置不作用于任何库"，于是直接把「使用分组配置」关掉。
   */
  const toggleAllRepos = (checked: boolean): void => {
    setUseRepos([])
    if (checked) return
    setUseForRepos(false)
    message.info(t('repo.group.useReposNone'))
  }
  // 分组的预览只能是"父目录 + 各库同名子目录"这层说明：一次挂一组，每个库落在自己的目录里。
  const parentRoot = trimTrailingSeparators(dir.trim() || config?.default_mount_dir || '')
  const previewLabel = parentRoot === '' ? '' : t('repo.group.dirEffective', { path: parentRoot })

  const save = async (): Promise<boolean> => {
    setError(null)
    setSaving(true)
    try {
      await agentApi.setGroupMountPref(group, {
        mount_mode: mode,
        mount_dir: dir.trim(),
        auto_mount: autoMount,
        cache_enabled: cacheEnabled,
        direct_mode: directMode,
        use_for_repos: useForRepos,
        // 全选就存空数组（新加入组的库也算上），只有部分勾选才存具体列表。
        // 注意别在这里"猜"：state 已经是配置里的值，组内库没加载出来时也照存不误。
        use_repos: useRepos
      })
      await agent.refresh()
      message.success(t('repo.group.saved'))
      return true
    } catch (err) {
      setError(err)
      return false
    } finally {
      setSaving(false)
    }
  }

  return {
    mode,
    dir,
    autoMount,
    cacheEnabled,
    directMode,
    setMode,
    setDir,
    setAutoMount,
    setCacheEnabled,
    setDirectMode,
    useForRepos,
    setUseForRepos,
    useRepos,
    setUseRepos,
    allReposSelected,
    toggleAllRepos,
    saving,
    error,
    disabled: !agent.available,
    available: agent.available,
    governedByGroup: '',
    save,
    previewLabel,
    // 分组没有"本库此刻的缓存目标"，清空缓存入口不出现。
    cacheUsedBytes: null,
    cacheLimitBytes: 0,
    clearing: false,
    clearCache: async () => undefined
  }
}

/**
 * 分组设置里"哪些库遵循这份配置"。
 *
 * 入口是一个**蓝链按钮**（真实反馈："选择存储库 这个能力应该改成一个蓝链按钮，然后点击之后有
 * 一个分页的列表可以勾选"）：组里的库一多，原地铺开的勾选列表会把弹窗撑得很长，用户得在滚动条
 * 里找库名；链按钮只报一句结论（已选几个 / 全组全选），要改时再开一个**分页**的勾选列表。
 *
 * 保存语义不变：空数组 = 组内全选（含以后加入本组的库），见 useGroupMountSettings。
 * 全选状态下列表里每个库都打勾，也允许逐个取消 —— 取消即从"全选"退化成显式列表；
 * 一个不剩等价于"这份配置不作用于任何库"，交给外层「使用分组配置」开关表达。
 */
function GroupReposPicker({ state, repos }: { state: GroupMountSettingsState; repos: RepoDTO[] }): JSX.Element {
  const { t } = useI18n()
  const [open, setOpen] = useState(false)
  const [page, setPage] = useState(1)

  // 空数组 = 全选：展示上要展开成"当前组的全部库都打勾"。
  const checkedIds = state.allReposSelected ? repos.map((repo) => repo.id) : state.useRepos
  const partial = checkedIds.length > 0 && checkedIds.length < repos.length
  const pageCount = Math.max(1, Math.ceil(repos.length / REPO_PAGE_SIZE))
  // 库被删掉后当前页可能越界：就地夹回最后一页，不额外开 effect 去纠正。
  const current = Math.min(page, pageCount)
  const pageRepos = repos.slice((current - 1) * REPO_PAGE_SIZE, current * REPO_PAGE_SIZE)
  const summary = state.allReposSelected
    ? t('repo.group.useReposSummaryAll', { count: repos.length })
    : t('repo.group.useReposSummary', { count: checkedIds.length, total: repos.length })

  /** 勾 / 取消一个库：全选存空数组（将来新加入组的库自动跟随）。 */
  const toggleRepo = (id: string, checked: boolean): void => {
    const next = checked ? [...checkedIds, id] : checkedIds.filter((repoId) => repoId !== id)
    if (next.length === 0) {
      // 一个都不勾 = 这份配置不作用于任何库（见 toggleAllRepos）。
      state.toggleAllRepos(false)
      return
    }
    state.setUseRepos(next.length === repos.length ? [] : next)
  }

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'baseline', flexWrap: 'wrap', gap: spacing.xs }}>
        <Typography.Text>{t('repo.group.useRepos')}</Typography.Text>
        <Button
          type="link"
          size="small"
          disabled={state.disabled || repos.length === 0}
          onClick={() => setOpen(true)}
        >
          {t('repo.group.useReposPick')}
        </Button>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {summary}
        </Typography.Text>
      </div>
      <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
        {t('repo.group.useReposHint')}
      </Typography.Text>

      <Modal
        open={open}
        title={t('repo.group.useReposModalTitle')}
        width={420}
        centered
        onCancel={() => setOpen(false)}
        footer={
          <Button type="primary" onClick={() => setOpen(false)}>
            {t('common.close')}
          </Button>
        }
      >
        <Checkbox
          checked={state.allReposSelected}
          indeterminate={partial}
          onChange={(event) => state.toggleAllRepos(event.target.checked)}
        >
          {t('repo.group.selectAll')}
        </Checkbox>
        {/* 高度按整页固定：翻到末页只剩一两行时弹窗不会忽高忽低，翻页按钮也不跳。 */}
        <div style={{ marginTop: spacing.sm, minHeight: REPO_PAGE_SIZE * 32 }}>
          {pageRepos.map((repo) => (
            <Checkbox
              key={repo.id}
              checked={checkedIds.includes(repo.id)}
              onChange={(event) => toggleRepo(repo.id, event.target.checked)}
              style={{ display: 'flex', alignItems: 'center', height: 32 }}
            >
              {/* 库名可能很长（带服务端前缀 / 业务名）：单行省略，不把弹窗撑宽。 */}
              <Typography.Text ellipsis style={{ maxWidth: 320 }}>
                {repo.name}
              </Typography.Text>
            </Checkbox>
          ))}
          {repos.length === 0 ? (
            <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
              {t('repo.group.useReposEmpty')}
            </Typography.Text>
          ) : null}
        </div>
        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: spacing.xs }}>
          <Pagination
            size="small"
            current={current}
            pageSize={REPO_PAGE_SIZE}
            total={repos.length}
            showSizeChanger={false}
            onChange={(next) => setPage(next)}
          />
        </div>
      </Modal>
    </div>
  )
}

/** 分组挂载设置弹窗：字段与单库一致，额外提供"使用分组配置"开关与作用的库。 */
export function GroupMountSettingsModal({
  group,
  repos,
  open,
  onClose
}: {
  group: string
  /** 组内的库：用于"哪些库遵循分组配置"的勾选列表（默认全选）。 */
  repos: RepoDTO[]
  open: boolean
  onClose: () => void
}): JSX.Element {
  const { t } = useI18n()
  const state = useGroupMountSettings(group)

  return (
    <Modal
      open={open}
      title={`${t('repo.group.settingsTitle')} · ${group}`}
      width={SETTINGS_MODAL_WIDTH}
      centered
      okText={t('common.save')}
      cancelText={t('common.cancel')}
      confirmLoading={state.saving}
      onCancel={onClose}
      onOk={() => {
        void state.save().then((saved) => {
          if (saved) onClose()
        })
      }}
    >
      <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
        <div>
          <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
            {t('repo.group.useForRepos')}
          </Typography.Text>
          <Switch checked={state.useForRepos} disabled={state.disabled} onChange={state.setUseForRepos} />
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
            {t('repo.group.useForReposHint')}
          </Typography.Text>
        </div>
        {/* 只有真的要用分组配置时，才需要选"作用于哪些库" */}
        {state.useForRepos ? <GroupReposPicker state={state} repos={repos} /> : null}
        <RepoMountFields state={state} />
      </Space>
    </Modal>
  )
}

export interface RepoMountSettingsModalProps {
  repo: RepoDTO
  open: boolean
  onClose: () => void
}

/** 卡片「配置」图标弹出的挂载配置弹窗（只改当前这一个库）。 */
export function RepoMountSettingsModal({ repo, open, onClose }: RepoMountSettingsModalProps): JSX.Element {
  const { t } = useI18n()
  const state = useRepoMountSettings(repo.id, repo.name, repo.group)

  return (
    <Modal
      open={open}
      title={`${t('repo.settings.title')} · ${repo.name}`}
      width={SETTINGS_MODAL_WIDTH}
      centered
      okText={t('common.save')}
      cancelText={t('common.cancel')}
      confirmLoading={state.saving}
      // 跟随分组时这里保存不了（生效的是分组那份配置），按钮一并置灰。
      okButtonProps={{ disabled: state.governedByGroup !== '' }}
      onCancel={onClose}
      onOk={() => {
        // 保存失败不关窗：错误提示就在表单里，用户改完可以直接重试。
        void state.save().then((saved) => {
          if (saved) onClose()
        })
      }}
    >
      <RepoMountFields state={state} />
    </Modal>
  )
}

/**
 * 详情页「设置」tab 的挂载配置：与卡片弹窗同一份数据、同一套控件。
 *
 * 弹窗是"不改库、只改本机怎么挂"的快捷入口，详情页 tab 是同一件事的完整页，
 * 两者不引入第二套语义。
 */
export function RepoMountSettingsPanel({ repo }: { repo: RepoDTO }): JSX.Element {
  const { t } = useI18n()
  const state = useRepoMountSettings(repo.id, repo.name, repo.group)

  return (
    <SectionCard title={t('repo.settings.title')}>
      <RepoMountFields state={state} />
      <Button
        type="primary"
        loading={state.saving}
        disabled={state.disabled}
        style={{ marginTop: spacing.md }}
        onClick={() => void state.save()}
      >
        {t('common.save')}
      </Button>
    </SectionCard>
  )
}
