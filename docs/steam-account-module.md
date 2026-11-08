# Steam 账号模块设计文档

> 状态：设计稿  
> 范围：Steam 内容账号的独立管理、许可同步、并发租约，以及与「Steam 游戏组 / 游戏库创建」的解耦集成  
> 关联：`docs/design.md`、`docs/implementation.md`

---

## 1. 背景

Steam 游戏托管需要解决两类问题：

1. **内容从哪里来**：用哪个 Steam 账号登录 SteamCMD，下载/更新哪个 AppID，目标平台是 Windows 还是 Linux。
2. **账号怎么管理**：账号密码、Steam Guard、许可、登录状态、并发占用、失效处理。

此前的初步方案把「绑定 Steam 账号」放进了游戏组/游戏库的创建流程里，会导致：

- 创建表单承担登录、2FA、许可校验等职责；
- 账号凭据和业务配置耦合；
- 一个账号被多个组复用时，创建流程重复处理账号状态；
- 账号失效时，创建流程和更新流程都要各自处理错误。

本设计明确拆开：

> **Steam 账号绑定是独立模块；游戏组/游戏库创建时只从已有账号中选择，不内嵌绑定动作。**

---

## 2. 目标与非目标

### 2.1 目标

- 独立管理 Steam 内容账号：新增、编辑、禁用、删除、测试登录、同步许可。
- 独立处理凭据加密、Steam Guard、登录会话、账号健康检查。
- 独立处理账号并发租约，保证同一账号不会并发跑 SteamCMD。
- 对外提供「按 AppID / 平台查询可用账号」的能力。
- 游戏组/游戏/存储库只保存 `steam_account_id` 外键，不保存凭据。
- 创建流程只做「选择账号」，不出现账号密码、验证码、登录按钮。
- 账号异常只影响更新/下载，不影响已有游戏库的挂载与游玩。

### 2.2 非目标

- 不做玩家游玩账号的自动登录/切号（后续独立设计）。
- 不做 Steam 客户端 GUI 自动化。
- 不做账号交易、共享、租借等合规敏感能力。
- 不在本模块内决定目标平台；平台由调用方（游戏组/游戏）传入。
- 不在本模块内实现磁盘差异、合并、iSCSI；这些继续由现有 `app` 模块负责。

---

## 3. 设计原则

| 原则 | 说明 |
|---|---|
| **单一职责** | 账号模块只负责账号、凭据、登录、2FA、许可、健康、租约 |
| **引用而非内嵌** | 组/游戏只存 `steam_account_id`，不存用户名密码 |
| **创建时只选** | 创建组/游戏时从可用账号列表选择，不在创建表单里绑定 |
| **能力查询** | 通过 `available?app_id=&platform=` 查询可用账号，避免调用方自己判断 |
| **失败隔离** | 账号失效不阻断已有库的挂载和游玩，只暂停更新/新下载 |
| **可替换** | SteamCMD 执行器抽象成接口，未来可替换为 DepotDownloader 或远程 Builder |
| **可审计** | 登录、2FA、许可同步、租约、凭据变更全部审计 |
| **平台无关** | 账号本身不绑定平台；平台由调用方传入 |

### 3.1 反模式

以下是明确不做的：

- ❌ 在「创建 Steam 组」弹窗里输入 Steam 用户名/密码。
- ❌ 创建组时同步执行 Steam 登录并等待验证码。
- ❌ 把账号密码、TOTP secret 存在 `repo_groups` 或 `repositories` 表。
- ❌ 每个组各自维护一份账号凭据。
- ❌ 更新流程直接读取账号密码，绕过租约和健康检查。

正确的做法：

- ✅ 先在「Steam 账号」模块完成账号添加、登录验证、许可同步。
- ✅ 创建组时从下拉框选择 `steam_account_id`。
- ✅ 更新时由账号模块发放租约并注入登录信息。

---

## 4. 模块边界

