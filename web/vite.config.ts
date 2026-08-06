import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

/**
 * 丢掉 @fontsource 里的 .woff 回退。
 *
 * 每个 @font-face 都写成 `woff2, woff` 两份,Vite 会把两份都打进 dist ——
 * 中文字体分了两百多片,这一项就是 6.4MB。而 woff2 从 2016 年起全线支持,
 * 能跑 Vue 3 + AntD 4 的浏览器没有一个需要 woff 回退。
 *
 * 产物要 embed 进单个二进制、由一键脚本下发到 VPS,这 6.4MB 是每次升级的实打实开销。
 */
function dropWoffFallback(): Plugin {
  return {
    name: 'litebox-drop-woff-fallback',
    enforce: 'pre',
    transform(code, id) {
      if (!id.includes('@fontsource') || !id.endsWith('.css')) return null
      return { code: code.replace(/,\s*url\([^)]+\.woff\)\s*format\('woff'\)/g, ''), map: null }
    },
  }
}

export default defineConfig({
  plugins: [vue(), dropWoffFallback()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // 产物由 Go embed 嵌入二进制,因此固定输出到 dist 且清空旧文件。
    outDir: 'dist',
    emptyOutDir: true,
    // 关闭 sourcemap:面板会被部署到公网,不需要暴露源码结构。
    sourcemap: false,
  },
  server: {
    port: 5173,
    // 开发时前端跑 Vite,API 代理到本地 Go 后端。
    // 端口不是 8080 时用 LITEBOX_DEV_API 覆盖,不用改这个文件。
    proxy: {
      '/api': {
        target: process.env.LITEBOX_DEV_API ?? 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
      // 订阅是公开路由,不在 /api 下。调试订阅内容时要能直接打开。
      '/sub': {
        target: process.env.LITEBOX_DEV_API ?? 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
