/**
 * 门户截图用的演示数据。
 *
 * 这些数据只在 demo 页面里使用，不参与正式构建，也不会被任何生产入口引用。
 * 目的很单纯：让真实的界面组件在浏览器里跑起来，截图时能看到完整的存储库、
 * 缓存命中、性能曲线和管理端列表，而不是一排空状态。
 */

const GB = 1024 ** 3
const MB = 1024 ** 2
const TB = 1024 ** 4
const NOW = Date.now()
const NOW_SEC = Math.floor(NOW / 1000)

// 所有姓名、库名、游戏名、容量和曲线都是虚构的演示数据，不对应任何真实用户或资产。
export const demoUser = {
  id: 'u-demo-admin',
  username: 'demo_admin',
  role: 'super_admin' as const,
  enabled: true,
  quota_bytes: 3.91 * TB,
  used_bytes: 499 * GB,
  remark: '演示管理员',
  created_at: NOW - 26 * 24 * 3600 * 1000,
  updated_at: NOW - 2 * 3600 * 1000
}

const users = [
  demoUser,
  { id: 'u-nova', username: 'nova', role: 'user', enabled: true, quota_bytes: 1 * TB, used_bytes: 312 * GB, remark: '美术组', created_at: NOW - 19 * 24 * 3600 * 1000, updated_at: NOW - 6 * 3600 * 1000 },
  { id: 'u-orbit', username: 'orbit', role: 'user', enabled: true, quota_bytes: 2 * TB, used_bytes: 764 * GB, remark: '客户端组', created_at: NOW - 14 * 24 * 3600 * 1000, updated_at: NOW - 3 * 24 * 3600 * 1000 },
  { id: 'u-lumen', username: 'lumen', role: 'user', enabled: true, quota_bytes: 1 * TB, used_bytes: 208 * GB, remark: '测试组', created_at: NOW - 12 * 24 * 3600 * 1000, updated_at: NOW - 2 * 24 * 3600 * 1000 },
  { id: 'u-echo', username: 'echo', role: 'user', enabled: false, quota_bytes: 512 * GB, used_bytes: 0, remark: '已停用', created_at: NOW - 9 * 24 * 3600 * 1000, updated_at: NOW - 5 * 24 * 3600 * 1000 }
]

export const demoStorages = [
  {
    id: 'stor-atlas',
    name: 'AtlasData',
    path: 'D:\\AtlasData',
    enabled: true,
    created_at: NOW - 80 * 24 * 3600 * 1000,
    updated_at: NOW - 12 * 3600 * 1000,
    file_system: 'NTFS',
    free_bytes: 14.5 * TB,
    total_bytes: 25.5 * TB,
    volume_name: 'AtlasData',
    media_type: 'hdd' as const,
    disk_count: 22,
    used_bytes: 11.0 * TB,
    kind: 'dir' as const,
    mounted: true,
    ref: '',
    pool_ref: '',
    size_bytes: 0
  },
  {
    id: 'stor-nova',
    name: 'NovaSSD',
    path: 'E:\\NovaSSD',
    enabled: true,
    created_at: NOW - 46 * 24 * 3600 * 1000,
    updated_at: NOW - 2 * 24 * 3600 * 1000,
    file_system: 'NTFS',
    free_bytes: 2.8 * TB,
    total_bytes: 4.0 * TB,
    volume_name: 'NovaSSD',
    media_type: 'ssd' as const,
    disk_count: 6,
    used_bytes: 1.2 * TB,
    kind: 'dir' as const,
    mounted: true,
    ref: '',
    pool_ref: '',
    size_bytes: 0
  },
  {
    id: 'stor-archive',
    name: 'ArchiveBay',
    path: 'F:\\ArchiveBay',
    enabled: true,
    created_at: NOW - 120 * 24 * 3600 * 1000,
    updated_at: NOW - 9 * 24 * 3600 * 1000,
    file_system: 'NTFS',
    free_bytes: 7.9 * TB,
    total_bytes: 10.0 * TB,
    volume_name: 'ArchiveBay',
    media_type: 'hdd' as const,
    disk_count: 4,
    used_bytes: 2.1 * TB,
    kind: 'dir' as const,
    mounted: true,
    ref: '',
    pool_ref: '',
    size_bytes: 0
  }
]

