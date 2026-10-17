/// <reference types="node" />
import { defineConfig, devices } from '@playwright/test'

/**
 * 前端回归测试(UI 2.0.1)。
 *
 * **全程不连任何后端。** 每个用例都经 e2e/support/mock.ts 拦下页面发出的全部请求:
 * /api/ 与 /sub/ 一律由固定数据(e2e/fixtures/api.json)或用例里的覆盖来答,
 * 没模拟到的接口直接判失败;写操作没有显式模拟就返回 501 —— 测试里点「确认」
 * 打到的永远是这里,不会是一台真机器。vite 的代理目标也指向一个没人监听的端口,
 * 万一有请求漏过拦截层,拿到的是连接拒绝而不是生产数据。
 *
 * 录制固定数据(只读,对着本机 scratch 面板)见 e2e/README.md。
 */
const record = !!process.env.E2E_RECORD
const preview = !record && (!!process.env.CI || !!process.env.E2E_PREVIEW)
// 三种服务各用各的端口:本机会复用已经在跑的那一个,而录制那一个的代理指向真后端。
const PORT = record ? 4178 : preview ? 4180 : 4179

export default defineConfig({
  testDir: './e2e',
  testMatch: record ? /tools[\\/]record\.spec\.ts$/ : /\.spec\.ts$/,
  testIgnore: record ? [] : ['**/tools/**'],
  // 截图基线按平台分开存:字体渲染在 Windows 与 Linux 上不一样,混用一份只会天天失败。
  snapshotPathTemplate: '{testDir}/__screenshots__/{testFilePath}/{arg}-{platform}{ext}',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : [['list']],
  timeout: 30_000,
  expect: {
    timeout: 7_000,
    // 容差按像素数给,不按比例:整页截图有两百万像素,1% 的比例能放过一整句结论与半张卡片的变化。
    // 基线按平台存,同一台机器上渲染是确定的,50 个像素只给抗锯齿留余地。
    toHaveScreenshot: { animations: 'disabled', caret: 'hide', maxDiffPixels: 50 },
  },
  use: {
    ...devices['Desktop Chrome'],
    baseURL: `http://localhost:${PORT}`,
    viewport: { width: 1440, height: 900 },
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    colorScheme: 'light',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium' }],
  webServer: {
    // CI 先 vite build,再对构建产物跑(更快,也更接近生产包);本机默认用开发服务器。
    command: preview ? `npx vite preview --port ${PORT} --strictPort` : `npx vite --port ${PORT} --strictPort`,
    url: `http://localhost:${PORT}/login`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    env: {
      // 录制时才指向本机 scratch 面板;平时指向一个没人监听的端口(discard)。
      LITEBOX_DEV_API: record ? (process.env.E2E_RECORD_API ?? 'http://127.0.0.1:18100') : 'http://127.0.0.1:9',
    },
  },
})
