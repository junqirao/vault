/**
 * 本机 Steam 库扫描。
 *
 * 设计目标：**不再依赖服务端访问 Steam 商店 API 来补名称/封面**。
 * 客户端本地通常已经登录 Steam，`appcache/appinfo.vdf`、`appcache/librarycache`
 * 和 `steamapps/appmanifest_*.acf` 里就有游戏名称、封面与安装状态。
 *
 * 这里只读取本地文件，不读取/上传任何 Steam 凭据、token 或登录态。
 */

import { execFile } from 'node:child_process'
import { promises as fs } from 'node:fs'
import path from 'node:path'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)

/** 单个游戏的本地库条目。 */
export interface LocalSteamGame {
  app_id: number
  name: string
  installed: boolean
  has_image: boolean
  has_icon: boolean
  size_bytes?: number
}

/** 一次本机 Steam 库扫描结果。 */
export interface LocalSteamLibrary {
  available: boolean
  steam_root?: string
  account_name?: string
  games: LocalSteamGame[]
  error?: string
}

interface AppMetadata {
  name: string
  type: string
}

interface InternalGame {
  appID: number
  name: string
  installed: boolean
  imagePath?: string
  iconPath?: string
  sizeBytes?: number
}

interface ScanCache {
  at: number
  result: LocalSteamLibrary
}

const CACHE_TTL_MS = 30_000
/** 单张本地封面允许通过 IPC 传给渲染进程的最大体积（data URL 会再增加约 1/3）。 */
const MAX_LOCAL_IMAGE_BYTES = 2 * 1024 * 1024
let scanCache: ScanCache | null = null
/** appID → 本地封面绝对路径；只允许 main 进程内部按 appID 读取。 */
const imageFiles = new Map<number, string>()
/** appID → 本地 32×32 应用图标绝对路径；列表行优先用这个，不用封面硬裁。 */
const iconFiles = new Map<number, string>()

type VdfValue = string | VdfObject
interface VdfObject {
  [key: string]: VdfValue
}

function asObject(value: VdfValue | undefined): VdfObject {
  return value !== null && typeof value === 'object' ? value : {}
}

function asString(value: VdfValue | undefined): string {
  return typeof value === 'string' ? value : ''
}

/** 极简文本 VDF 解析器，够读 libraryfolders.vdf / appmanifest_*.acf / loginusers.vdf。 */
class TextVdfParser {
  private i = 0

  constructor(private readonly text: string) {}

  parse(): VdfObject {
    return this.parseObject(false)
  }

  private skip(): void {
    for (;;) {
      while (this.i < this.text.length && /\s/.test(this.text[this.i])) this.i += 1
      if (this.text[this.i] === '/' && this.text[this.i + 1] === '/') {
        while (this.i < this.text.length && this.text[this.i] !== '\n') this.i += 1
        continue
      }
      return
    }
  }

  private parseObject(nested: boolean): VdfObject {
    const out: VdfObject = {}
    for (;;) {
      this.skip()
      if (this.i >= this.text.length) {
        if (nested) throw new Error('VDF 对象未闭合')
        return out
      }
      if (this.text[this.i] === '}') {
        this.i += 1
        if (!nested) throw new Error('多余的 VDF 结束符')
        return out
      }
      const key = this.parseToken()
      this.skip()
      if (this.text[this.i] === '{') {
        this.i += 1
        out[key] = this.parseObject(true)
      } else {
        out[key] = this.parseToken()
      }
    }
  }

  private parseToken(): string {
    this.skip()
    if (this.text[this.i] === '"') {
      this.i += 1
      let out = ''
      while (this.i < this.text.length) {
        const ch = this.text[this.i]
        this.i += 1
        if (ch === '"') return out
        if (ch === '\\' && this.i < this.text.length) {
          const esc = this.text[this.i]
          this.i += 1
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc
          continue
        }
        out += ch
      }
      throw new Error('VDF 字符串未闭合')
    }
    let out = ''
    while (this.i < this.text.length && !/\s/.test(this.text[this.i]) && !'{}'.includes(this.text[this.i])) {
      out += this.text[this.i]
      this.i += 1
    }
    return out
  }
}