function repo(input: {
  id: string
  name: string
  group: string
  capacity: number
  diffUsed: number
  parentUsed: number
  shares: number
  createdDaysAgo: number
  parentVersion?: number
  note?: string
  storage?: string
  mounted?: boolean
  drive?: string
  cacheGB?: number
}) {
  const mounted = input.mounted ?? false
  return {
    id: input.id,
    name: input.name,
    mode: 'shared' as const,
    owner_id: demoUser.id,
    parent_disk_id: `disk-${input.id}-parent`,
    parent_version: input.parentVersion ?? 1,
    parent_condition: 'idle' as const,
    share_count: input.shares,
    max_diff_disks: input.shares,
    quota_bytes: 0,
    capacity_bytes: input.capacity,
    parent_used_bytes: input.parentUsed,
    used_bytes: input.capacity,
    state: 'active' as const,
    group: input.group,
    note: input.note ?? '',
    auto_compact: true,
    pool: true,
    storage_id: input.storage === 'ssd' ? 'stor-nova' : 'stor-atlas',
    storage_name: input.storage === 'ssd' ? 'NovaSSD' : 'AtlasData',
    permission: 'manage' as const,
    created_at: NOW - input.createdDaysAgo * 24 * 3600 * 1000,
    updated_at: NOW - 3 * 3600 * 1000,
    // 下面几个字段只给 demo 自己用，界面不会读
    _diffUsed: input.diffUsed,
    _mounted: mounted,
    _drive: input.drive ?? '',
    _cacheGB: input.cacheGB ?? 0
  }
}

export const demoRepos = [
  repo({ id: 'repo-nebula', name: 'NebulaDrift-Shared', group: 'Game', capacity: 512 * GB, diffUsed: 84.2 * GB, parentUsed: 96.4 * GB, shares: 8, createdDaysAgo: 38, parentVersion: 3, note: 'Steam 公共库 · 低峰自动更新', mounted: true, drive: 'G:', cacheGB: 3.38, storage: 'ssd' }),
  repo({ id: 'repo-iron', name: 'IronHarbor-Base', group: 'Game', capacity: 256 * GB, diffUsed: 12.6 * GB, parentUsed: 41.8 * GB, shares: 6, createdDaysAgo: 31, note: 'Steam 游戏基础库', mounted: true, drive: 'H:', cacheGB: 1.12, storage: 'ssd' }),
  repo({ id: 'repo-pixel', name: 'PixelOrchard-Modpack', group: 'Game', capacity: 64 * GB, diffUsed: 3.4 * GB, parentUsed: 8.9 * GB, shares: 12, createdDaysAgo: 22, note: '模组整合包测试库', mounted: false, storage: 'hdd' }),
  repo({ id: 'repo-starfall', name: 'StarfallTactics-Base', group: 'Game', capacity: 320 * GB, diffUsed: 48.7 * GB, parentUsed: 121.5 * GB, shares: 4, createdDaysAgo: 17, parentVersion: 2, note: 'Steam 游戏基础库', mounted: true, drive: 'I:', cacheGB: 5.44, storage: 'ssd' }),
  repo({ id: 'repo-docs', name: 'DocsSuite-Base', group: 'Work', capacity: 64 * GB, diffUsed: 8.9 * GB, parentUsed: 22.7 * GB, shares: 20, createdDaysAgo: 52, note: '文档套件基础镜像', mounted: true, drive: 'J:', cacheGB: 0.86, storage: 'hdd' }),
  repo({ id: 'repo-cad', name: 'CADSuite-Workstation', group: 'Work', capacity: 200 * GB, diffUsed: 22.5 * GB, parentUsed: 73.6 * GB, shares: 5, createdDaysAgo: 44, note: 'CAD 工程库', mounted: false, storage: 'hdd' }),
  repo({ id: 'repo-creative', name: 'CreativeSuite-Base', group: 'Work', capacity: 160 * GB, diffUsed: 31.2 * GB, parentUsed: 58.3 * GB, shares: 6, createdDaysAgo: 35, note: '创意工具基础镜像', mounted: false, storage: 'hdd' }),
  repo({ id: 'repo-os', name: 'OSImage-Regression', group: 'Lab', capacity: 128 * GB, diffUsed: 31.8 * GB, parentUsed: 27.4 * GB, shares: 10, createdDaysAgo: 63, note: '系统镜像回归测试', mounted: true, drive: 'K:', cacheGB: 2.11, storage: 'hdd' }),
  repo({ id: 'repo-linux', name: 'LinuxDesktop-QA', group: 'Lab', capacity: 48 * GB, diffUsed: 5.2 * GB, parentUsed: 6.8 * GB, shares: 8, createdDaysAgo: 12, note: 'Linux 桌面验证', mounted: false, storage: 'hdd' })
]

