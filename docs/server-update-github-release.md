# 服务端经 GitHub Release 自更新调研与设计

> 状态：已实现（Windows / Linux；Linux systemd 使用独立 helper unit）  
> 范围：服务端自更新（Windows / Linux）  
> 结论：**技术可行且已落地**。客户端更新链路完全复用；服务端自更新包含“检查 → 下载 → 暂存 → 替换 → 重启 → 对账”完整链路。

---

## 1. 背景与目标

当前发布流程已经会把 Windows / Linux 服务端包、客户端包和校验文件上传到 GitHub Release。希望在此基础上增加一条服务端自更新链路：

1. 在管理端「系统设置」里检查 GitHub Release 是否有新版本；
2. 版本比较支持 `beta` / `rc` 等预发布版本；
3. 从 GitHub 下载服务端更新包，支持进度展示与断点续传；
4. 下载完成后由独立进程完成替换并重启服务端，避免“程序自己替换自己”；
5. 更新成功后删除旧的更新包与临时文件；
6. 服务端包内已经包含客户端更新产物，客户端继续使用原有更新逻辑，不改客户端自更新流程。

需要明确的是：**“设置页面”属于 Electron 客户端的管理端界面，不是服务端网页。** 因此服务端自更新逻辑本身不需要改客户端更新器，但新增检查按钮、进度条和重启提示仍然需要改管理端前端代码。

---

## 2. 现状核查

### 2.1 客户端更新链路可以复用

现有链路：

```text
客户端启动 / 设置页手动检查
  -> GET  /v1/system/update/manifest
  -> GET  /v1/system/update/signature
  -> 客户端内置公钥验签
  -> 按清单选择产物
  -> GET  /v1/system/update/artifacts/<filename>
  -> 下载、校验、解压、重启客户端
```

相关代码：

- `internal/api/update.go`：服务端只做 mirror，不解析、不签名、不改写清单；
- `internal/app/update.go`：`update.artifacts_dir` 指向发布目录，默认 `updates`，相对路径按配置文件目录解析；
- `internal/updatepkg/updatepkg.go`：清单格式与 Ed25519 验签；
- `internal/agent/update.go`：客户端自更新、断点续传、替换器、pending/backup/启动对账；
- `cmd/vault-updator/main.go`：客户端启动期整包更新器。

`build.py --package-all` 会自动启用 `--with-client`，因此服务端部署包里的 `updates/` 已经包含客户端更新清单与客户端产物。服务端更新完成后，只要新 `updates/` 就位，客户端继续请求同一个接口即可，**客户端自更新逻辑无需修改**。

### 2.2 服务端目前没有自更新

服务端现有能力：

- `cmd/vault-server/main.go`：前台运行、优雅停机；
- `cmd/vault-server/launcher_windows.go`：`--background` 启动器，派生无控制台后台进程；
- `cmd/vault-server/stop_windows.go` / `stop_other.go`：`Vault-Server stop` 跨进程优雅停机；
- 启动时获取单实例锁；
- `Vault-Server doctor` 等运维子命令。

没有：

- 服务端更新检查接口；
- 服务端更新包下载器；
- 服务端替换器 / 重启助手；
- 服务端更新状态与进度接口。

### 2.3 版本号体系需要补齐

当前有两套版本：

| 版本 | 来源 | 示例 | 用途 |
| --- | --- | --- | --- |
| 内部构建号 | 根目录 `VERSION` | `0.3.286` | 自动更新、兼容性判定、产物命名 |
| 对外发布版本 | 根目录 `RELEASE.json` | `0.3.1` + `beta` | 界面展示、GitHub Release 标签、发布包命名 |

问题：

- 前端通过 Vite 注入 `RELEASE_VERSION` / `RELEASE_CHANNEL`；
- Go 服务端只注入了 `version.Version`（内部构建号），**不知道自己的对外发布版本**；
- `internal/semver` 会直接裁掉 `-beta` / `-rc` 后缀，不能用于发布版本的预发布比较；
- GitHub Release 标签形如 `v0.3.1-beta`，必须能正确比较 `beta`、`rc` 与正式版。