```text
┌────────────────────────────────────────────────────┐
│ Steam Account Module                               │
│                                                    │
│  AccountService                                    │
│   ├─ CRUD / 状态机                                  │
│   ├─ Credential Vault（加密存储）                   │
│   ├─ Login / Steam Guard / 2FA                     │
│   ├─ License Sync（licenses_print）                │
│   ├─ Health Check                                  │
│   ├─ Lease（账号级并发控制）                        │
│   └─ Selector（按 AppID/平台查可用账号）            │
│                                                    │
│  Runner（SteamCMD 执行器，可替换）                  │
└───────────────┬────────────────────────────────────┘
                │ account_id（外键）
                │ Reader / Leaser / Runner 接口
┌───────────────▼────────────────────────────────────┐
│ Game Group / Game / Repository                     │
│   ├─ 创建时选择 steam_account_id                    │
│   ├─ 更新时申请租约                                 │
│   └─ 不接触凭据                                     │
└────────────────────────────────────────────────────┘
```

依赖方向：

```text
internal/api
    └─ internal/group
          ├─ internal/game
          └─ internal/steamaccount   （通过接口）
                ├─ internal/store
                ├─ internal/secret
                ├─ internal/job
                └─ internal/logging / audit
```

`steamaccount` **不依赖** `group`、`game`、`repo` 包，避免循环依赖。

---

## 5. 总体架构

```text
               ┌──────────────────────────┐
               │ Admin UI                 │
               │  Steam 账号页            │
               │  组创建时账号下拉         │
               └────────────┬─────────────┘
                            │ REST
               ┌────────────▼─────────────┐
               │ API                      │
               │  /v1/steam/accounts/*    │
               │  /v1/repo-groups/*       │
               └────────────┬─────────────┘
                            │
        ┌───────────────────▼───────────────────┐
        │ SteamAccountService                   │
        │  凭据加密 / 状态 / 许可 / 租约 / 选择   │
        └───────────────────┬───────────────────┘
                            │
        ┌───────────────────▼───────────────────┐
        │ SteamCMD Runner                       │
        │  本地（Windows/Linux）或远程 Builder    │
        └───────────────────┬───────────────────┘
                            │
        ┌───────────────────▼───────────────────┐
        │ Steam / SteamCMD                      │
        └───────────────────────────────────────┘
```

---

## 6. 数据模型

### 6.1 steam_accounts

```sql
CREATE TABLE steam_accounts (
  id                    TEXT PRIMARY KEY,
  username              TEXT NOT NULL UNIQUE,
  display_name          TEXT,

  -- 账号状态：active | needs_2fa | disabled | error
  status                TEXT NOT NULL DEFAULT 'needs_2fa',

  -- 凭据：密码或 refresh token，密文存储
  credential_cipher     BLOB NOT NULL,
  credential_kind       TEXT NOT NULL DEFAULT 'password', -- password | refresh_token

  -- Steam Guard
  guard_type            TEXT NOT NULL DEFAULT 'none',      -- none | totp | email
  guard_secret_cipher   BLOB,                              -- TOTP secret，密文存储

  -- 并发控制
  max_concurrent        INTEGER NOT NULL DEFAULT 1,

  -- 健康状态
  last_login_at         INTEGER,
  last_checked_at       INTEGER,
  last_error            TEXT,

  -- SteamCMD 持久化目录（每账号独立，保存 ssfn/config）
  session_home          TEXT,

  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL
);
```

说明：

- `credential_cipher` / `guard_secret_cipher` 永不通过 API 返回。
- `session_home` 用于保存 SteamCMD 的 `ssfn*`、`config`，减少重复 2FA。
- `max_concurrent` 默认 1；大于 1 时租约层按计数控制。

### 6.2 steam_account_apps

```sql
CREATE TABLE steam_account_apps (
  account_id    TEXT NOT NULL REFERENCES steam_accounts(id) ON DELETE CASCADE,
  app_id        INTEGER NOT NULL,
  source        TEXT NOT NULL DEFAULT 'licenses_print', -- licenses_print | manual
  verified_at   INTEGER NOT NULL,
  PRIMARY KEY (account_id, app_id)
);
```

用途：

- 创建游戏时校验该账号是否拥有该 AppID。
- `GET /v1/steam/accounts/available?app_id=` 的过滤依据。
- 许可同步失败时保留旧数据，只标记 `verified_at` 过期。

### 6.3 steam_account_leases