export const demoUsers = users

/** 每个库一条分配；挂载中的库有真实的挂载记录与本地用量。 */
export const demoAllocations = demoRepos.map((item) => ({
  id: `alloc-${item.id}`,
  repo_id: item.id,
  disk_id: `disk-${item.id}-diff`,
  user_id: demoUser.id,
  state: item._mounted ? 'mounted' : 'allocated',
  created_at: NOW - 6 * 3600 * 1000,
  updated_at: NOW - 20 * 60 * 1000
}))

export function disksOf(repoId: string) {
  const target = demoRepos.find((item) => item.id === repoId)
  if (!target) return []
  const isFirst = target.id === 'repo-nebula'
  const basePath = target.storage_id === 'stor-nova' ? 'E:\\NovaSSD' : 'D:\\AtlasData'
  return [
    {
      id: `disk-${repoId}-parent`,
      repo_id: repoId,
      kind: 'parent' as const,
      vhdx_path: `${basePath}\\${target.name}\\parent.vhdx`,
      parent_version: 0,
      size_bytes: target.capacity_bytes,
      physical_bytes: target.parent_used_bytes,
      vhd_type: 'dynamic',
      state: 'ready' as const,
      mounted: false,
      created_at: target.created_at,
      updated_at: NOW - 3 * 3600 * 1000
    },
    {
      id: `disk-${repoId}-diff`,
      repo_id: repoId,
      kind: 'diff' as const,
      vhdx_path: `${basePath}\\${target.name}\\diff-${isFirst ? 'demo-01' : 'demo'}.vhdx`,
      parent_id: `disk-${repoId}-parent`,
      parent_version: target.parent_version,
      size_bytes: target.capacity_bytes,
      physical_bytes: target._diffUsed,
      vhd_type: 'dynamic',
      state: 'published' as const,
      mounted: target._mounted,
      created_at: target.created_at + 3600 * 1000,
      updated_at: NOW - 20 * 60 * 1000
    }
  ]
}

export const demoMounts = demoRepos
  .filter((item) => item._mounted)
  .map((item) => ({
    repo_id: item.id,
    repo_name: item.name,
    server_key: 'srv-demo',
    server_url: 'https://vault.demo.lan:8443',
    server_name: 'VAULT-DEMO-01',
    allocation_id: `alloc-${item.id}`,
    lease_id: `lease-${item.id}`,
    target_iqn: `iqn.2026-02.demo.vault:${item.id}`,
    portal: '10.20.30.40:3260',
    mount_mode: 'letter' as const,
    mount_path: item._drive,
    disk_number: 3,
    state: 'mounted' as const,
    mounted_at: NOW - (item.id === 'repo-nebula' ? 5.4 * 3600 * 1000 : 2.1 * 3600 * 1000),
    last_heartbeat_at: NOW - 8 * 1000,
    session_active: true,
    session_checked_at: NOW - 5 * 1000,
    cache_enabled: true,
    cache_active: true,
    direct_mode: false
  }))

export const demoUsage = demoRepos
  .filter((item) => item._mounted)
  .map((item) => ({
    allocation_id: `alloc-${item.id}`,
    repo_id: item.id,
    mount_path: item._drive,
    mount_mode: 'letter' as const,
    state: 'mounted' as const,
    total_bytes: item.capacity_bytes,
    free_bytes: item.capacity_bytes - item._diffUsed,
    used_bytes: item._diffUsed,
    file_system: 'NTFS',
    checked_at: NOW - 4 * 1000
  }))