建议新增：

```go
// internal/version
var (
    Version        = "0.3.286" // 内部构建号
    ReleaseVersion = "0.3.1"   // 对外发布版本
    ReleaseChannel = "beta"    // stable / beta / rc / alpha
    ReleaseRepo    = "junqirao/vault"
)
```

由 `build.py` 从 `RELEASE.json` 读取并注入 ldflags，`SystemInfo` 增加：

```json
{
  "server_version": "0.3.286",
  "release_version": "0.3.1",
  "release_channel": "beta"
}
```

设置页同时展示“发布版本”和“内部构建号”。

> 旧版本服务端没有这些字段，也没有新的更新接口，因此第一次升级到“支持在线更新的版本”必须手动安装一次，这是预期行为。

### 2.4 GitHub Release 实测

当前仓库 `junqirao/vault` 的 Release 已发布预发布版本，例如 `v0.3.1-beta`：

- `prerelease=true`；
- 资产包括：
  - `Vault-Server-0.3.1-beta-win64.zip`
  - `Vault-Client-0.3.1-beta-win64.zip`
  - `SHA256SUMS.txt`
- GitHub API 的 asset 已经直接返回 `digest: "sha256:..."`；
- 资产下载支持 `Range`，返回 `206 Partial Content` 与 `Accept-Ranges: bytes`，断点续传可行；
- `browser_download_url` 会 302 到带时效的 CDN 地址，续传前如果地址过期，需要重新查询 release/asset 获取新地址；
- `GET /releases/latest` **不会返回预发布版本**，当前会 404。必须使用 `/releases` 列表并按版本筛选。

这些结论说明：GitHub Release 作为服务端更新源是可行的，不能通过抓 HTML 页面实现，应走 GitHub REST API。

---

## 3. 总体方案

```text
管理端设置页
  -> GET  /v1/system/server-update          读取当前版本与更新状态
  -> POST /v1/system/server-update/check    查询 GitHub Release
  -> POST /v1/system/server-update/download 下载 + 校验 + 暂存
  -> POST /v1/system/server-update/apply    启动替换器并优雅停机
                                                |
                                                v
                                     独立 helper 进程
                                       -> 等待旧进程退出
                                       -> 备份旧 exe
                                       -> 替换 exe / scripts / updates
                                       -> 启动新服务端
                                       -> 等待服务端监听/健康检查
                                       -> 成功清理 / 失败回滚
```

职责边界：

- **服务端**：负责检查、下载、校验、解压、写 pending、启动 helper、优雅停机；
- **helper**：负责在旧进程退出后替换文件、启动新进程、失败回滚；
- **客户端**：只负责管理端界面展示与操作，不改变自更新流程；
- **GitHub**：只作为发布物来源，不作为运行期依赖。

---

## 4. 详细设计

### 4.1 发布版本比较

新增独立包，例如 `internal/releasever`，按 semver 2.0 规则比较：

- `0.3.1-beta < 0.3.1-rc < 0.3.1`
- `0.3.1-rc.1 < 0.3.1-rc.2`
- `0.3.2-beta > 0.3.1`
- 支持 `v0.3.1-rc` 与 `v0.3.1-rc.1`
- 忽略 build metadata（`+build`）

不建议直接修改 `internal/semver`：它当前服务于客户端兼容区间，裁掉预发布后缀是有意行为，改动会影响 `client_compat` 的 `min` / `max` 语义。

通道选择建议：

| 当前通道 | 允许升级到 |
| --- | --- |
| stable | 仅 stable |
| beta | beta / rc / stable |
| rc | rc / stable |
| alpha | alpha / beta / rc / stable |

如果同一版本有多个通道，按 semver 预发布优先级选择，不按字符串排序。

### 4.2 GitHub Release 检查

请求：

```http
GET https://api.github.com/repos/{owner}/{repo}/releases?per_page=30
Accept: application/vnd.github+json
User-Agent: Vault-Server
X-GitHub-Api-Version: 2022-11-28
```