```sql
CREATE TABLE steam_account_leases (
  id            TEXT PRIMARY KEY,
  account_id    TEXT NOT NULL REFERENCES steam_accounts(id) ON DELETE CASCADE,
  holder        TEXT NOT NULL,          -- group_update | game_update | test_login | license_sync
  holder_id     TEXT,                   -- job_id / game_id
  state         TEXT NOT NULL,          -- active | released | expired
  acquired_at   INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  released_at   INTEGER,
  last_error    TEXT
);

-- max_concurrent = 1 时可用部分唯一索引兜底
CREATE UNIQUE INDEX ux_steam_lease_active
  ON steam_account_leases(account_id)
  WHERE state = 'active';
```

说明：

- 租约是账号级，不按组隔离；两个组绑定同一账号也会互斥。
- 长任务需要续租；进程崩溃后租约到期可被回收。
- `max_concurrent > 1` 时不用唯一索引，改为事务内计数。

### 6.4 与组/游戏的关联

```sql
-- 组上只存外键
ALTER TABLE repo_groups
  ADD COLUMN steam_account_id TEXT REFERENCES steam_accounts(id);

-- 游戏可覆盖组账号
ALTER TABLE games
  ADD COLUMN steam_account_id TEXT REFERENCES steam_accounts(id);
```

删除策略：

- 建议账号**软删除**（`status=disabled`），不物理删除。
- 如必须物理删除，外键使用 `ON DELETE SET NULL`，并让相关组/游戏暂停自动更新。
- 不允许在账号仍被组引用时静默删除。

### 6.5 审计

复用现有 `audit_logs`，新增 action：

```text
steam.account.create
steam.account.update
steam.account.disable
steam.account.delete
steam.account.test_login
steam.account.license_sync
steam.account.lease_acquire
steam.account.lease_release
steam.account.guard_update
```

`detail` 中不记录密码、TOTP secret；只记录账号 ID、结果、错误码。

---

## 7. 领域模型与状态机

### 7.1 账号状态

```text
needs_2fa ──验证码通过──> active
needs_2fa ──密码错误/被锁──> error
active ──需要验证码──> needs_2fa
active ──密码变更/风控/锁定──> error
active ──管理员禁用──> disabled
error ──修复后测试登录成功──> active
disabled ──管理员启用 + 测试成功──> active
```

| 状态 | 含义 | 是否可用于更新 |
|---|---|---|
| `active` | 已登录验证，许可可用 | ✅ |
| `needs_2fa` | 需要验证码或手机令牌确认 | ❌ |
| `error` | 密码错误、被锁、网络/风控异常 | ❌ |
| `disabled` | 管理员主动禁用 | ❌ |

### 7.2 账号能力

```go
type Account struct {
    ID           string
    Username     string
    DisplayName  string
    Status       Status
    GuardType    GuardType
    MaxConcurrent int
    LastLoginAt  time.Time
    LastError    string
}

type Capability struct {
    Available      bool
    OwnsApp        bool
    PlatformOK     bool   // 由调用方平台 + 账号能力共同判断
    Busy           bool
    Reason         string
}
```

账号模块本身**不存储平台**。平台是调用方传入的查询/执行参数：

```go
selector.Available(ctx, AvailableQuery{
    AppID:   1091500,
    Platform: "windows", // 由组/游戏传入
})
```

---

## 8. 凭据与安全

### 8.1 加密

复用 `internal/secret`：

```go
key := secret.LoadOrCreateMasterKey(cfg.Security.MasterKey)
cipher, _ := secret.NewCipher(key)

enc, _ := cipher.EncryptString(password)
plain, _ := cipher.DecryptString(enc)
```

要求：

- 主密钥来自配置/密钥文件，不写进数据库。
- 密文存 `BLOB`；API DTO 不包含该字段。
- 日志中禁止打印密码、TOTP secret、完整命令行参数。
- 错误信息只返回错误码和账号 ID，不返回凭据。

### 8.2 凭据类型

支持两种：

| 类型 | 说明 | 适用 |
|---|---|---|
| `password` | 用户名 + 密码 +（可选）Steam Guard 验证码 | 通用 |
| `refresh_token` | 通过 SteamKit/steam-session 获取的刷新令牌 | 减少重复登录，后续增强 |