export const demoCacheTargets = demoRepos
  .filter((item) => item._mounted)
  .map((item, index) => ({
    allocation_id: `alloc-${item.id}`,
    target_iqn: `iqn.2026-02.demo.vault:${item.id}`,
    cache_enabled: true,
    l1_used_bytes: Math.round(item._cacheGB * 0.18 * GB),
    l1_limit_bytes: Math.round(512 * GB / 5),
    l1_resident_bytes: Math.round(item._cacheGB * 0.22 * GB),
    l2_used_bytes: Math.round(item._cacheGB * 0.82 * GB),
    l2_limit_bytes: 4 * GB,
    reads: Math.round(item._diffUsed / MB) * 6400,
    request_hits: Math.round((item._diffUsed / MB) * 6380),
    l1_hits: Math.round((item._diffUsed / MB) * 4920),
    partial_hits: 1240,
    l2_hits: Math.round((item._diffUsed / MB) * 1460),
    backend_reads: 1840,
    writes: Math.round(item._diffUsed / MB) * 210,
    hit_rate: 0.998,
    peak_read_bytes_per_sec: (2.15 + index * 0.19) * GB,
    peak_write_bytes_per_sec: (0.18 + index * 0.02) * GB
  }))

export const demoCache = {
  running: true,
  addr: '127.0.0.1:49152',
  l1_limit_bytes: 512 * MB,
  l2_limit_bytes: 4 * GB,
  local_limit_bytes: 40 * GB,
  enabled_repos: demoMounts.length,
  known_repo_bytes: demoRepos.reduce((sum, item) => sum + item.capacity_bytes, 0),
  known_repos: demoRepos.length,
  recommend: {
    l1_bytes: 512 * MB,
    l2_bytes: 4 * GB,
    basis_bytes: demoRepos.reduce((sum, item) => sum + item.capacity_bytes, 0),
    basis_repos: demoRepos.length
  },
  disk_l1_bytes: 0,
  disk_l2_bytes: demoCacheTargets.reduce((sum, item) => sum + item.l2_used_bytes, 0),
  targets: demoCacheTargets,
  files: demoRepos
    .filter((item) => item._mounted)
    .map((item) => ({
      key: item.id,
      active: true,
      allocation_id: `alloc-${item.id}`,
      l1_bytes: 0,
      l2_bytes: Math.round(item._cacheGB * 0.82 * GB),
      l2_index_bytes: 0
    }))
}

export const demoPerformance = (() => {
  const points = Array.from({ length: 61 }, (_, index) => {
    const t = NOW_SEC - (60 - index) * 5
    const readIops = Math.round(1090 + 380 * Math.sin(index / 5.7) + 170 * Math.sin(index / 2.1) + (index === 36 ? 420 : 0))
    const writeIops = Math.round(176 + 62 * Math.sin(index / 4.4 + 0.9) + 26 * Math.sin(index / 1.9))
    const readBps = Math.round(readIops * 256 * 1024 * (1 + 0.08 * Math.sin(index / 3.3)))
    const writeBps = Math.round(writeIops * 128 * 1024 * (1 + 0.1 * Math.sin(index / 2.8)))
    const l1 = 352 * MB + Math.round(28 * MB * Math.sin(index / 6))
    const l2 = 3.42 * GB + Math.round(180 * MB * Math.sin(index / 9))
    return {
      t,
      read_iops: readIops,
      write_iops: writeIops,
      read_bytes_per_sec: readBps,
      write_bytes_per_sec: writeBps,
      hit_rate: 0.9965 + 0.0028 * Math.sin(index / 7),
      random_4k_rate: 0.61 + 0.11 * Math.sin(index / 5.2),
      l1_used_bytes: l1,
      l2_used_bytes: l2,
      cache_used_bytes: l1 + l2
    }
  })
  return { interval_sec: 5, l1_limit_bytes: 512 * MB, l2_limit_bytes: 4 * GB, points }
})()

