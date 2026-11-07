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

export const demoUser = {
  id: 'u-zhuzhu',
  username: 'zhuzhu',
  role: 'super_admin' as const,
  enabled: true,
  quota_bytes: 3.91 * TB,
  used_bytes: 499 * GB,
  remark: '实验室共享盘',
  created_at: NOW - 26 * 24 * 3600 * 1000,
  updated_at: NOW - 2 * 3600 * 1000
}

const users = [
  demoUser,
  { id: 'u-linyu', username: 'linyu', role: 'user', enabled: true, quota_bytes: 1 * TB, used_bytes: 312 * GB, remark: '', created_at: NOW - 19 * 24 * 3600 * 1000, updated_at: NOW - 6 * 3600 * 1000 },
  { id: 'u-chenxi', username: 'chenxi', role: 'user', enabled: true, quota_bytes: 2 * TB, used_bytes: 764 * GB, remark: '美术组', created_at: NOW - 14 * 24 * 3600 * 1000, updated_at: NOW - 3 * 24 * 3600 * 1000 },
  { id: 'u-wangqi', username: 'wangqi', role: 'user', enabled: true, quota_bytes: 1 * TB, used_bytes: 208 * GB, remark: '', created_at: NOW - 12 * 24 * 3600 * 1000, updated_at: NOW - 2 * 24 * 3600 * 1000 },
  { id: 'u-heyan', username: 'heyan', role: 'user', enabled: false, quota_bytes: 512 * GB, used_bytes: 0, remark: '已停用', created_at: NOW - 9 * 24 * 3600 * 1000, updated_at: NOW - 5 * 24 * 3600 * 1000 }
]