MVP 先做 `password + TOTP`，`refresh_token` 作为后续能力。

### 8.3 Steam Guard

| guard_type | 处理方式 |
|---|---|
| `none` | 直接登录（不推荐） |
| `totp` | 服务端用加密存储的 TOTP secret 生成验证码 |
| `email` | 无法自动读取邮件；需要管理员在账号模块手动输入一次验证码 |

要求：

- 优先使用 `totp`。
- `email` 模式只允许手动验证，不允许在创建流程里等待。
- 验证码失败要退避，避免触发 Steam 风控。
- 每个账号使用独立 `session_home`，持久化 `ssfn*`，减少验证频率。

### 8.4 权限

- 账号 CRUD、测试登录、同步许可：`super_admin`。
- 组/游戏的创建者：只能**读取可用账号列表**并选择，不能查看/修改凭据。
- 账号详情中的 `last_error` 可以展示，但不能包含密码或验证码。

---

## 9. SteamCMD 会话

### 9.1 执行器接口

```go
type Runner interface {
    // 检查 App 信息，返回 buildid
    CheckAppInfo(ctx context.Context, req InfoRequest) (*AppInfo, error)

    // 更新 App
    UpdateApp(ctx context.Context, req UpdateRequest) (*UpdateResult, error)

    // 同步许可
    SyncLicenses(ctx context.Context, accountID string) ([]int, error)

    // 测试登录
    TestLogin(ctx context.Context, accountID string) error
}

type InfoRequest struct {
    AccountID string
    AppID     int
    Platform  string // windows | linux
}

type UpdateRequest struct {
    AccountID string
    AppID     int
    Platform  string // windows | linux
    InstallDir string
    Validate  bool
}
```

平台由调用方传入，账号模块只负责把它拼进 SteamCMD 参数。

### 9.2 登录注入

账号模块负责：

- 解密凭据；
- 生成 TOTP（如需要）；
- 注入 `+login`；
- 管理 `session_home`；
- 申请/释放租约。

调用方只关心 `AppID`、`Platform`、`InstallDir`。

### 9.3 命令示例

检查版本：

```bash
steamcmd \
  +@sSteamCmdForcePlatformType windows \
  +login "$STEAM_USER" "$STEAM_PASS" "$GUARD_CODE" \
  +app_info_print 1091500 \
  +quit
```

更新：

```bash
steamcmd \
  +@sSteamCmdForcePlatformType windows \
  +login "$STEAM_USER" "$STEAM_PASS" "$GUARD_CODE" \
  +force_install_dir /srv/steam-stage/1091500 \
  +app_update 1091500 \
  +quit
```

同步许可：

```bash
steamcmd \
  +login "$STEAM_USER" "$STEAM_PASS" "$GUARD_CODE" \
  +licenses_print \
  +quit
```

要求：

- 一个账号同一时刻只允许一个 SteamCMD 进程。
- 一个进程只更新一个 AppID（平台参数是进程级全局配置）。
- 密码/验证码通过进程参数传递时注意进程列表泄露风险；如平台支持，优先使用环境变量或临时凭据文件，并在使用后清理。

---

## 10. 许可同步

### 10.1 触发时机

- 账号首次验证成功后。
- 管理员手动点击「同步许可」。
- 每日定时（可配置）。
- 创建游戏前按需校验单个 AppID。

### 10.2 解析

`licenses_print` 输出中包含已授权的 AppID 列表。解析后：

```text
steam_account_apps(account_id, app_id, source='licenses_print', verified_at=now)
```

失败处理：

- 保留旧许可数据；
- 标记 `last_checked_at` 和 `last_error`；
- 不因为许可同步失败而禁用账号。

### 10.3 可用账号查询

```http
GET /v1/steam/accounts/available?app_id=1091500&platform=windows
```

返回：

```json
{
  "items": [
    {
      "id": "acc_01",
      "username": "content-01",
      "display_name": "内容账号 1",
      "status": "active",
      "owns_app": true,
      "busy": false,
      "max_concurrent": 1,
      "reason": ""
    }
  ]
}
```

过滤规则：