export const demoCacheHistory = (() => {
  const targets: Record<string, Array<Record<string, number>>> = {}
  for (const [mountIndex, mount] of demoMounts.entries()) {
    let reads = 824_000 + mountIndex * 36_000
    let hits = 822_600 + mountIndex * 35_900
    let writes = 126_000 + mountIndex * 9_200
    let readBytes = 196 * GB + mountIndex * 12 * GB
    let writeBytes = 21 * GB + mountIndex * 2 * GB
    let random4k = 512_000 + mountIndex * 24_000
    const step = 5100 + mountIndex * 470
    targets[mount.allocation_id] = Array.from({ length: 61 }, (_, index) => {
      const t = NOW_SEC - (60 - index) * 5
      const sample = {
        t,
        reads,
        hits,
        writes,
        read_bytes: readBytes,
        write_bytes: writeBytes,
        backend_reads: 3200 + index * 2,
        random_4k: random4k,
        l1_used_bytes: Math.round((86 + mountIndex * 9) * MB + 18 * MB * Math.sin(index / 5 + 1)),
        l2_used_bytes: Math.round((760 + mountIndex * 80) * MB + 140 * MB * Math.sin(index / 8))
      }
      reads += step + (index % 3) * 260
      hits += step - 12 + (index % 3) * 258
      writes += 760 + mountIndex * 50 + (index % 4) * 40
      readBytes += (268 + mountIndex * 14) * MB
      writeBytes += 24 * MB
      random4k += 3200 + (index % 5) * 90
      return sample
    })
  }
  return { interval_sec: 5, targets }
})()

export const demoAgentState = {
  servers: [
    {
      server_key: 'srv-demo',
      primary: true,
      url: 'https://vault.demo.lan:8443',
      instance_id: 'srv-demo',
      name: 'VAULT-DEMO-01',
      connected: true,
      phase: 'connected',
      user: { id: demoUser.id, username: demoUser.username, role: 'super_admin' },
      session: {
        server_key: 'srv-demo',
        server_url: 'https://vault.demo.lan:8443',
        token: 'demo-token',
        expires_at: NOW + 12 * 3600 * 1000,
        user_id: demoUser.id,
        username: demoUser.username,
        role: 'super_admin'
      }
    }
  ],
  primary_key: 'srv-demo',
  server: {
    server_key: 'srv-demo',
    primary: true,
    url: 'https://vault.demo.lan:8443',
    instance_id: 'srv-demo',
    name: 'VAULT-DEMO-01',
    connected: true,
    phase: 'connected'
  },
  user: { id: demoUser.id, username: demoUser.username, role: 'super_admin' },
  mounts: demoMounts,
  last_mounts: Object.fromEntries(demoMounts.map((item) => [item.repo_id, item.mounted_at])),
  auto_mount: true,
  host: {
    iscsi_available: true,
    iscsi_service: 'running',
    iscsi_ready: true,
    checked_at: NOW - 6 * 1000
  },
  update: {
    channel: 'stable',
    current_version: '0.0.0-demo',
    downloading: false,
    received_bytes: 0,
    total_bytes: 0
  },
  session: {
    server_key: 'srv-demo',
    server_url: 'https://vault.demo.lan:8443',
    token: 'demo-token',
    expires_at: NOW + 12 * 3600 * 1000,
    user_id: demoUser.id,
    username: demoUser.username,
    role: 'super_admin'
  }
}

export const demoAgentHealth = {
  ok: true,
  agent_version: '0.0.0-demo',
  platform: 'windows',
  admin: true,
  host: demoAgentState.host,
  servers: demoAgentState.servers,
  primary_key: 'srv-demo',
  expected_version: '0.0.0-demo',
  version_mismatch: false,
  server: {
    connected: true,
    url: 'https://vault.demo.lan:8443',
    name: 'VAULT-DEMO-01'
  }
}