export const demoStorages = [
  {
    id: 'stor-vaultdata',
    name: 'VaultData',
    path: 'D:\\VaultData',
    enabled: true,
    created_at: NOW - 80 * 24 * 3600 * 1000,
    updated_at: NOW - 12 * 3600 * 1000,
    file_system: 'NTFS',
    free_bytes: 14.5 * TB,
    total_bytes: 25.5 * TB,
    volume_name: 'VaultData',
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
    id: 'stor-ssd',
    name: 'VaultSSD',
    path: 'E:\\VaultSSD',
    enabled: true,
    created_at: NOW - 46 * 24 * 3600 * 1000,
    updated_at: NOW - 2 * 24 * 3600 * 1000,
    file_system: 'NTFS',
    free_bytes: 2.8 * TB,
    total_bytes: 4.0 * TB,
    volume_name: 'VaultSSD',
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
    name: 'Archive',
    path: 'F:\\VaultArchive',
    enabled: true,
    created_at: NOW - 120 * 24 * 3600 * 1000,
    updated_at: NOW - 9 * 24 * 3600 * 1000,
    file_system: 'NTFS',
    free_bytes: 7.9 * TB,
    total_bytes: 10.0 * TB,
    volume_name: 'Archive',
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
    storage_id: input.storage === 'ssd' ? 'stor-ssd' : 'stor-vaultdata',
    storage_name: input.storage === 'ssd' ? 'VaultSSD' : 'VaultData',
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
  repo({ id: 'repo-steam', name: 'SteamLibrary-Shared', group: 'Game', capacity: 512 * GB, diffUsed: 84.2 * GB, parentUsed: 96.4 * GB, shares: 8, createdDaysAgo: 38, parentVersion: 3, note: 'Steam 公共库，含 CS2 / Dota2 / PUBG', mounted: true, drive: 'G:', cacheGB: 3.38 }),
  repo({ id: 'repo-epic', name: 'EpicGames-Base', group: 'Game', capacity: 256 * GB, diffUsed: 12.6 * GB, parentUsed: 41.8 * GB, shares: 6, createdDaysAgo: 31, note: 'Epic 启动器与常用游戏', mounted: true, drive: 'H:', cacheGB: 1.12 }),
  repo({ id: 'repo-minecraft', name: 'Minecraft-Modpack', group: 'Game', capacity: 64 * GB, diffUsed: 3.4 * GB, parentUsed: 8.9 * GB, shares: 12, createdDaysAgo: 22, note: '整合包测试库', mounted: false }),
  repo({ id: 'repo-unreal', name: 'UnrealEditor-5.4', group: 'Game', capacity: 320 * GB, diffUsed: 48.7 * GB, parentUsed: 121.5 * GB, shares: 4, createdDaysAgo: 17, parentVersion: 2, note: 'UE 5.4 + 示例工程', mounted: true, drive: 'I:', cacheGB: 5.44 }),
  repo({ id: 'repo-office', name: 'Office2021-Base', group: 'Office', capacity: 64 * GB, diffUsed: 8.9 * GB, parentUsed: 22.7 * GB, shares: 20, createdDaysAgo: 52, note: '办公软件基础镜像', mounted: true, drive: 'J:', cacheGB: 0.86 }),
  repo({ id: 'repo-cad', name: 'CAD-Workstation', group: 'Office', capacity: 200 * GB, diffUsed: 22.5 * GB, parentUsed: 73.6 * GB, shares: 5, createdDaysAgo: 44, note: 'AutoCAD + SolidWorks', mounted: false }),
  repo({ id: 'repo-adobe', name: 'Adobe-CC', group: 'Office', capacity: 160 * GB, diffUsed: 31.2 * GB, parentUsed: 58.3 * GB, shares: 6, createdDaysAgo: 35, note: 'PS / PR / AE 2024', mounted: false }),
  repo({ id: 'repo-win11', name: 'Win11-24H2', group: 'Test', capacity: 128 * GB, diffUsed: 31.8 * GB, parentUsed: 27.4 * GB, shares: 10, createdDaysAgo: 63, note: '系统镜像回归测试', mounted: true, drive: 'K:', cacheGB: 2.11 }),
  repo({ id: 'repo-ubuntu', name: 'Ubuntu-24.04', group: 'Test', capacity: 48 * GB, diffUsed: 5.2 * GB, parentUsed: 6.8 * GB, shares: 8, createdDaysAgo: 12, note: 'Linux 桌面验证', mounted: false })
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
  const isFirst = target.id === 'repo-steam'
  return [
    {
      id: `disk-${repoId}-parent`,
      repo_id: repoId,
      kind: 'parent' as const,
      vhdx_path: `D:\\VaultData\\${target.name}\\parent.vhdx`,
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
      vhdx_path: `D:\\VaultData\\${target.name}\\diff-${isFirst ? 'zhuzhu-01' : 'zhuzhu'}.vhdx`,
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
    server_key: 'srv-vault',
    server_url: 'https://vault.example.lan:8443',
    server_name: 'WIN-E095H11JSS',
    allocation_id: `alloc-${item.id}`,
    lease_id: `lease-${item.id}`,
    target_iqn: `iqn.2026-02.lan.vault:${item.id}`,
    portal: '10.10.20.8:3260',
    mount_mode: 'letter' as const,
    mount_path: item._drive,
    disk_number: 3,
    state: 'mounted' as const,
    mounted_at: NOW - (item.id === 'repo-steam' ? 5.4 * 3600 * 1000 : 2.1 * 3600 * 1000),
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
    target_iqn: `iqn.2026-02.lan.vault:${item.id}`,
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
  enabled_repos: demoMounts.length,
  known_repo_bytes: demoRepos.reduce((sum, item) => sum + item.capacity_bytes, 0),
  known_repos: demoRepos.length,
  recommend: {
    l1_bytes: 512 * MB,
    l2_bytes: 4 * GB,
    basis_bytes: demoRepos.reduce((sum, item) => sum + item.capacity_bytes, 0),
    basis_repos: demoRepos.length
  },
  targets: demoCacheTargets
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
      server_key: 'srv-vault',
      primary: true,
      url: 'https://vault.example.lan:8443',
      instance_id: 'srv-vault',
      name: 'WIN-E095H11JSS',
      connected: true,
      phase: 'connected',
      user: { id: demoUser.id, username: demoUser.username, role: 'super_admin' },
      session: {
        server_key: 'srv-vault',
        server_url: 'https://vault.example.lan:8443',
        token: 'demo-token',
        expires_at: NOW + 12 * 3600 * 1000,
        user_id: demoUser.id,
        username: demoUser.username,
        role: 'super_admin'
      }
    }
  ],
  primary_key: 'srv-vault',
  server: {
    server_key: 'srv-vault',
    primary: true,
    url: 'https://vault.example.lan:8443',
    instance_id: 'srv-vault',
    name: 'WIN-E095H11JSS',
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
    channel: 'beta',
    current_version: '0.2.0',
    downloading: false,
    received_bytes: 0,
    total_bytes: 0
  },
  session: {
    server_key: 'srv-vault',
    server_url: 'https://vault.example.lan:8443',
    token: 'demo-token',
    expires_at: NOW + 12 * 3600 * 1000,
    user_id: demoUser.id,
    username: demoUser.username,
    role: 'super_admin'
  }
}

export const demoAgentHealth = {
  ok: true,
  agent_version: '0.2.0',
  platform: 'windows',
  admin: true,
  host: demoAgentState.host,
  servers: demoAgentState.servers,
  primary_key: 'srv-vault',
  expected_version: '0.2.0',
  version_mismatch: false,
  server: {
    connected: true,
    url: 'https://vault.example.lan:8443',
    name: 'WIN-E095H11JSS'
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
  update_channel: 'beta',
  server_alias: 'WIN-E095H11JSS',
  auto_login: true,
  auto_update: true,
  cache_l1_bytes: 512 * MB,
  cache_l2_bytes: 4 * GB,
  cache_l2_dir: '',
  repo_mounts: {},
  group_mounts: {}
}

export const demoSystemInfo = {
  server_instance_id: 'srv-vault',
  server_name: 'WIN-E095H11JSS',
  api_version: 1,
  server_version: '0.2.0',
  client_compat: { min: '0.1.0', max: '0.3.0', recommended: '0.2.0' },
  features: ['shared_repo', 'diff_disk', 'cert_login', 'auto_update', 'pool'],
  capabilities: {
    platform_kind: 'windows',
    win_target: true,
    iscsi_sessions: true,
    lvm: false,
    lvm_cache: false,
    hyperv: true,
    storage_root: 'D:\\VaultData',
    storage_roots: ['D:\\VaultData', 'E:\\VaultSSD', 'F:\\VaultArchive'],
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
