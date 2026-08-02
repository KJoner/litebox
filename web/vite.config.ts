import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue()],
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
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