1. `status=active`；
2. `steam_account_apps` 包含 AppID，或该 App 允许匿名/免费下载；
3. 当前活跃租约数 < `max_concurrent`；
4. 平台能力匹配（如果账号模块记录了平台限制；MVP 可全部视为支持）。

---

## 11. 并发租约与健康检查

### 11.1 租约

```go
lease, err := leaser.Acquire(ctx, AcquireRequest{
    AccountID: accountID,
    Holder:    "game_update",
    HolderID:  jobID,
    TTL:       30 * time.Minute,
})
defer leaser.Release(ctx, lease.ID)
```

规则：

- 同一账号默认 `max_concurrent=1`，用部分唯一索引兜底。
- 长任务每 5 分钟续租。
- 进程崩溃后，租约在 TTL 后过期；新的任务可以抢占。
- 租约获取失败返回 `steam.account_busy`，调用方排队或稍后重试。
- 租约期间账号状态变为 `error` 时，任务应终止并释放租约。

### 11.2 健康检查

- 定时任务：每 N 小时对 `active` 账号做一次轻量登录测试。
- 手动：账号详情页「测试登录」。
- 结果更新 `status`、`last_checked_at`、`last_error`。
- 连续失败要退避，避免触发 Steam 风控。
- 健康检查失败只暂停相关组的自动更新，不影响已有游戏库挂载。

---

## 12. 与创建流程的集成

### 12.1 核心规则

> **创建组/游戏时只选择已有账号，不执行绑定。**

- 绑定动作发生在「Steam 账号」独立模块。
- 创建流程只保存 `steam_account_id`。
- 不在创建弹窗里输入密码、验证码，不调用 SteamCMD。

### 12.2 创建 Steam 组

表单：

```text
组名称：        [单机游戏组        ]
组类型：        [Steam 游戏组     ]
Steam 账号：    [内容账号 1 ▼]     ← 来自账号模块
目标平台：      [Windows ▼]
自动更新：      [开]
检查周期：      [每 6 小时]
更新时间窗：    [03:00 - 06:00]
最大并发：      [1]
```

如果账号列表为空：

```text
还没有可用的 Steam 账号。
请先到「Steam 账号」模块添加并验证账号。
[前往 Steam 账号]
```

创建时不执行：

- 不登录 Steam；
- 不校验密码；
- 不等待 2FA；
- 不下载游戏。

可选的轻量校验：

- 账号存在；
- 账号 `status=active`；
- 如果已选 AppID，检查 `steam_account_apps` 是否包含该 AppID。

### 12.3 添加游戏到组

```text
AppID：         [1091500        ]
游戏名称：      [自动获取/手填  ]
下载平台：      [继承组：Windows ▼]
Steam 账号：    [继承组：内容账号 1 ▼]
```

校验：

- AppID 合法；
- 账号拥有该 AppID（或允许匿名）；
- 平台与客户端的文件系统匹配（Windows → NTFS，Linux → ext4）。

失败提示：

```text
当前账号未拥有该游戏，请先到 Steam 账号模块同步许可，
或选择其它拥有该游戏的账号。
```

### 12.4 更新流程

```text
GroupScheduler
  → 读取 group.steam_account_id / game.steam_account_id
  → steamaccount.Acquire(accountID, holder=job)
  → steamaccount.CheckAppInfo(appID, platform)
  → 有更新：
      enter maintenance
      steamaccount.UpdateApp(appID, platform, installDir)
      StartDiffFromDir
      MergeDiff
  → Release(lease)
```

账号异常：

- `needs_2fa` / `error` / `disabled` → 跳过更新，标记组/游戏状态，通知管理员；
- 已有游戏库仍可挂载游玩；
- 管理员修复账号后，调度器自动恢复。

### 12.5 账号删除/禁用

- 禁用：立即停止新的更新任务；已有租约允许完成或强制释放（可配置）。
- 删除：建议禁止；如允许，组/游戏的 `steam_account_id` 置空，自动更新暂停。
- UI 必须提示影响范围：

```text
该账号被 3 个游戏组使用。
禁用后这些组的自动更新会暂停，但已有游戏仍可游玩。
```

---

## 13. API 设计

### 13.1 账号模块