function parseTextVdf(text: string): VdfObject {
  return new TextVdfParser(text).parse()
}

/** 读取二进制 appinfo.vdf 中 appID → 名称/类型。 */
function parseAppInfo(data: Buffer): Map<number, AppMetadata> {
  const out = new Map<number, AppMetadata>()
  if (data.length < 16) return out
  let pos = 0
  const magic = data.readUInt32LE(pos)
  pos += 4
  const version = magic & 0xff
  if (magic >>> 8 !== 0x075644) return out
  pos += 4 // universe

  let strings: string[] = []
  if (version >= 41) {
    const tableOffset = Number(data.readBigUInt64LE(pos))
    pos += 8
    if (!Number.isSafeInteger(tableOffset) || tableOffset <= 0 || tableOffset >= data.length) return out
    const save = pos
    pos = tableOffset
    const count = data.readUInt32LE(pos)
    pos += 4
    for (let i = 0; i < count && pos < data.length; i += 1) {
      const end = data.indexOf(0, pos)
      if (end < 0) return out
      strings.push(data.subarray(pos, end).toString('utf8'))
      pos = end + 1
    }
    pos = save
  }

  const parseKey = (): string => {
    if (version >= 41) {
      const idx = data.readUInt32LE(pos)
      pos += 4
      return strings[idx] ?? ''
    }
    const end = data.indexOf(0, pos)
    if (end < 0) throw new Error('appinfo key 未闭合')
    const key = data.subarray(pos, end).toString('utf8')
    pos = end + 1
    return key
  }

  const readCString = (): string => {
    const end = data.indexOf(0, pos)
    if (end < 0) throw new Error('appinfo string 未闭合')
    const value = data.subarray(pos, end).toString('utf8')
    pos = end + 1
    return value
  }

  const readWideString = (): string => {
    let end = pos
    while (end + 1 < data.length && !(data[end] === 0 && data[end + 1] === 0)) end += 2
    const value = data.subarray(pos, end).toString('utf16le')
    pos = end + 2
    return value
  }

  const parseValue = (end: number): [string, VdfValue] | null => {
    if (pos >= end) return null
    const type = data[pos]
    pos += 1
    if (type === 0x08) return null
    const key = parseKey()
    switch (type) {
      case 0x00: {
        const obj: VdfObject = {}
        for (;;) {
          const child = parseValue(end)
          if (!child) break
          obj[child[0]] = child[1]
        }
        return [key, obj]
      }
      case 0x01:
        return [key, readCString()]
      case 0x02:
        if (pos + 4 > end) return [key, '0']
        {
          const value = data.readInt32LE(pos)
          pos += 4
          return [key, String(value)]
        }
      case 0x03:
        if (pos + 4 > end) return [key, '0']
        {
          const value = data.readFloatLE(pos)
          pos += 4
          return [key, String(value)]
        }
      case 0x04:
      case 0x06:
        if (pos + 4 > end) return [key, '0']
        {
          const value = data.readUInt32LE(pos)
          pos += 4
          return [key, String(value)]
        }
      case 0x05:
        return [key, readWideString()]
      case 0x07:
        if (pos + 8 > end) return [key, '0']
        {
          const value = data.readBigUInt64LE(pos)
          pos += 8
          return [key, value.toString()]
        }
      default:
        throw new Error(`appinfo 未知类型 ${type}`)
    }
  }

  while (pos + 4 <= data.length) {
    const appID = data.readUInt32LE(pos)
    pos += 4
    if (appID === 0) break
    if (pos + 4 > data.length) break
    const size = data.readUInt32LE(pos)
    pos += 4
    const end = Math.min(pos + size, data.length)
    // infoState + lastUpdated + accessToken + sha1 + changeNumber
    pos += 4 + 4 + 8 + 20 + 4
    if (version >= 40) pos += 20 // binaryDataHash
    try {
      const root = parseValue(end)
      if (root) {
        const appinfo = asObject(root[1])
        const common = asObject(appinfo.common)
        const name = asString(common.name).trim()
        const type = asString(common.type).trim()
        if (name) out.set(appID, { name, type })
      }
    } catch {
      // 单个条目解析失败不影响其它游戏：Steam 版本变化时尽量降级为 AppID。
    }
    pos = end
  }
  return out
}

