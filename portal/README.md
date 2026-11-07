# Vault 门户页

`portal/index.html` 是一个静态产品页，直接用浏览器打开即可，不需要构建，也没有外部依赖。

页面里的截图来自 `frontend/apps/client/demo.html`：它用真实的 `@vault/ui` 组件渲染，只把 API 和 Agent 的返回替换成了 `demo/mockData.ts` 里的模拟数据。界面布局、状态口径和交互与正式客户端一致，存储库名、用户、容量和曲线是编的。

## 页面结构

页面是一个 5 屏的垂直滚动 deck，支持 scroll-snap、右侧章节导航、右下角“下一节”入口、上下方向键和翻页键。浅色底 + 蓝色强调色，背景带网格、光斑、同心圆和斜线。

1. Hero：存储共享 & 端侧加速，GitHub 入口，存储库 + 首页截图
2. 性能：CrystalDiskMark 参考数据、按库性能视图、参考说明
3. 存储共享：基础盘 / 独立盘结构、生命周期、存储库详情和存储列表
4. 旁路加速：数据路径、缓存状态截图、命中/未命中/写路径
5. 客户端与部署：用户管理、首页、构建命令、限制、下载和 GitHub 入口

页面不展示产品版本号；截图使用收起侧边栏的客户端界面，也不带构建号。

## 多语言

页面内置简体中文和英文。首次打开按浏览器时区选择：

- 中国 / 香港 / 澳门 / 台湾时区 → 简体中文
- 其他时区 → 英文

右上角可以手动切换，选择会写入 `localStorage`。也可以用 `?lang=zh-CN` 或 `?lang=en-US` 强制指定语言。

## 下载

页面里的“下载最新版本”链接指向：

`https://github.com/junqirao/vault/releases`

它使用 releases 列表页，当前没有 Release 时不会 404；发布后进入就是最新版本列表。

## 重新截图

先在 `frontend/apps/client` 下启动 Vite：

```powershell
C:\Users\89412\Projects\golang\vault\frontend\node_modules\.bin\vite.cmd --config vite.config.mts --host 127.0.0.1
```

然后按需要打开：

| 页面 | URL |
| --- | --- |
| 存储库分组总览 | `http://127.0.0.1:5173/demo.html?screen=overview` |
| Game 分组卡片 | `http://127.0.0.1:5173/demo.html?screen=repos` |
| 存储库详情 | `http://127.0.0.1:5173/demo.html?screen=detail` |
| 磁盘页签 | `http://127.0.0.1:5173/demo.html?screen=disks` |
| 存储用量 | `http://127.0.0.1:5173/demo.html?screen=storages` |
| 管理端用户 | `http://127.0.0.1:5173/demo.html?screen=users` |
| 管理端存储 | `http://127.0.0.1:5173/demo.html?screen=admin` |
| 性能详情 | `http://127.0.0.1:5173/demo.html?screen=performance` |

无头 Edge 截图示例（Windows）：

```powershell
& "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" `
  --headless=new --disable-gpu --hide-scrollbars `
  --window-size=1600,1000 --virtual-time-budget=12000 `
  --screenshot="$env:TEMP\vault-overview.png" `
  "http://127.0.0.1:5173/demo.html?screen=overview"
```

`--virtual-time-budget` 给性能曲线和顶栏延迟一点时间，避免截到首帧的 `0 ms` / 空图。

## 资产

`assets/` 里是页面实际使用的图片：

- `dashboard.png`：我的存储库分组总览
- `repositories.png`：Game 分组的存储库卡片
- `repository-detail.png`：存储库详情概览
- `admin-users.png`：管理端用户管理
- `admin-storage.png`：管理端存储列表
- `performance-panel.png`：性能详情，按库拆分
- `cache-detail.png`：性能详情顶部曲线，用于旁路加速页
- `benchmark.png`：仓库原有的 CrystalDiskMark 对比图（`docs/img/performance.png`）

`demo/` 目录和 demo 入口只在开发时使用，不影响正式前端构建和客户端打包。
