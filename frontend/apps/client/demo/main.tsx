import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App as AntApp, ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes, useParams } from 'react-router-dom'

import {
  ApiProvider,
  AuthProvider,
  PerformancePage,
  RepoDetail,
  RepoList,
  ServerConfigProvider,
  StorageList,
  StorageUsagePage,
  UserList,
  createVaultTheme,
  type ServerConfigValue
} from '@vault/ui'

import { AppLayout } from '../src/layouts/AppLayout'
import { demoUser, demoAgentState } from './mockData'
import { demoApi, installDemoRuntime } from './mockRuntime'

installDemoRuntime()

const params = new URLSearchParams(window.location.search)
const screen = params.get('screen') ?? 'overview'

// RepoList 的分组记忆走 sessionStorage：这里按截图场景预先选好分组，
// 页面本身不用为了 demo 改动任何逻辑。
if (screen === 'repos') {
  window.sessionStorage.setItem('vault.repo.group', 'Game')
} else {
  window.sessionStorage.removeItem('vault.repo.group')
}

// 截图里让侧边栏收起，只保留图标，页面更聚焦，也不会把构建号带进图里。
window.localStorage.setItem('vault.sidebar.collapsed', '1')

const initialPath =
  screen === 'detail' || screen === 'disks'
    ? '/repos/repo-steam'
    : screen === 'admin'
      ? '/admin/storages'
      : screen === 'users'
        ? '/admin/users'
        : screen === 'storages'
          ? '/storages'
          : screen === 'performance'
            ? '/performance'
            : '/my-repos'

const mode = screen === 'admin' || screen === 'users' ? 'admin' : 'app'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: false, refetchOnWindowFocus: false, staleTime: 5 * 60 * 1000 }
  }
})

const serverConfig: ServerConfigValue = {
  servers: [
    {
      key: 'srv-vault',
      baseUrl: 'http://127.0.0.1:5173',
      serverName: 'WIN-E095H11JSS',
      instanceId: 'srv-vault',
      autoConnect: true,
      token: 'demo-token',
      tokenExpiresAt: Date.now() + 12 * 3600 * 1000,
      userId: demoUser.id,
      username: demoUser.username,
      role: 'super_admin'
    }
  ],
  activeKey: 'srv-vault',
  active: {
    key: 'srv-vault',
    baseUrl: 'http://127.0.0.1:5173',
    serverName: 'WIN-E095H11JSS',
    instanceId: 'srv-vault',
    autoConnect: true,
    token: 'demo-token',
    tokenExpiresAt: Date.now() + 12 * 3600 * 1000,
    userId: demoUser.id,
    username: demoUser.username,
    role: 'super_admin'
  },
  setActive: () => undefined,
  upsert: () => undefined,
  addServer: () => undefined,
  updateServer: () => undefined,
  remove: () => undefined,
  updateActive: () => undefined
}

function DemoRepoDetail(): JSX.Element {
  const { repoId } = useParams<{ repoId: string }>()
  return (
    <RepoDetail
      repoId={repoId ?? 'repo-steam'}
      isSuperAdmin
      currentUserId={demoUser.id}
      currentUserName={demoUser.username}
      initialTab={screen === 'disks' ? 'disks' : undefined}
      onBack={() => undefined}
    />
  )
}

function DemoApp(): JSX.Element {
  return (
    <ConfigProvider theme={createVaultTheme()} locale={zhCN}>
      <AntApp>
        <QueryClientProvider client={queryClient}>
          <ApiProvider api={demoApi}>
            <AuthProvider
              value={{
                user: { id: demoUser.id, username: demoUser.username, role: 'super_admin' },
                isAuthenticated: true,
                isSuperAdmin: true,
                logout: () => undefined
              }}
            >
              <ServerConfigProvider value={serverConfig}>
                <MemoryRouter initialEntries={[initialPath]}>
                  <Routes>
                  <Route
                    element={
                      <AppLayout
                        mode={mode}
                        language="zh-CN"
                        onLanguageChange={() => undefined}
                      />
                    }
                  >
                    <Route
                      path="/my-repos"
                      element={
                        <RepoList
                          isSuperAdmin={false}
                          onCreate={() => undefined}
                          onOpen={() => undefined}
                          onOpenSettings={() => undefined}
                          onOpenPerformance={() => undefined}
                          onOpenUpload={() => undefined}
                          onOpenStorages={() => undefined}
                          onOpenUsers={() => undefined}
                        />
                      }
                    />
                    <Route path="/repos/:repoId" element={<DemoRepoDetail />} />
                    <Route path="/performance" element={<PerformancePage onBack={() => undefined} />} />
                    <Route
                      path="/storages"
                      element={<StorageUsagePage onBack={() => undefined} onManage={() => undefined} />}
                    />
                    <Route path="/admin/storages" element={<StorageList onCreate={() => undefined} />} />
                    <Route path="/admin/users" element={<UserList />} />
                  </Route>
                  </Routes>
                </MemoryRouter>
              </ServerConfigProvider>
            </AuthProvider>
          </ApiProvider>
        </QueryClientProvider>
      </AntApp>
    </ConfigProvider>
  )
}

// 演示页需要一个稳定的会话名：顶栏的用户入口读的是 AuthProvider，服务端状态读的是
// 被替换过的 agentApi。这里只把顶栏名字对齐，避免截图里出现两个不同用户。
console.info('[vault demo]', {
  screen,
  server: demoAgentState.server.name,
  repos: 9,
  mounted: demoAgentState.mounts.length
})

const root = document.getElementById('root')
if (!root) throw new Error('#root not found')
createRoot(root).render(
  <StrictMode>
    <DemoApp />
  </StrictMode>
)