export const demoAgentConfig = {
  auto_mount: true,
  default_mount_mode: 'letter' as const,
  default_mount_dir: 'D:\\VaultMounts',
  default_download_dir: 'D:\\VaultDownloads',
  shortcut_dir: '',
  language: 'zh-CN',
  start_at_login: true,
  update_channel: 'stable',
  server_alias: 'VAULT-DEMO-01',
  auto_login: true,
  auto_update: true,
  cache_l1_bytes: 512 * MB,
  cache_l2_bytes: 4 * GB,
  cache_total_bytes: 40 * GB,
  cache_l2_dir: '',
  repo_mounts: {},
  group_mounts: {}
}

export const demoSystemInfo = {
  server_instance_id: 'srv-demo',
  server_name: 'VAULT-DEMO-01',
  api_version: 1,
  server_version: '0.0.0-demo',
  client_compat: { min: '0.0.0-demo', max: '0.0.0-demo', recommended: '0.0.0-demo' },
  features: ['shared_repo', 'diff_disk', 'cert_login', 'auto_update', 'pool', 'steam'],
  capabilities: {
    platform_kind: 'windows',
    win_target: true,
    iscsi_sessions: true,
    lvm: false,
    lvm_cache: false,
    hyperv: true,
    storage_root: 'D:\\AtlasData',
    storage_roots: ['D:\\AtlasData', 'E:\\NovaSSD', 'F:\\ArchiveBay'],
    file_system: 'NTFS',
    volume_free_bytes: demoStorages.reduce((sum, item) => sum + item.free_bytes, 0),
    volume_total_bytes: demoStorages.reduce((sum, item) => sum + item.total_bytes, 0),
    volumes: [],
    database_driver: 'sqlite'
  }
}

export const demoDepsReport = {
  supported: false,
  ok: true,
  checked_at: new Date(NOW).toISOString(),
  elapsed_ms: 12,
  root: false,
  repair_enabled: false,
  install_enabled: false,
  items: [],
  missing: [],
  fixed: []
}

// ---------- Steam 游戏库（全部为虚构账号、游戏与 AppID） ----------

export const demoSteamAccounts = [
  {
    id: 'steam-demo-01',
    username: 'demo_steam_01',
    display_name: 'Demo Steam 01',
    status: 'active' as const,
    credential_kind: 'password' as const,
    has_credential: true,
    guard_type: 'email' as const,
    has_guard_secret: true,
    session_state: 'idle',
    max_concurrent: 1,
    active_leases: 0,
    last_login_at: NOW - 3 * 3600 * 1000,
    last_checked_at: NOW - 8 * 60 * 1000,
    last_guard_at: NOW - 26 * 3600 * 1000,
    last_error: '',
    last_output: '',
    session_home: 'C:\\VaultDemo\\SteamCMD\\session-01',
    cmd_root: 'C:\\VaultDemo\\SteamCMD',
    created_at: NOW - 42 * 24 * 3600 * 1000,
    updated_at: NOW - 3 * 3600 * 1000
  },
  {
    id: 'steam-demo-02',
    username: 'demo_steam_02',
    display_name: 'Demo Steam 02',
    status: 'active' as const,
    credential_kind: 'refresh_token' as const,
    has_credential: true,
    guard_type: 'none' as const,
    has_guard_secret: false,
    session_state: 'idle',
    max_concurrent: 1,
    active_leases: 0,
    last_login_at: NOW - 9 * 3600 * 1000,
    last_checked_at: NOW - 21 * 60 * 1000,
    last_guard_at: 0,
    last_error: '',
    last_output: '',
    session_home: 'C:\\VaultDemo\\SteamCMD\\session-02',
    cmd_root: 'C:\\VaultDemo\\SteamCMD',
    created_at: NOW - 28 * 24 * 3600 * 1000,
    updated_at: NOW - 9 * 3600 * 1000
  }
]