```text
GET    /v1/steam/accounts
POST   /v1/steam/accounts
GET    /v1/steam/accounts/{id}
PATCH  /v1/steam/accounts/{id}
DELETE /v1/steam/accounts/{id}

POST   /v1/steam/accounts/{id}/test-login
POST   /v1/steam/accounts/{id}/sync-licenses
GET    /v1/steam/accounts/{id}/apps
GET    /v1/steam/accounts/{id}/leases

GET    /v1/steam/accounts/available?app_id=&platform=
```

创建请求：

```json
{
  "username": "content-01",
  "display_name": "内容账号 1",
  "credential": "********",
  "credential_kind": "password",
  "guard_type": "totp",
  "guard_secret": "********",
  "max_concurrent": 1
}
```

响应永不包含 `credential` / `guard_secret`：

```json
{
  "id": "acc_01",
  "username": "content-01",
  "display_name": "内容账号 1",
  "status": "needs_2fa",
  "guard_type": "totp",
  "max_concurrent": 1,
  "last_login_at": null,
  "last_error": ""
}
```

### 13.2 组/游戏引用

```text
POST /v1/repo-groups
{
  "name": "单机游戏组",
  "kind": "steam",
  "steam_account_id": "acc_01",
  "target_platform": "windows",
  "auto_update": true,
  "update_schedule": "0 */6 * * *",
  "update_window": "03:00-06:00"
}

POST /v1/repo-groups/{id}/games
{
  "app_id": 1091500,
  "steam_account_id": null,   // 继承组
  "target_platform": null     // 继承组
}
```

没有单独的「绑定账号」接口：

- 账号绑定在账号模块完成；
- 组/游戏只是引用 `steam_account_id`；
- 修改组账号 = `PATCH /v1/repo-groups/{id}` 更新外键。

---

## 14. UI 设计

### 14.1 独立「Steam 账号」页面

位置：管理端 → Steam 账号（或设置 → 集成）。

列表列：

```text
用户名 | 显示名 | 状态 | Guard | 拥有游戏数 | 当前租约 | 最近登录 | 最近错误 | 操作
```

操作：

- 添加账号
- 编辑
- 测试登录
- 同步许可
- 禁用/启用
- 查看详情（许可、租约、审计）

添加/编辑弹窗：

```text
用户名：        [            ]
密码/Token：    [            ]   ← 仅写入，不回显
凭据类型：      [密码 ▼]
Steam Guard：   [TOTP ▼]
Guard Secret：  [            ]   ← 仅写入，不回显
最大并发：      [1]
[保存] [测试登录]
```

要求：

- 密码/secret 输入框不显示已有值，只显示「已设置」。
- 测试登录结果实时展示，但不展示凭据。
- 2FA 需要验证码时，在账号详情页处理，不在组创建页处理。

### 14.2 组设置中的账号选择

在「Steam 游戏组」Tab 中：

```text
Steam 账号： [内容账号 1 ▼]  [管理账号]
```

- 下拉只显示可用账号；
- 「管理账号」跳转到独立页面；
- 没有账号时显示引导；
- 不在组设置里出现密码/验证码输入框。

### 14.3 添加游戏

- 选择 AppID 后，显示该账号的许可状态；
- 未拥有时提示去账号模块同步许可；
- 允许临时选择另一个拥有该游戏的账号（游戏级覆盖）。

### 14.4 状态展示

组卡片/游戏卡片：

```text
Steam · Windows · 自动更新开
账号：内容账号 1（正常）
```

异常：

```text
账号需要 2FA，自动更新已暂停
[去处理]
```

---

## 15. 代码结构

```text
internal/steamaccount/
  types.go          # Account, App, Lease, Status, Capability
  service.go        # CRUD、状态机、审计
  credential.go     # 加密/解密、密码/Token
  guard.go          # TOTP、验证码
  steamcmd.go       # Runner 实现：login/check/update/licenses
  license.go        # licenses_print 解析、同步
  lease.go          # 租约获取/续租/释放
  health.go         # 健康检查
  selector.go       # 可用账号查询
  runner.go         # Runner 接口 + fake runner（测试）
```

接口：