async function readText(file: string): Promise<string> {
  try {
    return await fs.readFile(file, 'utf8')
  } catch {
    return ''
  }
}

async function exists(file: string): Promise<boolean> {
  try {
    await fs.access(file)
    return true
  } catch {
    return false
  }
}

async function queryRegistrySteamPath(): Promise<string | null> {
  if (process.platform !== 'win32') return null
  try {
    const { stdout } = await execFileAsync('reg', ['query', 'HKCU\\Software\\Valve\\Steam', '/v', 'SteamPath'], {
      windowsHide: true,
      timeout: 3000
    })
    const match = /SteamPath\s+REG_SZ\s+(.+)$/im.exec(stdout)
    if (!match) return null
    return match[1].trim().replace(/\//g, '\\')
  } catch {
    return null
  }
}

async function findSteamRoot(): Promise<string | null> {
  const candidates: string[] = []
  if (process.platform === 'win32') {
    const fromRegistry = await queryRegistrySteamPath()
    if (fromRegistry) candidates.push(fromRegistry)
    const programFiles = process.env['ProgramFiles'] ?? 'C:\\Program Files'
    const programFilesX86 = process.env['ProgramFiles(x86)'] ?? 'C:\\Program Files (x86)'
    candidates.push(path.join(programFilesX86, 'Steam'), path.join(programFiles, 'Steam'))
  } else if (process.platform === 'darwin') {
    const home = process.env.HOME ?? ''
    candidates.push(path.join(home, 'Library', 'Application Support', 'Steam'))
  } else {
    const home = process.env.HOME ?? ''
    candidates.push(path.join(home, '.steam', 'steam'), path.join(home, '.local', 'share', 'Steam'))
  }
  const seen = new Set<string>()
  for (const candidate of candidates) {
    if (!candidate) continue
    const normalized = path.normalize(candidate)
    if (seen.has(normalized)) continue
    seen.add(normalized)
    if (await exists(path.join(normalized, 'steamapps'))) return normalized
  }
  return null
}

function imageCandidates(dir: string): string[] {
  // 优先使用宽幅素材（460x215 / 920x430），最后才回退到竖版胶囊和透明 logo：
  // 卡片是宽幅比例，竖版图即便不拉伸也会被裁掉大半。
  return [
    'header_schinese.jpg',
    'header.jpg',
    'library_header.jpg',
    'library_hero.jpg',
    'capsule_467x181.jpg',
    'capsule_231x87.jpg',
    'library_capsule_schinese.jpg',
    'library_capsule.jpg',
    'library_600x900_schinese.jpg',
    'library_600x900.jpg',
    'logo.png'
  ].map((name) => path.join(dir, name))
}

async function pickImage(dir: string): Promise<string | undefined> {
  // 大图（library_hero 常见 3~5 MB）通过 IPC 传给渲染进程太重；跳过它们继续找下一张。
  for (const candidate of imageCandidates(dir)) {
    try {
      const stat = await fs.stat(candidate)
      if (stat.isFile() && stat.size > 0 && stat.size <= MAX_LOCAL_IMAGE_BYTES) return candidate
    } catch {
      // 不存在就继续看下一个候选。
    }
  }
  try {
    const entries = await fs.readdir(dir, { withFileTypes: true })
    for (const entry of entries) {
      if (!entry.isFile()) continue
      if (!/\.(jpg|jpeg|png)$/i.test(entry.name)) continue
      // 40 位十六进制文件是 32×32 应用图标，不能拿来当封面。
      if (/^[0-9a-f]{40}\.(jpg|jpeg|png)$/i.test(entry.name)) continue
      if (/^icon\.(jpg|jpeg|png)$/i.test(entry.name)) continue
      const candidate = path.join(dir, entry.name)
      try {
        const stat = await fs.stat(candidate)
        if (stat.size > 0 && stat.size <= MAX_LOCAL_IMAGE_BYTES) return candidate
      } catch {
        // 单个文件读不到不影响其它候选。
      }
    }
  } catch {
    // 没缓存封面：界面显示占位块即可。
  }
  return undefined
}

/**
 * 读取 Steam 本地 32×32 应用图标。
 *
 * Steam 的 librarycache 目录里，封面是 header/library_*，图标则是 40 位 sha1 命名的
 * 小 jpg（或 icon.jpg）。列表行用图标而不是封面，避免比例不兼容。
 */
async function pickIcon(dir: string): Promise<string | undefined> {
  for (const name of ['icon.jpg', 'icon.png']) {
    const candidate = path.join(dir, name)
    try {
      const stat = await fs.stat(candidate)
      if (stat.isFile() && stat.size > 0 && stat.size <= 256 * 1024) return candidate
    } catch {
      // 继续看哈希命名的图标。
    }
  }
  try {
    const entries = await fs.readdir(dir, { withFileTypes: true })
    for (const entry of entries) {
      if (!entry.isFile()) continue
      if (!/^[0-9a-f]{40}\.(jpg|jpeg|png)$/i.test(entry.name)) continue
      const candidate = path.join(dir, entry.name)
      try {
        const stat = await fs.stat(candidate)
        if (stat.size > 0 && stat.size <= 256 * 1024) return candidate
      } catch {
        // 单个文件读不到不影响其它候选。
      }
    }
  } catch {
    // 没有图标缓存：界面用占位图标。
  }
  return undefined
}

/**
 * appinfo 的 common.type：只保留**游戏本体**。
 *
 * 列表里不再展示 DLC、Demo、专用服务器、SDK/工具、原声和内置应用；
 * 这些类型即使本地有缓存，也不应该出现在"添加游戏"选择器里。
 */
function isGameType(type: string): boolean {
  return type.trim().toLowerCase() === 'game'
}

async function collectInstalled(
  games: Map<number, InternalGame>,
  libraryRoots: string[],
  appMetadata: Map<number, AppMetadata>
): Promise<void> {
  for (const root of libraryRoots) {
    const steamapps = path.join(root, 'steamapps')
    let entries: string[] = []
    try {
      entries = await fs.readdir(steamapps)
    } catch {
      continue
    }
    for (const name of entries) {
      const match = /^appmanifest_(\d+)\.acf$/i.exec(name)
      if (!match) continue
      const appID = Number(match[1])
      if (!Number.isSafeInteger(appID) || appID <= 0) continue
      const meta = appMetadata.get(appID)
      if (!isGameType(meta?.type ?? '')) continue
      const raw = await readText(path.join(steamapps, name))
      if (!raw) continue
      let state: VdfObject = {}
      try {
        state = asObject(parseTextVdf(raw).AppState)
      } catch {
        continue
      }
      const gameName = asString(state.name).trim() || meta?.name || `App ${appID}`
      const size = Number(asString(state.SizeOnDisk))
      const existing = games.get(appID)
      games.set(appID, {
        appID,
        name: existing?.name && !existing.name.startsWith('App ') ? existing.name : gameName,
        installed: true,
        imagePath: existing?.imagePath,
        iconPath: existing?.iconPath,
        sizeBytes: Number.isFinite(size) && size > 0 ? size : existing?.sizeBytes
      })
    }
  }
}

async function collectLibraryCache(games: Map<number, InternalGame>, appMetadata: Map<number, AppMetadata>, root: string): Promise<void> {
  const cacheDir = path.join(root, 'appcache', 'librarycache')
  let entries: import('node:fs').Dirent[] = []
  try {
    entries = await fs.readdir(cacheDir, { withFileTypes: true })
  } catch {
    return
  }
  for (const entry of entries) {
    if (!entry.isDirectory() || !/^\d+$/.test(entry.name)) continue
    const appID = Number(entry.name)
    if (!Number.isSafeInteger(appID) || appID <= 0) continue
    const dir = path.join(cacheDir, entry.name)
    const meta = appMetadata.get(appID)
    if (!isGameType(meta?.type ?? '')) continue
    const imagePath = await pickImage(dir)
    const iconPath = await pickIcon(dir)
    const existing = games.get(appID)
    games.set(appID, {
      appID,
      name: meta?.name ?? existing?.name ?? `App ${appID}`,
      installed: existing?.installed ?? false,
      imagePath: imagePath ?? existing?.imagePath,
      iconPath: iconPath ?? existing?.iconPath,
      sizeBytes: existing?.sizeBytes
    })
  }
}

async function collectLocalConfigApps(games: Map<number, InternalGame>, appMetadata: Map<number, AppMetadata>, root: string): Promise<void> {
  const userdata = path.join(root, 'userdata')
  let users: string[] = []
  try {
    users = (await fs.readdir(userdata, { withFileTypes: true })).filter((e) => e.isDirectory()).map((e) => e.name)
  } catch {
    return
  }
  for (const user of users) {
    const raw = await readText(path.join(userdata, user, 'config', 'localconfig.vdf'))
    if (!raw) continue
    try {
      const parsed = parseTextVdf(raw)
      const store = asObject(parsed.UserLocalConfigStore)
      const software = asObject(store.Software)
      const valve = asObject(software.Valve)
      const steam = asObject(valve.Steam)
      const apps = asObject(steam.apps)
      for (const key of Object.keys(apps)) {
        if (!/^\d+$/.test(key)) continue
        const appID = Number(key)
        if (!Number.isSafeInteger(appID) || appID <= 0 || games.has(appID)) continue
        const meta = appMetadata.get(appID)
        // 没有 appinfo 元数据的条目通常是内置应用/SDK：不展示。
        if (!meta || !isGameType(meta.type)) continue
        games.set(appID, { appID, name: meta.name, installed: false, imagePath: undefined })
      }
    } catch {
      // 单个用户的 localconfig 解析失败不影响其它来源。
    }
  }
}

/** 本机 Steam 库信息（用于把游戏挂到 Steam 能识别的库目录）。 */
export interface SteamLibraryInfo {
  path: string
  label?: string
  app_count: number
  free_bytes?: number
  total_bytes?: number
  /** 是否是 Steam 安装目录本身（它同时也是一个 Steam 库）。 */
  steam_root: boolean
}

/**
 * 列出本机 Steam 库（steamapps 目录所在路径）。
 *
 * 读取 libraryfolders.vdf 的 path/label/apps，并尽量附带卷可用空间；
 * 仅用于客户端设置里的下拉选择，不上传任何凭据。
 *
 * 注意两个坑：
 *  1. Windows 路径大小写不敏感：注册表给的是 `c:\program files (x86)\steam`，
 *     libraryfolders.vdf 里是 `C:\Program Files (x86)\Steam`，按字符串去重会出两条。
 *  2. Steam 安装目录本身也是一个库，排在真正的游戏库后面，避免用户以为选错了。
 */
export async function listSteamLibraries(): Promise<SteamLibraryInfo[]> {
  const root = await findSteamRoot()
  if (!root) return []

  const pathKey = (value: string): string =>
    process.platform === 'win32' ? value.toLowerCase() : value
  type Entry = { path: string; label: string; apps: number }
  const entries = new Map<string, Entry>()
  const add = (rawPath: string, label: string, apps: number): void => {
    const cleaned = path.normalize(rawPath.trim())
    if (!cleaned) return
    const key = pathKey(cleaned)
    const prev = entries.get(key)
    entries.set(key, {
      // 先出现的写法优先：libraryfolders.vdf 里的拼写通常和资源管理器一致。
      path: prev?.path ?? cleaned,
      label: prev?.label || label,
      apps: Math.max(prev?.apps ?? 0, apps)
    })
  }

  for (const file of [
    path.join(root, 'steamapps', 'libraryfolders.vdf'),
    path.join(root, 'config', 'libraryfolders.vdf')
  ]) {
    const raw = await readText(file)
    if (!raw) continue
    try {
      const folders = asObject(parseTextVdf(raw).libraryfolders)
      for (const value of Object.values(folders)) {
        // 新格式是 { path, label, apps }；老版本是 "1"  "D:\\SteamLibrary" 这样的纯路径。
        if (typeof value === 'string') {
          add(value, '', 0)
          continue
        }
        const obj = asObject(value)
        const folder = asString(obj.path).trim()
        if (!folder) continue
        add(folder, asString(obj.label).trim(), Object.keys(asObject(obj.apps)).length)
      }
    } catch {
      // 单个 libraryfolders.vdf 损坏不影响其它来源。
    }
  }
  // 安装目录兜底：vdf 里通常也列了它，这里只在缺失时补上（并沿用 vdf 的写法）。
  add(root, '', 0)

  const out: SteamLibraryInfo[] = []
  for (const entry of entries.values()) {
    const libPath = entry.path
    // 特征校验：Steam 库的标志是 <库根>\steamapps 存在（appmanifest 都写在里面）。
    if (!(await exists(path.join(libPath, 'steamapps')))) continue
    let freeBytes: number | undefined
    let totalBytes: number | undefined
    try {
      const stats = await fs.statfs(libPath)
      freeBytes = Number(stats.bavail) * Number(stats.bsize)
      totalBytes = Number(stats.blocks) * Number(stats.bsize)
    } catch {
      // statfs 在部分平台/旧 Node 上不可用：路径与游戏数仍然可用。
    }
    out.push({
      path: libPath,
      label: entry.label || undefined,
      app_count: entry.apps,
      free_bytes: freeBytes,
      total_bytes: totalBytes,
      steam_root: pathKey(libPath) === pathKey(root)
    })
  }
  return out.sort((a, b) => {
    if (a.steam_root !== b.steam_root) return a.steam_root ? 1 : -1
    return a.path.localeCompare(b.path)
  })
}

async function collectLibraryRoots(root: string): Promise<string[]> {
  const result = new Set<string>([path.normalize(root)])
  for (const file of [path.join(root, 'steamapps', 'libraryfolders.vdf'), path.join(root, 'config', 'libraryfolders.vdf')]) {
    const raw = await readText(file)
    if (!raw) continue
    try {
      const parsed = parseTextVdf(raw)
      const folders = asObject(parsed.libraryfolders)
      for (const value of Object.values(folders)) {
        const obj = asObject(value)
        const folder = asString(obj.path).trim()
        if (folder) result.add(path.normalize(folder))
      }
    } catch {
      // 忽略损坏的 libraryfolders.vdf，至少保留主 Steam 目录。
    }
  }
  return [...result]
}

async function readActiveAccountName(root: string): Promise<string | undefined> {
  const raw = await readText(path.join(root, 'config', 'loginusers.vdf'))
  if (!raw) return undefined
  try {
    const users = asObject(parseTextVdf(raw).users)
    let best: { name: string; timestamp: number; auto: boolean } | null = null
    for (const value of Object.values(users)) {
      const user = asObject(value)
      const name = asString(user.AccountName).trim()
      if (!name) continue
      const timestamp = Number(asString(user.timestamp)) || 0
      const auto = asString(user.AutoLogin) === '1'
      if (!best || (auto && !best.auto) || (auto === best.auto && timestamp > best.timestamp)) {
        best = { name, timestamp, auto }
      }
    }
    return best?.name
  } catch {
    return undefined
  }
}

/** 单个扫描的共享 Promise：封面 IPC 与列表 IPC 并发时只扫一次。 */
let scanPromise: Promise<LocalSteamLibrary> | null = null

/** 扫描本机 Steam 库；结果缓存 30 秒。 */
export async function listLocalSteamLibrary(force = false): Promise<LocalSteamLibrary> {
  // 正在扫描时复用同一个 Promise：rescan 与封面懒加载并发也不会互相清空 imageFiles。
  if (scanPromise) return scanPromise
  if (!force && scanCache && Date.now() - scanCache.at < CACHE_TTL_MS) {
    return scanCache.result
  }
  scanPromise = scanLocalSteamLibrary()
  try {
    return await scanPromise
  } finally {
    scanPromise = null
  }
}

async function scanLocalSteamLibrary(): Promise<LocalSteamLibrary> {
  imageFiles.clear()
  iconFiles.clear()
  const root = await findSteamRoot()
  if (!root) {
    const result: LocalSteamLibrary = {
      available: false,
      games: [],
      error: 'steam_not_found'
    }
    scanCache = { at: Date.now(), result }
    return result
  }

  const appMetadata = new Map<number, AppMetadata>()
  const appInfoPath = path.join(root, 'appcache', 'appinfo.vdf')
  for (let attempt = 0; attempt < 2 && appMetadata.size === 0; attempt += 1) {
    try {
      const raw = await fs.readFile(appInfoPath)
      for (const [appID, meta] of parseAppInfo(raw)) appMetadata.set(appID, meta)
    } catch {
      // appinfo.vdf 读不到时仍可用 appmanifest 的安装游戏名称。
    }
    // Steam 可能正在异步重写 appinfo.vdf（读到半截）：稍等再试一次。
    if (appMetadata.size === 0) await new Promise((resolve) => setTimeout(resolve, 300))
  }

  const games = new Map<number, InternalGame>()
  const libraryRoots = await collectLibraryRoots(root)
  await collectInstalled(games, libraryRoots, appMetadata)
  await collectLibraryCache(games, appMetadata, root)
  await collectLocalConfigApps(games, appMetadata, root)

  const list: LocalSteamGame[] = [...games.values()]
    .filter((game) => game.appID > 0)
    // 只展示有真实名称的游戏本体；App <id> 占位说明本地没有商店信息，不进入选择器。
    .filter((game) => game.name.trim() !== '' && !/^App \d+$/.test(game.name.trim()))
    .map((game) => {
      if (game.imagePath) imageFiles.set(game.appID, game.imagePath)
      if (game.iconPath) iconFiles.set(game.appID, game.iconPath)
      return {
        app_id: game.appID,
        name: game.name || `App ${game.appID}`,
        installed: game.installed,
        has_image: Boolean(game.imagePath),
        has_icon: Boolean(game.iconPath),
        size_bytes: game.sizeBytes
      }
    })
    .sort((a, b) => {
      if (a.installed !== b.installed) return a.installed ? -1 : 1
      return a.name.localeCompare(b.name)
    })

  const result: LocalSteamLibrary = {
    available: true,
    steam_root: root,
    account_name: await readActiveAccountName(root),
    games: list
  }
  scanCache = { at: Date.now(), result }
  return result
}

/** 读取本机缓存封面，返回 data URL；只允许扫描阶段登记过的 appID。 */
export async function readLocalSteamImage(appID: number): Promise<string | null> {
  let file = imageFiles.get(appID)
  if (!file) {
    // 页面加载与封面懒加载存在竞态：首次没有图时等待一次扫描，避免永久显示占位块。
    await listLocalSteamLibrary(false)
    file = imageFiles.get(appID)
  }
  if (!file) return null
  try {
    const stat = await fs.stat(file)
    if (stat.size <= 0 || stat.size > MAX_LOCAL_IMAGE_BYTES) return null
    const data = await fs.readFile(file)
    const mime = /\.png$/i.test(file) ? 'image/png' : 'image/jpeg'
    return `data:${mime};base64,${data.toString('base64')}`
  } catch {
    return null
  }
}

/** 读取本机 Steam 32×32 应用图标，返回 data URL；没有缓存时返回 null。 */
export async function readLocalSteamIcon(appID: number): Promise<string | null> {
  let file = iconFiles.get(appID)
  if (!file) {
    await listLocalSteamLibrary(false)
    file = iconFiles.get(appID)
  }
  if (!file) return null
  try {
    const stat = await fs.stat(file)
    if (stat.size <= 0 || stat.size > 256 * 1024) return null
    const data = await fs.readFile(file)
    const mime = /\.png$/i.test(file) ? 'image/png' : 'image/jpeg'
    return `data:${mime};base64,${data.toString('base64')}`
  } catch {
    return null
  }
}