export const demoSteamGroups = [
  {
    id: 'grp-nebula',
    name: 'Nebula 公共游戏库',
    kind: 'steam' as const,
    description: '热门游戏集中维护，低峰自动更新',
    steam_account_id: 'steam-demo-01',
    target_platform: 'windows' as const,
    auto_update: true,
    update_schedule: '',
    update_window: '03:00-06:00',
    max_concurrent: 1,
    storage_id: 'stor-nova',
    access_mode: 'all' as const,
    member_ids: [],
    game_count: 4,
    created_at: NOW - 38 * 24 * 3600 * 1000,
    updated_at: NOW - 2 * 3600 * 1000
  },
  {
    id: 'grp-qa',
    name: 'QA 游戏验证库',
    kind: 'steam' as const,
    description: '发布前验证与回滚，手动更新',
    steam_account_id: 'steam-demo-02',
    target_platform: 'windows' as const,
    auto_update: false,
    update_schedule: '',
    update_window: '',
    max_concurrent: 1,
    storage_id: 'stor-atlas',
    access_mode: 'selected' as const,
    member_ids: ['u-nova', 'u-orbit'],
    game_count: 2,
    created_at: NOW - 21 * 24 * 3600 * 1000,
    updated_at: NOW - 5 * 3600 * 1000
  }
]

export const demoSteamGames = [
  {
    id: 'game-nebula',
    group_id: 'grp-nebula',
    repo_id: 'repo-nebula',
    app_id: 900001,
    app_name: 'Nebula Drift',
    install_dir: 'NebulaDrift',
    build_id: '18420011',
    available_build_id: '',
    target_platform: 'windows' as const,
    steam_account_id: 'steam-demo-01',
    auto_update: true,
    status: 'ready' as const,
    progress_percent: 0,
    progress_phase: '' as const,
    progress_stage: '',
    progress_bytes_done: 0,
    progress_bytes_total: 0,
    progress_speed: 0,
    can_stop: false,
    last_checked_at: NOW - 8 * 60 * 1000,
    last_updated_at: NOW - 6 * 24 * 3600 * 1000,
    last_error: '',
    created_at: NOW - 38 * 24 * 3600 * 1000,
    updated_at: NOW - 8 * 60 * 1000
  },
  {
    id: 'game-iron',
    group_id: 'grp-nebula',
    repo_id: 'repo-iron',
    app_id: 900002,
    app_name: 'Iron Harbor',
    install_dir: 'IronHarbor',
    build_id: '18419904',
    available_build_id: '18420607',
    target_platform: 'windows' as const,
    steam_account_id: 'steam-demo-01',
    auto_update: true,
    status: 'updating' as const,
    progress_percent: 62.4,
    progress_phase: 'downloading' as const,
    progress_stage: 'downloading',
    progress_bytes_done: 4.7 * GB,
    progress_bytes_total: 7.5 * GB,
    progress_speed: 268 * MB,
    can_stop: true,
    last_checked_at: NOW - 2 * 60 * 1000,
    last_updated_at: NOW - 6 * 24 * 3600 * 1000,
    last_error: '',
    created_at: NOW - 31 * 24 * 3600 * 1000,
    updated_at: NOW - 20 * 1000
  },
  {
    id: 'game-pixel',
    group_id: 'grp-nebula',
    repo_id: 'repo-pixel',
    app_id: 900003,
    app_name: 'Pixel Orchard',
    install_dir: 'PixelOrchard',
    build_id: '18411022',
    available_build_id: '18411880',
    target_platform: 'windows' as const,
    steam_account_id: 'steam-demo-01',
    auto_update: true,
    status: 'paused' as const,
    progress_percent: 38,
    progress_phase: 'downloading' as const,
    progress_stage: 'paused',
    progress_bytes_done: 1.1 * GB,
    progress_bytes_total: 2.9 * GB,
    progress_speed: 0,
    can_stop: true,
    last_checked_at: NOW - 14 * 60 * 1000,
    last_updated_at: NOW - 11 * 24 * 3600 * 1000,
    last_error: '',
    created_at: NOW - 22 * 24 * 3600 * 1000,
    updated_at: NOW - 14 * 60 * 1000
  },
  {
    id: 'game-starfall',
    group_id: 'grp-nebula',
    repo_id: 'repo-starfall',
    app_id: 900004,
    app_name: 'Starfall Tactics',
    install_dir: 'StarfallTactics',
    build_id: '18418841',
    available_build_id: '',
    target_platform: 'windows' as const,
    steam_account_id: 'steam-demo-01',
    auto_update: true,
    status: 'ready' as const,
    progress_percent: 0,
    progress_phase: '' as const,
    progress_stage: '',
    progress_bytes_done: 0,
    progress_bytes_total: 0,
    progress_speed: 0,
    can_stop: false,
    last_checked_at: NOW - 8 * 60 * 1000,
    last_updated_at: NOW - 3 * 24 * 3600 * 1000,
    last_error: '',
    created_at: NOW - 17 * 24 * 3600 * 1000,
    updated_at: NOW - 8 * 60 * 1000
  },
  {
    id: 'game-echo',
    group_id: 'grp-qa',
    repo_id: 'repo-docs',
    app_id: 900005,
    app_name: 'Echo Vanguard',
    install_dir: 'EchoVanguard',
    build_id: '18420155',
    available_build_id: '',
    target_platform: 'windows' as const,
    steam_account_id: 'steam-demo-02',
    auto_update: false,
    status: 'ready' as const,
    progress_percent: 0,
    progress_phase: '' as const,
    progress_stage: '',
    progress_bytes_done: 0,
    progress_bytes_total: 0,
    progress_speed: 0,
    can_stop: false,
    last_checked_at: NOW - 21 * 60 * 1000,
    last_updated_at: NOW - 4 * 24 * 3600 * 1000,
    last_error: '',
    created_at: NOW - 21 * 24 * 3600 * 1000,
    updated_at: NOW - 21 * 60 * 1000
  },
  {
    id: 'game-crimson',
    group_id: 'grp-qa',
    repo_id: 'repo-cad',
    app_id: 900006,
    app_name: 'Crimson Circuit',
    install_dir: 'CrimsonCircuit',
    build_id: '',
    available_build_id: '',
    target_platform: 'windows' as const,
    steam_account_id: 'steam-demo-02',
    auto_update: false,
    status: 'error' as const,
    progress_percent: 0,
    progress_phase: '' as const,
    progress_stage: '',
    progress_bytes_done: 0,
    progress_bytes_total: 0,
    progress_speed: 0,
    can_stop: false,
    last_checked_at: NOW - 33 * 60 * 1000,
    last_updated_at: 0,
    last_error: 'SteamCMD: login required',
    created_at: NOW - 19 * 24 * 3600 * 1000,
    updated_at: NOW - 33 * 60 * 1000
  }
]

