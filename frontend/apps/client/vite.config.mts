import { readFileSync } from 'node:fs'
import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

/**
 * 构建期注入两类版本号：
 *
 * - `__APP_VERSION__`：内部构建号（package.json 的 version），用于自动更新与兼容性判定；
 * - `__RELEASE_VERSION__` / `__RELEASE_CHANNEL__`：对外发布版本（仓库根目录 RELEASE.json），
 *   只用于界面展示与发布产物命名。
 *
 * 两者刻意分开：内部构建号会随每次改动递增，对外版本只在正式发布时手工调整。
 */
const pkg = JSON.parse(
  readFileSync(fileURLToPath(new URL('./package.json', import.meta.url)), 'utf8')
) as { version: string }

const release = JSON.parse(
  readFileSync(fileURLToPath(new URL('../../../RELEASE.json', import.meta.url)), 'utf8')
) as { version?: string; channel?: string }

if (!release.version) {
  throw new Error('RELEASE.json 缺少 version 字段')
}

export default defineConfig({
  plugins: [react()],
  base: './',
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
    __RELEASE_VERSION__: JSON.stringify(release.version),
    __RELEASE_CHANNEL__: JSON.stringify((release.channel ?? 'stable').trim().toLowerCase())
  },
  resolve: {
    alias: {
      '@vault/ui': fileURLToPath(new URL('../../packages/ui/src/index.ts', import.meta.url)),
      '@vault/ui/': fileURLToPath(new URL('../../packages/ui/src/', import.meta.url))
    }
  },
  server: {
    port: 5173,
    strictPort: true
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 2000
  }
})