处理：

1. 过滤 `draft=true`；
2. 按当前通道过滤 `prerelease`；
3. 解析 `tag_name` 为发布版本与预发布后缀；
4. 选择 semver 最大的候选；
5. 与服务端当前 `ReleaseVersion + ReleaseChannel` 比较；
6. 按操作系统/架构选择资产。

注意：

- 公共仓库匿名访问有 60 次/小时限制；检查结果会缓存，避免每次打开页面都请求；
- 更新源固定为 `https://github.com/junqirao/vault/releases`，不提供仓库/令牌/代理配置；
- 标准 `HTTP_PROXY` / `HTTPS_PROXY` 环境变量仍由 Go 默认传输层自动继承；
- 不抓 HTML，不依赖 `/releases/latest`。

### 4.3 资产选择

按平台选择：

| 平台 | 资产名 |
| --- | --- |
| Windows amd64 | `Vault-Server-<public-version>-win64.zip` |
| Linux amd64 | `Vault-Server-<public-version>-linux-amd64.tar.gz` |

当前 `release.py` 默认只上传 Windows 服务端；Linux 需要 `--with-linux`。如果服务端支持 Linux，建议把 Linux 资产纳入默认发布，或在检查时明确返回“当前平台没有可用资产”。

建议在 Release 中额外发布一个小的 `release.json`：

```json
{
  "version": "0.3.2",
  "channel": "rc",
  "internal_version": "0.3.300",
  "assets": {
    "win64": {
      "name": "Vault-Server-0.3.2-rc-win64.zip",
      "sha256": "...",
      "size": 122300378
    }
  }
}
```

这样服务端不必从标签和文件名反推全部信息，也方便未来扩展平台与签名信息。

### 4.4 下载与断点续传

更新状态建议：

```text
idle -> checking -> available -> downloading -> downloaded -> applying -> restarting
                                 \-> failed
```

下载目录建议：

```text
<config dir>/update/server/
  downloads/<asset>.zip.part  # 断点续传文件
  downloads/<asset>.zip       # 下载完成、校验通过后的包
  staging/<tag>/              # 解压目录
  backup/Vault-Server.exe...  # 旧版本备份（成功后删除）
  helper/Vault-Server.exe...  # helper 副本（新服务端启动后清理）
  pending.json                # 替换任务
  last-result.json            # 上一次更新结果
```

下载流程：

1. 如果 `.part` 存在，读取已有大小；
2. 请求资产地址并带 `Range: bytes=<offset>-`；
3. 若返回 `206`，从断点续传；若返回 `200`，丢弃前缀从头下载；
4. 边下载边计算 sha256；
5. 下载完成后与 GitHub asset `digest` 或 `SHA256SUMS.txt` 比对；
6. 校验通过后把 `.part` 改名为正式 zip；
7. 失败时保留 `.part`，下一次继续续传；摘要不符时丢弃 `.part`。

注意：

- 资产下载地址有时效，长时间暂停后需要重新查询；
- 下载前检查磁盘空间；
- 下载过程要有并发保护，同一时间只允许一个下载任务；
- 进度通过 `GET /v1/system/server-update` 轮询即可，服务端限流足够；也可后续增加 SSE。

### 4.5 解压与替换范围

服务端 zip 内部通常是一个版本目录，不能假设文件在 zip 根目录：

```text
vault-server-<internal-version>-win64/
  Vault-Server.exe
  config.example.yaml
  scripts/
  updates/
```

替换白名单：

- `Vault-Server.exe`
- `config.example.yaml`
- `scripts/`
- `updates/`（或配置的 `update.artifacts_dir` 目录内容）

**绝不覆盖：**

- `config.yaml`
- `data/`
- `pki/`
- `logs/`
- `steam/`
- `steamcmd/`
- 其它运行期数据目录

如果 `update.artifacts_dir` 是自定义目录，需要把 zip 内的 `updates/` 内容复制到该目录，而不是只更新包内 `updates/`。服务端应把该目录的绝对路径传给 helper。