const STEAM_ICON_STYLES: Record<number, { a: string; b: string; label: string }> = {
  900001: { a: '#4f7cff', b: '#8f6bff', label: 'ND' },
  900002: { a: '#1e9e8a', b: '#2f6f62', label: 'IH' },
  900003: { a: '#e28b2d', b: '#b24b2f', label: 'PO' },
  900004: { a: '#6f5bd6', b: '#2e2a66', label: 'ST' },
  900005: { a: '#2f9fd0', b: '#1b4f72', label: 'EV' },
  900006: { a: '#c74f6b', b: '#6d2437', label: 'CC' }
}

/** 生成一张纯几何的虚构封面图标，避免截图里出现任何真实游戏素材。 */
export function demoSteamIcon(appID: number): string | null {
  const style = STEAM_ICON_STYLES[appID]
  if (!style) return null
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 96 96">
<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="${style.a}"/><stop offset="1" stop-color="${style.b}"/></linearGradient></defs>
<rect width="96" height="96" rx="20" fill="url(#g)"/>
<circle cx="72" cy="24" r="26" fill="rgba(255,255,255,.12)"/>
<path d="M0 72 Q24 54 48 72 T96 72 V96 H0Z" fill="rgba(0,0,0,.18)"/>
<text x="48" y="57" text-anchor="middle" font-family="Segoe UI,Arial,sans-serif" font-size="28" font-weight="700" fill="#fff" letter-spacing="1">${style.label}</text>
</svg>`
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

export const demoSteamLibrary = {
  available: true,
  steam_root: 'C:\\VaultDemo\\Steam',
  account_name: 'demo_steam_01',
  games: demoSteamGames.map((game) => ({
    app_id: game.app_id,
    name: game.app_name,
    installed: true,
    has_image: true,
    has_icon: true,
    size_bytes: 0
  }))
}
