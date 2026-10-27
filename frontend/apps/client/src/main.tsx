import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { signalRendererReady } from '@vault/ui'

import { App } from './App'

const container = document.getElementById('root')
if (!container) throw new Error('#root not found')

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>
)

// 首帧画出来之后再告诉主进程"这一层跑起来了"。
//
// 主进程用它判定热更层是否白屏：新层的 JS 若在启动阶段就抛错，这段代码根本不会执行，
// 主进程等不到上报就会回退内置资源（见 electron/main.ts 的 armLayerWatchdog）。
// 连等两帧是为了确保真的画完了，而不是刚提交完 React 的更新就上报。
requestAnimationFrame(() => {
  requestAnimationFrame(() => signalRendererReady())
})
