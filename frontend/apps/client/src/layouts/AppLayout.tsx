import {
  AppstoreOutlined,
  CloudOutlined,
  ClusterOutlined,
  ControlOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  DesktopOutlined,
  DownOutlined,
  FileTextOutlined,
  InfoCircleOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  SettingOutlined,
  TeamOutlined
} from '@ant-design/icons'
import { Alert, Avatar, Button, Dropdown, Layout, Menu, Popover, Space, Tag, Tooltip, Typography } from 'antd'
import type { MenuProps } from 'antd'
import { useState } from 'react'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'

// 侧边栏品牌处展示对外的发布版本（内部构建号只在「关于」里展示）。
import { RELEASE_CHANNEL, RELEASE_VERSION } from '../config'
// 与本地条目比对服务端地址用同一口径：代理回传的 url 与本地 baseUrl 可能差一个尾斜杠。
import { normalizeUrl, rememberManualServerSwitch } from '../store/appStore'

import {
  LayoutModeProvider,
  LoadingState,
  SystemDepsBanner,
  layout,
  palette,
  roleLabel,
  spacing,
  useAgent,
  useAgentEventNotifier,
  useAuth,
  useI18n,
  useServerConfig,
  useServerLatency,
  type Language
} from '@vault/ui'

/** 外壳模式：应用端（面向普通用户）或管理端（仅超级管理员可达）。 */
export type AppShellMode = 'app' | 'admin'

export interface AppLayoutProps {
  mode: AppShellMode
  language: Language
  onLanguageChange: (language: Language) => void
}

/**
 * 切换服务端后、整页重载前的等待（毫秒）。
 *
 * activeKey 的持久化走的是异步 IPC（Electron 下写 %APPDATA%/Vault/config.json），
 * 立刻 `location.reload()` 会读回**旧**的服务端，表现为"切了等于没切"。
 * 300ms 足够一次本地写盘落地，用户也感知不到延迟。
 */
const SERVER_SWITCH_RELOAD_DELAY_MS = 300

/** 侧边栏收起后只露出图标：把菜单图标从 antd 默认的 16px 提到 20px（不改变展开态）。 */
const SIDEBAR_COLLAPSED_CSS = `
.vault-sidebar {
  transition: all 200ms cubic-bezier(0.22, 0.61, 0.36, 1) !important;
}
.vault-sidebar .ant-menu-item .anticon,
.vault-sidebar .ant-menu-submenu-title .anticon {
  transition: font-size 200ms cubic-bezier(0.22, 0.61, 0.36, 1) !important;
}
.vault-sidebar-collapsed .ant-menu-inline-collapsed .ant-menu-item .anticon,
.vault-sidebar-collapsed .ant-menu-inline-collapsed .ant-menu-submenu-title .anticon {
  font-size: 20px !important;
}
`
/** 侧边栏收起状态的本机偏好键：只影响布局，不进客户端配置。 */
const SIDEBAR_COLLAPSED_KEY = 'vault.sidebar.collapsed'

/** 读取侧边栏收起偏好；从未设置过时默认收起。 */
function readSidebarCollapsed(): boolean {
  try {
    const raw = window.localStorage.getItem(SIDEBAR_COLLAPSED_KEY)
    return raw === null ? true : raw === '1'
  } catch {
    return true
  }
}

/** 记住侧边栏收起偏好；写不进去只当没记住，不影响使用。 */
function storeSidebarCollapsed(collapsed: boolean): void {
  try {
    window.localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? '1' : '0')
  } catch {
    // 忽略：偏好没记住不是错误。
  }
}