### 4.6 替换器与重启

不能由运行中的服务端自己替换自己。推荐复用 Agent 已验证的“副本进程”模式：

1. 服务端把当前 exe 复制到 `update/helper/Vault-Server.exe`；
2. 以隐藏/脱离方式启动副本：

   ```text
   Vault-Server.exe apply-update -pending <pending.json>
   ```

3. 服务端写出 `pending.json`，包含：
   - 旧版本 / 新版本
   - 目标 exe 路径
   - 暂存目录
   - 备份路径
   - artifacts 目录
   - 原始启动参数与工作目录
   - 旧进程 PID
4. 服务端返回 202 并优雅停机；
5. helper 等待旧 PID 退出、单实例锁释放；
6. helper 备份旧 exe，替换 exe / scripts / updates；
7. helper 启动新服务端；
8. 新服务端成功启动后等待监听端口可连接（并观察 2 秒确认没有立即退出）；
9. helper 确认成功后清理备份、下载 zip、解压目录；
10. 如果新服务端未启动成功，helper 恢复旧 exe 并重新启动旧版本。

Windows 必须使用独立进程，因为运行中的 exe 不能被覆盖。

Linux 使用 systemd 时要注意：

- 当前 unit 是 `Restart=on-failure`；
- 服务端正常退出（code 0）后 systemd 不会自动重启；
- helper 直接启动新进程会导致 systemd 不再跟踪该服务；
- 推荐 helper 检测 systemd（如 `INVOCATION_ID`），替换文件后执行 `systemctl restart <unit>`；
- 单元名可通过配置指定，例如 `update.systemd_unit`。

建议配置：

```yaml
update:
  restart_mode: auto   # auto | helper | systemd | manual
  systemd_unit: vault-server.service
```

### 4.7 API 设计

仅超级管理员可调用：

```http
GET  /v1/system/server-update
POST /v1/system/server-update/check
POST /v1/system/server-update/download
POST /v1/system/server-update/apply
DELETE /v1/system/server-update/download
```

`GET` 返回示例：

```json
{
  "current": {
    "release_version": "0.3.1",
    "release_channel": "beta",
    "internal_version": "0.3.286"
  },
  "state": "downloading",
  "latest": {
    "version": "0.3.2",
    "channel": "rc",
    "tag": "v0.3.2-rc",
    "notes": "修复……",
    "size_bytes": 122300378,
    "asset_name": "Vault-Server-0.3.2-rc-win64.zip"
  },
  "progress": {
    "received_bytes": 45678901,
    "total_bytes": 122300378,
    "percent": 37
  }
}
```

接口语义：

| 接口 | 行为 |
| --- | --- |
| `GET` | 读取当前状态、进度、上次结果 |
| `POST /check` | 查询 GitHub，返回是否有更新 |
| `POST /download` | 启动下载与校验，返回 202 |
| `POST /apply` | 启动 helper，返回 202 后优雅停机 |
| `DELETE /download` | 取消下载并清理 `.part` |

`apply` 必须遵守：

- 先写 pending；
- 先启动 helper；
- 响应写完并 flush 后再触发停机；
- 停机后由 helper 接管，HTTP 服务不再处理替换动作。

### 4.8 设置页 UI

在 `frontend/packages/ui/src/features/system/SystemSettings.tsx` 增加「服务端更新」卡片：

- 当前发布版本 / 通道
- 当前内部构建号
- 「检查更新」按钮
- 新版本号、更新说明、包大小
- 下载进度条：已下载 / 总大小 / 百分比
- 「下载并重启更新」按钮
- 明确警告：更新会重启服务端，客户端连接和挂载会短暂中断
- 错误提示与上次更新结果
- 旧服务端没有接口时提示“当前服务端不支持在线更新，请手动更新”

同步补充：

- `frontend/packages/ui/src/api/types.ts` 的 `SystemInfo`、更新状态类型；
- `frontend/packages/ui/src/api/endpoints.ts` 的方法；
- i18n 的四种语言文案。

