import { computed, ref, watch } from 'vue'

const STORE_PREFIX = 'litebox.pageSize.'
const SIZES = [10, 20, 50, 100]

/**
 * 表格分页。四张长表共用一份实现,免得每页各写一遍页码状态。
 *
 * 三条来自实际使用的规则:
 *
 * 一、**每页条数记住。** 管理员把它调成 50 之后,翻一页或者切到另一张表
 *     又被重置回 20,他会再调一次,然后放弃。按表分别记 —— 审计日志想看 100 条
 *     不代表节点列表也要 100 条。
 *
 * 二、**总数变小时把页码收回第一页。** 停在第 4 页时筛掉大半数据,
 *     AntD 会老老实实渲染一个空白的第 4 页,看起来像「筛完一条都没有」。
 *
 * 三、**不放「跳至第 N 页」。** 这个量级的数据翻页就够了,输入框只是多一个
 *     要对齐的控件。
 */
export function usePagination(key: string, total: () => number, defaultSize = 20) {
  const pageSize = ref(readSize(key, defaultSize))
  const current = ref(1)

  watch(total, () => {
    const max = Math.max(1, Math.ceil(total() / pageSize.value))
    if (current.value > max) current.value = 1
  })

  function setSize(size: number) {
    pageSize.value = size
    current.value = 1
    try {
      localStorage.setItem(STORE_PREFIX + key, String(size))
    } catch {
      // 隐私模式下 localStorage 会抛异常。记不住而已,不该让页面挂掉。
    }
  }

  /** 直接交给 a-table 的 :pagination。 */
  const options = computed(() => ({
    current: current.value,
    pageSize: pageSize.value,
    total: total(),
    showSizeChanger: true,
    pageSizeOptions: SIZES.map(String),
    size: 'small' as const,
    showTotal: (t: number, range: [number, number]) =>
      t === 0 ? '共 0 条' : `第 ${range[0]}–${range[1]} 条 · 共 ${t} 条`,
    onChange: (page: number, size: number) => {
      if (size !== pageSize.value) setSize(size)
      else current.value = page
    },
  }))

  /** 窄屏的卡片列表自己切片 —— 否则手机上会一次铺出全部行。 */
  function slice<T>(items: T[]): T[] {
    const start = (current.value - 1) * pageSize.value
    return items.slice(start, start + pageSize.value)
  }

  return { options, current, pageSize, slice, setSize }
}

function readSize(key: string, fallback: number): number {
  try {
    const raw = Number(localStorage.getItem(STORE_PREFIX + key))
    return SIZES.includes(raw) ? raw : fallback
  } catch {
    return fallback
  }
}
