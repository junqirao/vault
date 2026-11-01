/**
 * 内部构建号：**正在运行的客户端本体**的版本（安装目录里那一份）。
 *
 * 运行时装填：主进程把本体版本经 preload 注入（见 electron/main.ts 的 additionalArguments），
 * 没有桥接（浏览器 dev / 管理端）时回退构建期常量 __APP_VERSION__（真源是 package.json，
 * 由 vite 的 define 注入，见 vite.config.mts）。用于自动更新比较、兼容性判定与关于页的
 * 诊断信息；**不直接作为对外版本展示**。
 *
 * ⚠️ 不能始终用 __APP_VERSION__：渲染层资源热更（client_web）可以在**不重启本体**的前提下
 * 把界面换成比本体更新的版本。那时界面里的 __APP_VERSION__ 是新界面的版本，而安装目录里的
 * Vault.exe / Vault-Agent.exe 还是旧版本 —— 关于页若拿它当「内部版本」，就会显示成
 * "客户端已更新、只有本地服务还是旧的"（真实工单）。
 */
export const CLIENT_VERSION =
  (typeof window !== 'undefined' ? window.vault?.appVersion?.trim() : '') || __APP_VERSION__

/**
 * 对外发布版本（不带上渠道后缀）。
 *
 * 真源是仓库根目录的 `RELEASE.json`，例如 `0.1.0`；渠道单独给出（如 `beta`）。
 * 侧边栏展示「v0.1.0 + BETA 标签」，发布产物也按这个版本命名。
 */
export const RELEASE_VERSION = __RELEASE_VERSION__

/** 发布渠道：stable / beta / rc / alpha。stable 不展示标签。 */
export const RELEASE_CHANNEL = __RELEASE_CHANNEL__

/** 本地配置文件名（用于界面提示，不含敏感内容）。 */
export const CONFIG_FILE_HINT = '%APPDATA%/Vault/config.json'
