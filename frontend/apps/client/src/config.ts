/**
 * 内部构建号。
 *
 * 真源是 `package.json` 的 `version`：由 vite 的 define 在构建期注入（见 vite.config.mts）。
 * 用于自动更新比较、兼容性判定与关于页的诊断信息；**不直接作为对外版本展示**。
 */
export const CLIENT_VERSION = __APP_VERSION__

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