### 4.9 客户端更新复用

服务端更新完成后：

1. 新的 `updates/manifest.json` 与客户端产物已经就位；
2. 客户端继续请求服务端更新接口；
3. 客户端验签、下载、替换逻辑完全不变。

因此“客户端复用之前逻辑”成立，但要注意：

- 管理端页面在客户端里，新增 UI 仍然要改前端；
- 服务端 `update.channel` 配置必须与新的 `manifest.json` channel 一致，否则客户端会收到 `channel_unavailable`；
- 如果更新改变了发布通道，需要在更新流程中明确提示或同步配置。

### 4.10 清理策略

更新成功后删除：

- 下载的旧服务端 zip；
- `.part` 文件；
- 解压暂存目录；
- 旧 `updates/` 中被替换的客户端更新包；
- 旧 exe 备份（新服务端启动成功并稳定监听后删除）。

保留：

- `last-result.json`（供界面展示上次更新结果）；
- 更新日志（排障）；
- 当前版本所需的最小元数据。

---

## 5. 配置项建议

```yaml
update:
  # 已有：客户端更新分发目录
  artifacts_dir: "updates"

  # 已有：客户端更新通道
  channel: "stable"

  # 服务端自更新
  server_auto_check: false
  server_check_interval: "6h"
  # 更新源固定为 https://github.com/junqirao/vault/releases，无需仓库/令牌/代理配置。
  restart_mode: auto        # auto | helper | systemd | manual
  systemd_unit: "vault-server.service"
```

服务端更新源是**写死的官方 Release 页面**，不提供 `github_repo` / `github_token` / `github_proxy` 配置。

---

## 6. 安全设计

推荐优先级：

1. **复用现有 Ed25519 发布签名**：为服务端产物增加签名清单，服务端内置公钥，先验签再下载/执行；
2. 至少校验 GitHub asset `digest` 与文件大小；
3. 固定仓库地址，避免配置被篡改成任意下载源；
4. 只允许 HTTPS；
5. 解压时拒绝 `..`、绝对路径、符号链接与超限文件；
6. 更新接口仅超级管理员，并写审计日志；
7. 不记录 token、签名私钥或完整下载地址中的敏感查询参数。

需要注意：GitHub asset digest 只能保证“传输内容与发布内容一致”，不能防止 GitHub 仓库/账号被攻破。如果执行的是服务端 exe，建议不要只依赖 GitHub 作为信任根。

> 当前实现已校验 GitHub asset `digest`（缺失时回退 `SHA256SUMS.txt`）与文件大小；
> 服务端 exe 的独立发布签名（复用现有 Ed25519 密钥）尚未加入，属于后续增强项。

---

## 7. 失败场景与回滚

| 场景 | 处理 |
| --- | --- |
| GitHub 不可达 / 限流 | 保留上次检查结果，提示稍后重试 |
| 下载中断 | 保留 `.part`，下次 Range 续传 |
| 摘要不符 | 删除 `.part`，拒绝安装 |
| 资产缺失 | 明确返回“当前平台无可用资产” |
| 磁盘不足 | 下载前预检并拒绝 |
| 解压失败 | 清理暂存目录，不动当前版本 |
| 替换失败 | helper 恢复旧 exe 并重启旧版本 |
| 新版本启动失败 | 根据监听/进程存活检查超时回滚 |
| systemd 重启失败 | 保留备份并写失败结果，提示人工处理 |

回滚建议只备份旧 exe，不备份整个 `updates/`（体积大）；新 `updates/` 即使回滚旧 exe 也可以继续使用，因为客户端更新包与旧服务端二进制通常兼容。

---

## 8. 测试计划

### 单元测试

- 发布版本比较：beta / rc / stable、数字预发布、build metadata；
- GitHub Release 选择：预发布过滤、通道过滤、最大版本选择；
- 资产选择：Windows / Linux、缺失资产；
- 下载续传：`.part` 偏移、206/200 分支、摘要失败；
- zip 解压：路径穿越、绝对路径、大小限制；
- 配置解析与敏感字段隐藏。