```go
type Reader interface {
    Get(ctx context.Context, id string) (*Account, error)
    ListAvailable(ctx context.Context, q AvailableQuery) ([]*Account, error)
    Owns(ctx context.Context, accountID string, appID int) (bool, error)
}

type Leaser interface {
    Acquire(ctx context.Context, req AcquireRequest) (*Lease, error)
    Renew(ctx context.Context, leaseID string, ttl time.Duration) error
    Release(ctx context.Context, leaseID string) error
}

type Runner interface {
    CheckAppInfo(ctx context.Context, req InfoRequest) (*AppInfo, error)
    UpdateApp(ctx context.Context, req UpdateRequest) (*UpdateResult, error)
    SyncLicenses(ctx context.Context, accountID string) ([]int, error)
    TestLogin(ctx context.Context, accountID string) error
}
```

组/游戏模块只依赖 `Reader`、`Leaser`、`Runner` 接口，不依赖 `steamaccount` 内部实现。

---

## 16. 关键时序

### 16.1 添加并验证账号

```text
Admin                API                  AccountService           SteamCMD
  │                   │                        │                     │
  │ POST /accounts    │                        │                     │
  │──────────────────>│                        │                     │
  │                   │ encrypt credential     │                     │
  │                   │───────────────────────>│                     │
  │                   │                        │ create account      │
  │                   │                        │ status=needs_2fa    │
  │                   │                        │                     │
  │ POST /test-login  │                        │                     │
  │──────────────────>│                        │ acquire lease       │
  │                   │                        │────────────────────>│
  │                   │                        │ login + guard       │
  │                   │                        │<────────────────────│
  │                   │                        │ active              │
  │                   │                        │ sync licenses       │
  │                   │                        │ release lease       │
  │                   │<───────────────────────│                     │
  │<──────────────────│                        │                     │
```

### 16.2 创建组时选择账号

```text
Admin                API                  GroupService          AccountService
  │                   │                        │                     │
  │ GET /accounts/available?app_id=...         │                     │
  │──────────────────────────────────────────────────────────────────>│
  │<──────────────────────────────────────────────────────────────────│
  │ POST /repo-groups {steam_account_id}       │                     │
  │──────────────────>│───────────────────────>│                     │
  │                   │                        │ validate FK/status  │
  │                   │                        │────────────────────>│
  │                   │                        │ save group          │
  │<──────────────────│                        │                     │
```

创建过程中没有 SteamCMD 登录。

### 16.3 自动更新时占用账号

```text
Scheduler            GroupService           AccountService          SteamCMD
  │                   │                        │                     │
  │ scan group        │                        │                     │
  │──────────────────>│                        │                     │
  │                   │ acquire(account_id)    │                     │
  │                   │───────────────────────>│                     │
  │                   │<───────────────────────│ lease               │
  │                   │ CheckAppInfo           │                     │
  │                   │─────────────────────────────────────────────>│
  │                   │<─────────────────────────────────────────────│
  │                   │ UpdateApp（如需）       │                     │
  │                   │─────────────────────────────────────────────>│
  │                   │ diff + merge           │                     │
  │                   │ release lease          │                     │
  │                   │───────────────────────>│                     │
```

---

## 17. 错误处理

| 场景 | 行为 | 用户提示 |
|---|---|---|
| 密码错误 | `status=error`，记录 `last_error`，退避 | 账号凭据无效，请更新密码 |
| 需要 2FA | `status=needs_2fa` | 需要在 Steam 账号模块完成验证 |
| 账号被锁/风控 | `status=error`，停止自动重试 | 账号被限制，请稍后处理 |
| 许可同步失败 | 保留旧许可，标记过期 | 许可同步失败，可手动重试 |
| 账号被占用 | 返回 `steam.account_busy` | 账号正在执行其它任务，已排队 |
| 账号未拥有游戏 | 拒绝添加/更新 | 该账号未拥有此游戏 |
| 账号被删除 | 组/游戏外键置空，暂停更新 | 账号已被删除，请重新选择 |
| 平台不匹配 | 创建时校验失败 | 目标平台与客户端文件系统不兼容 |
| SteamCMD 超时 | 释放租约，任务重试 | SteamCMD 超时，稍后重试 |

错误码建议：