/** 主界面布局：按 mode 渲染应用端或管理端两套菜单，顶栏元素两模式一致。 */
export function AppLayout({ mode }: AppLayoutProps): JSX.Element {
  const { t } = useI18n()
  const navigate = useNavigate()
  // 代理事件的全局通知出口：整个应用只注册一次（放外壳里）。
  // 之前每个 useAgent 组件都注册一个，同一条事件会弹出 2-3 条重复提示。
  useAgentEventNotifier()
  const location = useLocation()
  const { user, isSuperAdmin, logout } = useAuth()
  // 侧边栏收起后只剩品牌首字母与用户头像首字母，避免用两个不同风格的占位图标。
  const appInitial = (t('app.name').trim().charAt(0) || 'V').toUpperCase()
  const userInitial = (user?.username ?? '').trim().charAt(0).toUpperCase() || '?'
  const { servers, activeKey, active, setActive } = useServerConfig()
  const agent = useAgent()
  // 服务端往返延迟（顶栏指标，每 10 秒自测一次）：见下面 latencyText / latencyColor。
  const latency = useServerLatency(active?.baseUrl)
  // 手动重连进行中（按钮转圈）：只影响按钮，不参与任何状态判定。
  const [serverRetrying, setServerRetrying] = useState(false)
  // 侧边栏收起状态：底部按钮切换，记在本机（下次打开保持）。
  const [collapsed, setCollapsed] = useState(readSidebarCollapsed)
  // 服务器下拉：受控打开状态，底部「管理地址」点击后要能主动收起。
  const [serverMenuOpen, setServerMenuOpen] = useState(false)
  const toggleCollapsed = (): void => {
    setCollapsed((previous) => {
      const next = !previous
      storeSidebarCollapsed(next)
      return next
    })
  }

  // 渠道标签：stable 不展示；beta / rc / alpha 以标签形式跟在版本号后面。
  const releaseTag = RELEASE_CHANNEL && RELEASE_CHANNEL !== 'stable' ? RELEASE_CHANNEL.toUpperCase() : null
  const releaseTagColor = RELEASE_CHANNEL === 'alpha' ? 'red' : RELEASE_CHANNEL === 'beta' ? 'orange' : 'blue'

  const appMenuItems: MenuProps['items'] = [
    { key: '/my-repos', icon: <CloudOutlined />, label: t('nav.myRepos') },
    { key: '/logs', icon: <FileTextOutlined />, label: t('nav.logs') },
    { key: '/settings', icon: <SettingOutlined />, label: t('nav.settings') },
    // 「关于」紧贴「配置」之下：两者都是"这台客户端自己"的事（配置改行为，关于看版本与许可）。
    // 被问"你那边是什么版本"时用户第一眼会往侧边栏找，排在配置下面最顺手。
    { key: '/about', icon: <InfoCircleOutlined />, label: t('nav.about') }
  ]

  const adminMenuItems: MenuProps['items'] = [
    { key: '/admin/users', icon: <TeamOutlined />, label: t('nav.users') },
    { key: '/admin/repos', icon: <AppstoreOutlined />, label: t('nav.repos') },
    { key: '/admin/storages', icon: <DatabaseOutlined />, label: t('nav.storages') },
    { key: '/admin/jobs', icon: <DashboardOutlined />, label: t('nav.jobs') },
    // 审计日志与服务端日志已合并为一页，入口只留「服务日志」，页内用 Tab 切换。
    { key: '/admin/logs', icon: <FileTextOutlined />, label: t('nav.serverLogs') },
    { key: '/admin/system', icon: <ClusterOutlined />, label: t('nav.system') },
    // 管理端也留一个：关于里的版本/许可是登录角色无关的，排障时不该因为"当前在管理端"就找不到。
    // 路径带 /admin 前缀：管理外壳的路由是挂在 /admin 下的，写 '/about' 会跳到应用端那棵树上
    // （菜单高亮与外壳模式都会跟着不对）。
    { key: '/admin/about', icon: <InfoCircleOutlined />, label: t('nav.about') }
  ]

  const menuItems = mode === 'admin' ? adminMenuItems : appMenuItems

  const serverMenu: MenuProps = {
    items: [
      ...servers.map((server) => ({
        key: server.key,
        label: server.serverName || server.baseUrl
      })),
      { type: 'divider' as const },
      {
        key: '__manage__',
        icon: <SettingOutlined />,
        label: t('settings.servers.manageAddress')
      }
    ],
    selectable: true,
    selectedKeys: activeKey ? [activeKey] : [],
    onClick: ({ key }) => {
      if (key === '__manage__') {
        setServerMenuOpen(false)
        navigate('/settings')
        return
      }
      // 点当前这台不用刷新（否则等于无故重载一次）。
      if (key === activeKey) return
      // 手动切换后马上会整页重载；标记本次选择，避免目标服务端没开自动连接时
      // 重载后又被自动连接规则重置。
      rememberManualServerSwitch(key)
      setActive(key)
      // 换服务端后整页重载：hash 路由保留（仍停在当前页面），但 React Query 缓存与页面内部
      // 状态全部重建。只改 activeKey 是不够的——查询缓存键里没有服务端标识，
      // 列表会短暂显示上一台服务端的数据，且已打开的表单还握着旧对象的 ID。
      window.setTimeout(() => window.location.reload(), SERVER_SWITCH_RELOAD_DELAY_MS)
    }
  }

  const userMenu: MenuProps = {
    items: [
      { key: 'identity', label: `${user?.username ?? '-'} · ${roleLabel(user?.role ?? 'user')}`, disabled: true },
      { type: 'divider' as const },
      { key: 'logout', label: t('nav.logout') }
    ],
    onClick: ({ key }) => {
      if (key === 'logout') logout()
    }
  }

  const selectedKey =
    mode === 'admin'
      ? location.pathname
      : location.pathname.startsWith('/repos') || location.pathname.startsWith('/my-repos')
        ? '/my-repos'
        : location.pathname

  // 服务端连接阶段：phase 是代理侧的权威取值（connecting / connected / disconnected），
  // 但**不能让 phase 覆盖掉 connected** —— 旧版代理不带 phase，而心跳恢复连接时会先
  // 把 connected 置真；这里以 connected 为准做一次归一。
  //
  // 取的是**当前活动服务端**那一台：多服务端下每台各发各的 server 事件，compat `server`
  // 只指主服务端，直接用会出现"切到 B 了，顶栏还在看 A 连没连上"。按活动键取不到时
  // 退回 compat（旧版代理 / 客户端刚启动还没拿到 servers[]）。
  //
  // 键对不上时按地址兜底：代理会把"地址键"合并成"实例 ID 键"（反之亦然），本地条目可能还停在
  // 另一种写法上（老配置迁移过来的条目尤其如此），只按 key 找会退回主服务端那一份。
  const server =
    (activeKey
      ? (agent.state?.servers?.find((item) => item.server_key === activeKey) ??
        (active?.baseUrl
          ? agent.state?.servers?.find((item) => normalizeUrl(item.url) === normalizeUrl(active.baseUrl))
          : undefined))
      : undefined) ?? agent.state?.server
  const serverPhase = server?.connected ? 'connected' : server?.phase
  // "连接中"：代理正在连/自动重连。它不再出现在顶栏（顶栏"通没通"由延迟指标说明），
  // 只留在两处判定里：管理端不在此时遮罩（瞬时抖动不该盖死内容区），以及下面的"未连接"。
  const serverConnecting = agent.available && serverPhase === 'connecting'
  // "未连接"只在代理**明确**说"自动重连已放弃"（phase=disconnected）时才算数：这样旧版代理
  // （不带 phase）不会被误判成失败，也就不会挂出一个注定 404 的"重试"按钮。
  const serverExhausted = agent.available && serverPhase === 'disconnected'

  /**
   * 管理端在**确认连不上**服务端时遮罩管理页内容区（顶栏与侧边栏不遮，见下面 render 里遮罩的位置）。
   *
   * 理由：管理端的每个动作（列表、删除、建池…）都要打服务端，断开时点了必然失败；
   * 让用户对着一个注定报错的界面点，只会换回一串网络错误。
   *
   * 为什么**不把"连接中"算进来**：连接中大多是瞬时的（事件流重连、心跳抖动的退避重试），而管理端
   * 的请求是渲染进程**直连 baseUrl** 的，跟代理这条链路的瞬时抖动没关系。为一次瞬时抖动把内容区
   * 盖死，用户读到的是"点一下删除就卡死了"（真实反馈："点删除库会卡在连接中"）。
   *
   * 为什么带 agent.available 前置条件：代理没起来时 server 状态是"未知"（快照为空）而不是
   * "未连接"，而服务端 API 是渲染进程直连 baseUrl 的、可能完全正常——不加这个前提，
   * 代理一异常就会把界面凭空锁死。
   */
  const adminBlocked = mode === 'admin' && agent.available && !server?.connected && !serverConnecting

  // 本机 iSCSI 发起程序未就绪：挂载必定失败在 connect 阶段，而错误里没有可执行信息。
  // 启动时就挂横幅提示（代理探测到状态变化会推 host 事件，横幅随之出现/消失，无需重启）。
  //
  // checked_at === 0 表示**代理还没探测完**：此时 reason 为空，若直接按"未就绪"提示，
  // 会先闪一条原因不明的警告再变成真实原因（探测要跑一次 PowerShell，约 1s）。
  const hostState = agent.state?.host
  const hostWarning = agent.available && hostState && hostState.checked_at !== 0 && !hostState.iscsi_ready ? hostState : undefined
  const hostDescKey =
    hostWarning?.reason === 'module_missing'
      ? 'agent.host.module_missing'
      : hostWarning?.reason === 'probe_failed'
        ? 'agent.host.probe_failed'
        : 'agent.host.service_stopped'
  /**
   * 顶栏延迟指标：数值本身就是状态。
   *
   * 顶栏不再挂"已连接/未连接"标签 —— 那句话既占位置，又说不清"到底有多快"；一个带颜色的
   * 毫秒数把"通没通"和"有多快"合成一件事：测到数字就是通了（绿/黄/红按快慢），
   * 测不到（超时/网络错/非 2xx）就是没通，显示红色的 `- ms`。
   */
  const latencyText = t('agent.latency.value', {
    value: latency.down ? '-' : latency.latencyMs === undefined ? '…' : latency.latencyMs
  })
  // 阈值取"人眼看不出 vs 明显发钝"的分界：100ms 以内算流畅，300ms 以上算迟钝。
  const latencyColor = latency.down
    ? palette.danger
    : latency.latencyMs === undefined
      ? palette.textTertiary
      : latency.latencyMs < 100
        ? palette.success
        : latency.latencyMs < 300
          ? palette.warning
          : palette.danger

  /**
   * 服务器选择栏的悬停提示：实例 ID + 服务地址 + 指标口径。
   *
   * 前两者都是"核对这台是哪台"的稳定标识，但都长（ID 是 srv-<uuid>）：常驻在顶栏上只占地方，
   * 收进悬停提示里，要核对时一悬停就能看到完整值（顶栏上只留能一眼认出的名字）。
   */
  const serverTip = (
    <span style={{ display: 'block' }}>
      <span style={{ fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace' }}>
        {active?.instanceId || '-'}
      </span>
      <br />
      <span>{active?.baseUrl || '-'}</span>
      <br />
      <span style={{ opacity: 0.75 }}>{t('agent.latency.hint')}</span>
    </span>
  )

  // 手动重连：代理侧已不再自动重试（连续失败达上限），只能由用户点这里再试一次；
  // 重试会清零失败计数，代理随后重新获得自动重连机会。
  const handleServerRetry = (): void => {
    setServerRetrying(true)
    void (async () => {
      try {
        // 只重连活动那一台：代理可能同时连着好几台，替别的台重试没有意义。
        await agent.reconnectServer(activeKey ?? undefined)
      } catch {
        // 失败原因已由 useAgent 落进 agent.error（排障面板可见），这里只负责恢复按钮。
      } finally {
        setServerRetrying(false)
      }
    })()
  }

  return (
    <LayoutModeProvider mode={mode}>
      <style>{SIDEBAR_COLLAPSED_CSS}</style>
      <Layout style={{ height: '100vh' }}>
        <Layout.Sider
          width={layout.sidebarWidth}
          collapsedWidth={80}
          collapsed={collapsed}
          trigger={null}
          theme="light"
          className={collapsed ? 'vault-sidebar vault-sidebar-collapsed' : 'vault-sidebar'}
          style={{ borderRight: `1px solid ${palette.border}` }}
        >
          <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
            <div
              style={{
                padding: collapsed ? `${spacing.md}px ${spacing.xs}px` : `${spacing.md}px ${spacing.lg}px ${spacing.sm}px`,
                display: 'flex',
                justifyContent: collapsed ? 'center' : 'flex-start'
              }}
            >
              {collapsed ? (
                <Avatar
                  shape="square"
                  size={36}
                  style={{
                    background: palette.accent,
                    color: palette.textInverse,
                    fontWeight: 600,
                    fontSize: 16
                  }}
                >
                  {appInitial}
                </Avatar>
              ) : (
                <Space direction="vertical" size={0}>
                  <Space align="center" size={6}>
                    <Typography.Text strong style={{ fontSize: 16 }}>
                      {t('app.name')}
                    </Typography.Text>
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        v{RELEASE_VERSION}
                      </Typography.Text>
                      {releaseTag ? (
                        <Tag
                          color={releaseTagColor}
                          style={{
                            marginInlineEnd: 0,
                            fontSize: 10,
                            lineHeight: '16px',
                            paddingInline: 5,
                            fontWeight: 600
                          }}
                        >
                          {releaseTag}
                        </Tag>
                      ) : null}
                    </span>
                  </Space>
                  {mode === 'admin' ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {t('nav.admin')}
                    </Typography.Text>
                  ) : null}
                </Space>
              )}
            </div>
            <div style={{ flex: 1, minHeight: 0, overflow: 'auto' }}>
              <Menu
                mode="inline"
                inlineCollapsed={collapsed}
                selectedKeys={[selectedKey]}
                items={menuItems}
                // 收起后只露出图标：把字号调大一档（20px），否则 14px 图标在窄栏里显得发虚。
                style={{ borderInlineEnd: 'none', fontSize: collapsed ? 20 : 14 }}
                // 每一项都是路由（包括「关于」）：不再有"点了开弹窗、不留高亮"的特例。
                onClick={({ key }) => navigate(key)}
              />
            </div>
            <div
              style={{
                padding: spacing.xs,
                borderTop: `1px solid ${palette.border}`,
                display: 'flex',
                justifyContent: collapsed ? 'center' : 'flex-start'
              }}
            >
              <Tooltip title={t(collapsed ? 'nav.expandSidebar' : 'nav.collapseSidebar')} placement="right">
                <Button
                  type="text"
                  block={!collapsed}
                  style={collapsed ? { width: 40 } : undefined}
                  icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
                  aria-label={t(collapsed ? 'nav.expandSidebar' : 'nav.collapseSidebar')}
                  onClick={toggleCollapsed}
                >
                  {collapsed ? null : t('nav.collapseSidebar')}
                </Button>
              </Tooltip>
            </div>
          </div>
        </Layout.Sider>

        <Layout>
          <Layout.Header
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              borderBottom: `1px solid ${palette.border}`,
              background: palette.bgContainer
            }}
          >
            <Space size={spacing.xs}>
              {/* 悬停信息挂在整个选择器外层：鼠标落在名称、延迟数字或下拉箭头上都展示。 */}
              <Popover
                trigger="hover"
                placement="rightTop"
                mouseEnterDelay={0.3}
                content={serverTip}
              >
                <span style={{ display: 'inline-flex' }}>
                  <Dropdown
                    menu={serverMenu}
                    trigger={['click']}
                    open={serverMenuOpen}
                    onOpenChange={setServerMenuOpen}
                  >
                    <Button type="text">
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                        <span
                          style={{
                            maxWidth: 180,
                            overflow: 'hidden',
                            textOverflow: 'ellipsis',
                            whiteSpace: 'nowrap'
                          }}
                        >
                          {active?.serverName || active?.baseUrl || t('settings.server.noName')}
                        </span>
                        {/* 延迟指标：数字即状态。点击它只作为普通读数，不弹出任何浮层，
                            也不触发服务端下拉（否则会被当成"点 ms 出来 tooltip"）。 */}
                        <span
                          onClick={(event) => event.stopPropagation()}
                          style={{ color: latencyColor, fontVariantNumeric: 'tabular-nums' }}
                        >
                          {latencyText}
                        </span>
                        <DownOutlined style={{ fontSize: 10 }} />
                      </span>
                    </Button>
                  </Dropdown>
                </span>
              </Popover>
              {serverExhausted ? (
                <Button size="small" type="link" loading={serverRetrying} onClick={handleServerRetry}>
                  {t('agent.server.retry')}
                </Button>
              ) : null}
            </Space>

            <Space size={spacing.xs}>
              {isSuperAdmin ? (
                <Button
                  type="text"
                  icon={mode === 'admin' ? <DesktopOutlined /> : <ControlOutlined />}
                  onClick={() => navigate(mode === 'admin' ? '/my-repos' : '/admin/users')}
                >
                  {mode === 'admin' ? t('nav.switchToApp') : t('nav.switchToAdmin')}
                </Button>
              ) : null}
              <Dropdown menu={userMenu} trigger={['click']}>
                <Button type="text">
                  <Space size={6}>
                    <Avatar
                      size="small"
                      style={{ backgroundColor: palette.textTertiary, fontSize: 12, fontWeight: 600 }}
                    >
                      {userInitial}
                    </Avatar>
                    <span>{user?.username ?? '-'}</span>
                  </Space>
                </Button>
              </Dropdown>
            </Space>
          </Layout.Header>

          {/* 内容区容器：连接遮罩只盖这一块（见 adminBlocked），顶栏与侧边栏不盖。
              理由：顶栏上有"服务端切换 / 延迟指标 / 重试 / 退出登录"，全屏遮住之后一旦卡在
              "连接中"，用户连退出登录都点不到，只能去杀进程。 */}
          <div style={{ position: 'relative', display: 'flex', flex: 1, minHeight: 0 }}>
            <Layout.Content style={{ flex: 1, minWidth: 0, background: palette.bgPage, overflow: 'auto' }}>
              {/* 服务端系统依赖缺失（缺 configfs 挂载 / LIO 内核模块 / targetcli 等）：
                  启动时能自动修的已修，修不了的在这里告诉使用者"缺了什么、怎么补"。
                  能装包的项再给个"安装"按钮——但那个接口只有超级管理员能调，
                  非管理员渲染出来只会点了拿 403，所以按角色 gate。 */}
              <SystemDepsBanner canInstall={isSuperAdmin} />
              {hostWarning ? (
                <Alert
                  type="warning"
                  showIcon
                  style={{ margin: `${spacing.md}px ${spacing.md}px 0` }}
                  message={t('agent.host.title')}
                  description={t(hostDescKey, { error: hostWarning.error ?? '' })}
                />
              ) : null}
              <Outlet />
            </Layout.Content>
            {adminBlocked ? (
              <div
                style={{
                  position: 'absolute',
                  inset: 0,
                  // 必须高过 antd 的浮层（Modal/message/notification 都在 1000~1050）：
                  // 断开时若正好开着某个弹窗，它的 portal 挂在 body 末尾、会浮在低 z-index 的遮罩之上，
                  // 用户就能继续点一个后端已经不可达的表单。
                  zIndex: 1200,
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  background: palette.bgPage
                }}
              >
                <LoadingState text={t('agent.gate.hint')} />
              </div>
            ) : null}
          </div>
        </Layout>
      </Layout>
    </LayoutModeProvider>
  )
}
