import { test, expect } from './support/mock'
import { mockCanvas, scaleSnapshot } from './support/topologyData'

/**
 * 链路画布的性能验收(A28):固定的 100 / 300 个入口(含共享中继、独立分量、多地址卡片)。
 * 记录从打开页面到卡片全部画出来的时间与布局耗时(写进测试结果的注解),门槛放宽到 8 秒 ——
 * CI 机器快慢不定,这里只拦明显的退化;实测数字记在交付记录里。
 */
for (const total of [100, 300]) {
  test(`${total} 个入口:首次稳定展示与布局耗时`, async ({ page, api }) => {
    mockCanvas(api)
    api.on('GET', '/api/topology', { json: scaleSnapshot(total) })
    const started = Date.now()
    await page.goto('/topology')
    await expect(page.locator('.tc')).toHaveCount(total, { timeout: 15000 })
    const shown = Date.now() - started
    const perf = (await page.locator('.tv__perf').textContent()) ?? ''
    test.info().annotations.push({ type: 'perf', description: `${total} 个入口:首次稳定展示 ${shown} ms;${perf.trim()}` })
    console.log(`[perf] ${total} 个入口:首次稳定展示 ${shown} ms;${perf.trim()}`)
    expect(shown).toBeLessThan(8000)
    // 平移与缩放不应长时间卡住:连续几次缩放之后页面仍然可点。
    const t0 = Date.now()
    for (let i = 0; i < 5; i++) await page.getByRole('button', { name: '放大' }).click()
    await page.getByRole('button', { name: '适应全部' }).click()
    console.log(`[perf] ${total} 个入口:5 次缩放 + 适应全部 ${Date.now() - t0} ms`)
    expect(api.mutations()).toEqual([])
  })
}