### 集成测试

- 本地假 GitHub API + 假资产服务：
  - 检查更新；
  - 下载中断后续传；
  - 摘要错误；
  - 替换成功；
  - 启动失败回滚。
- Windows 实机：从旧版本更新到测试 Release，验证客户端能继续更新。
- Linux + systemd 实机：验证替换后 `systemctl restart` 与回滚。

### 手工验收

1. 服务端设置页能看到当前发布版本；
2. 检查更新能看到 rc 新版本；
3. 下载进度正常，断网后继续下载；
4. 下载完成后重启服务端，版本更新；
5. 新服务端的 `updates/` 已经是新客户端包；
6. 客户端仍能按原逻辑检查并更新；
7. 失败场景能回滚到旧版本；
8. 成功后旧的下载 zip 与临时目录被清理。

---

## 9. 发布流水线改动

- `build.py` 从 `RELEASE.json` 读取并注入 `ReleaseVersion` / `ReleaseChannel` / `ReleaseRepo`；
- `release.py` 建议额外发布 `release.json` 资产；
- 默认发布 Linux 服务端资产，或明确声明仅支持 Windows 服务端自更新；
- 服务端更新包建议附带签名信息；
- Release Notes 中保留内部构建号，便于排障。

当前 `release.py` 的 `--channel` 只写 `RELEASE.json`，不会自动传给 `build.py` 的 `update.channel`；发布时应确保两套通道配置一致，或在服务端更新流程中处理通道变化。

---

## 10. 风险与待确认

1. **GitHub 网络可达性**：目标服务器可能无法稳定访问 GitHub；可在服务进程环境中设置 `HTTP_PROXY` / `HTTPS_PROXY`。
2. **信任根**：是否接受 GitHub 作为服务端可执行文件的信任源，还是必须复用 Ed25519 签名。
3. **Linux systemd**：更新后由谁负责重启服务，helper 直接启动还是 `systemctl restart`。
4. **活跃会话**：更新会中断 iSCSI 会话与挂载，是否需要在有活跃租约时阻止或二次确认。
5. **通道一致性**：服务端 `update.channel` 与新 `updates/manifest.json` 的 channel 必须一致。
6. **自定义 artifacts 目录**：必须把新 `updates/` 写入配置目录，而不是只改包内目录。
7. **权限**：Windows 安装目录写权限、Linux root / systemd 权限。
8. **磁盘空间**：服务端 zip 约 120MB，解压后还需要额外空间。

---

## 11. 实施阶段

| 阶段 | 内容 | 预估 |
| --- | --- | --- |
| Phase 1 | 注入发布版本、版本比较、GitHub 检查接口、设置页展示 | 2~3 天 |
| Phase 2 | 下载、断点续传、摘要校验、进度接口与进度条 | 2 天 |
| Phase 3 | Windows helper、pending/backup、重启与回滚 | 3~5 天 |
| Phase 4 | Linux + systemd、服务端签名、自动检查、发布流水线 | 3~5 天 |
| Phase 5 | 单元/集成测试、实机验收、文档 | 2~3 天 |

Windows MVP 约 1 周；Windows + Linux + 签名 + 自动回滚的稳定版本约 2~3 周。

---

## 12. 验收标准

- [ ] 设置页能显示服务端发布版本与内部构建号；
- [ ] 能正确识别 `beta` / `rc` / stable，且不会把旧版本当成新版本；
- [ ] 能从 GitHub Release 选择正确的平台资产；
- [ ] 下载支持进度、取消与断点续传；
- [ ] 服务端更新包下载后经过摘要校验；
- [ ] 替换由独立 helper 完成，运行中的 exe 不被自己覆盖；
- [ ] 新服务端启动成功后才清理旧包；
- [ ] 启动失败能自动回滚；
- [ ] 更新后客户端仍能使用原有更新逻辑获取客户端更新；
- [ ] 成功后旧的下载 zip 与临时文件被删除；
- [ ] 更新操作有审计记录与可读日志。