```text
steam.account_not_found
steam.account_disabled
steam.account_needs_2fa
steam.account_invalid_credential
steam.account_busy
steam.account_no_license
steam.account_license_stale
steam.account_platform_unsupported
steam.account_steamcmd_failed
steam.account_lease_expired
```

---

## 18. 迁移与兼容

- 新增表，不改现有账号/用户表。
- `repo_groups.steam_account_id` 可空；普通组不受影响。
- 旧的分组挂载配置 `agent.group_mounts` 继续按组名工作。
- 组/游戏的 DTO 增加 `steam_account_id`、`steam_account_status`，不返回凭据。
- 迁移时不自动创建账号；管理员需要先在账号模块添加。
- 功能开关：`steam.enabled`，关闭时隐藏 Steam 账号页和组设置中的 Steam Tab。
- 如果未来支持账号池，新增 `steam_account_pools`，组引用 pool_id；当前先保持单账号外键。

---

## 19. 测试策略

### 19.1 单元测试

- 凭据加解密、密文不泄露。
- 状态机迁移。
- TOTP 生成与验证。
- `licenses_print` 解析。
- 租约获取/续租/过期/抢占。
- 可用账号过滤规则。

### 19.2 集成测试

- 使用 fake Runner：
  - 创建账号 → 测试登录 → 许可同步；
  - 创建组引用账号 → 添加游戏 → 校验许可；
  - 自动更新获取租约 → 执行 → 释放；
  - 账号异常时更新暂停，已有库仍可挂载。

### 19.3 安全测试

- API 响应不包含密码/TOTP。
- 日志不包含凭据。
- 审计记录完整。
- 并发更新同一账号只允许一个。

### 19.4 E2E

- 管理员添加账号并验证。
- 创建 Steam 组并选择账号。
- 添加游戏并触发手动更新。
- 账号失效后 UI 提示正确。

---

## 20. 分阶段落地

### Phase 1：独立账号模块（MVP）

- `steam_accounts` / `steam_account_apps` / `steam_account_leases`。
- 账号 CRUD、凭据加密、状态展示。
- 独立「Steam 账号」页面。
- 手动测试登录（可用 fake Runner 先跑通流程）。
- 组创建时账号下拉选择。

### Phase 2：SteamCMD 自动化

- SteamCMD Runner：登录、`app_info_print`、`app_update`、`licenses_print`。
- TOTP 支持、独立 `session_home`。
- 许可同步与可用账号查询。
- 租约与健康检查。

### Phase 3：更新集成

- 组/游戏自动更新调度。
- 账号级并发控制。
- 失败退避、通知、审计。
- 账号异常时暂停更新。

### Phase 4：增强

- `refresh_token` 支持。
- 账号池（一个组绑定多个账号）。
- 远程 Builder。
- 账号使用统计与配额。

---

## 21. 开放问题

| 问题 | 当前建议 |
|---|---|
| 用密码还是 refresh token？ | MVP 密码 + TOTP；后续支持 refresh token |
| 邮箱 Guard 怎么办？ | 只支持手动输入验证码；优先推荐 TOTP |
| 一个组一个账号还是账号池？ | 先一个账号；后续加 pool |
| 是否支持匿名/免费游戏？ | 可以作为特殊账号模式，后续评估 |
| 游戏是否允许覆盖组账号？ | 允许，用于个别游戏需要不同账号 |
| SteamCMD 跑在哪？ | 先本地（Windows/Linux）；后续远程 Builder |
| 账号删除策略？ | 默认软删除；物理删除需先解绑所有组 |
| 平台是否记在账号上？ | 不记；平台由组/游戏传入 |

---

## 22. 结论

本设计的核心是**解耦**：

- Steam 账号是独立资源，有自己的生命周期、凭据、状态、许可和租约。
- 游戏组/游戏库只是**引用**账号，不负责绑定。
- 创建流程只做「选择」，不做「登录」。
- 更新流程通过账号模块申请租约，不直接碰凭据。
- 账号异常只影响更新，不影响已有游戏库的挂载和游玩。

这样既满足「账号绑定单独一个模块」，又保证「创建时直接选」，同时最大限度复用 Vault 现有的存储、差异盘、合并、任务、审计和客户端挂载能力。
