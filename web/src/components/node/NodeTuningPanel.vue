<script setup lang="ts">
import { computed, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type TuneItem,
  type TuneReport,
  type TuneState,
} from '@/api/client'
import { LbStatusTag, lbDangerConfirm, type LbStatusMeta } from '@/components/lb'
import { formatBytes } from '@/utils/format'
import { color } from '@/theme/tokens'

/**
 * 节点 TCP 调优面板。
 *
 * 分两步:先只读检查给出方案,再由管理员决定是否应用。方案里的每个数字都
 * 由这台机器的内存现算,所以**必须先让他看见算出来的是什么**——
 * 一个直接下发的「一键优化」按钮,在 128MB 的小鸡和 4GB 的机器上写的是
 * 完全不同的值,而两次点击看起来一模一样。
 *
 * 每一项都并排显示当前值与目标值。只给目标值的话,管理员看不出这次到底
 * 改了什么;而「什么都没改」和「改了二十项」应该长得不一样。
 */
const props = defineProps<{ nodeId: number; nodeName: string; report: TuneReport }>()
const emit = defineEmits<{
  close: []
  'update:report': [report: TuneReport]
  /** 有动作在跑时抬起,抽屉据此屏蔽遮罩点击与 ESC */
  busy: [label: string]
}>()

const running = ref('')
/** 默认只列需要留意的项。二十几行全展开会把结论淹掉。 */
const showAll = ref(false)

const stateMeta: Record<TuneState, LbStatusMeta> = {
  // 六种状态六种形状。打印、投屏、色觉障碍下颜色全部失效,而
  //「容器里改不了」和「写了没生效」的处置方式完全不同 —— 只靠颜色分不出来。
  PENDING: { text: '待应用', shape: 'triangle', fg: color.brand, bg: color.brandBg, bd: color.brandBorder },
  SAME: { text: '已一致', shape: 'minus', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder },
  APPLIED: { text: '已生效', shape: 'check', fg: color.success, bg: color.successBg, bd: color.successBorder },
  UNSUPPORTED: { text: '内核不支持', shape: 'ring', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder },
  READONLY: {
    text: '容器里改不了', shape: 'square',
    fg: color.maintenance, bg: color.maintenanceBg, bd: color.maintenanceBorder,
  },
  FAILED: { text: '未生效', shape: 'cross', fg: color.danger, bg: color.dangerBg, bd: color.dangerBorder },
}

const facts = computed(() => props.report.facts)

const modeTitle = computed(() => {
  switch (props.report.mode) {
    case 'APPLY':
      return 'TCP 调优已应用'
    case 'RESTORE':
      return '已还原到调优前'
    default:
      return 'TCP 调优检查完成 · 尚未改动节点'
  }
})

/** 需要留意的项:一致的那些没有信息量。 */
const notable = computed(() => props.report.items.filter((i) => i.state !== 'SAME'))
const visibleItems = computed(() => (showAll.value ? props.report.items : notable.value))

/** 按分组切段,标题只在换组时出现。 */
const grouped = computed(() => {
  const out: { group: string; items: TuneItem[] }[] = []
  for (const item of visibleItems.value) {
    const last = out[out.length - 1]
    if (last && last.group === item.group) last.items.push(item)
    else out.push({ group: item.group, items: [item] })
  }
  return out
})

const failedCount = computed(() => props.report.items.filter((i) => i.state === 'FAILED').length)

async function refresh() {
  await run('重新检查', () => api.tuningPreview(props.nodeId))
}

async function run(label: string, fn: () => Promise<TuneReport>) {
  running.value = label
  emit('busy', label)
  try {
    emit('update:report', await fn())
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    running.value = ''
    emit('busy', '')
  }
}

/**
 * 应用是可逆的(有原值快照),所以用危险确认档而不是输入名称档。
 *
 * 故意不 return doApply() 的 Promise:AntD 只要拿到 Promise 就把确认框
 * 留在屏幕上转圈等它 resolve,而进度是显示在下面这块面板里的 ——
 * 确认框压着不走,管理员反而看不到自己等的东西。
 */
function confirmApply() {
  lbDangerConfirm({
    title: `对 ${props.nodeName} 应用 TCP 调优?`,
    okText: '应用',
    okType: 'primary',
    impacts: [
      '改这台机器的内核参数,并写入 ' + props.report.conf_path + ' 让它熬过重启',
      '不重启任何服务、不断开任何连接 —— 拥塞算法与缓冲区只对新建连接生效',
      props.report.baseline_present
        ? '节点上已有调优前的原值快照,随时可以一键还原'
        : '第一次应用会先把原值快照下来,之后随时可以一键还原',
      '只影响这一台机器,与用户、订阅、配置版本无关,不需要重新部署',
    ],
    footer: '每个数值都按这台机器的内存现算,不是一份固定模板。',
    onOk: () => {
      void run('应用调优', () => api.tuningApply(props.nodeId))
    },
  })
}

function confirmRestore() {
  lbDangerConfirm({
    title: `把 ${props.nodeName} 的内核参数还原到调优前?`,
    okText: '还原',
    impacts: [
      '按原值快照逐项写回,并删掉 ' + props.report.conf_path,
      '同样不重启服务、不断开连接',
      '快照保留,还原之后仍然可以再次调优',
    ],
    onOk: () => {
      void run('还原', () => api.tuningRestore(props.nodeId))
    },
  })
}
</script>

<template>
  <section class="tn">
    <div class="tn__head">
      <span class="tn__title">{{ modeTitle }}</span>
      <a @click="emit('close')">收起</a>
    </div>

    <!-- 依据先摆出来。方案是按这几个数算的,不显示的话「为什么给我这个值」
         永远没有答案。 -->
    <div class="tn__facts">
      <span>{{ facts.os_name || '未知系统' }}</span>
      <span class="lb-mono">内核 {{ facts.kernel || '—' }}</span>
      <span>虚拟化 {{ facts.virt || '未知' }}</span>
      <span class="lb-mono">内存 {{ formatBytes(facts.mem_total_kb * 1024) }}</span>
      <span class="lb-mono">{{ facts.cpu_count || '?' }} 核</span>
      <span class="lb-mono">根分区可用 {{ formatBytes(facts.disk_free_kb * 1024) }}</span>
    </div>

    <div class="tn__profile">
      <b>{{ report.profile }}</b>
      <span>{{ report.summary }}</span>
      <span v-if="running" class="tn__running">{{ running }}中…期间点击别处不会关掉本页</span>
    </div>

    <!-- 磁盘不参与算数值,但管理员会问"为什么采集磁盘"。写出来比让他猜好。 -->
    <div class="tn__basis">
      缓冲区、队列长度与 TIME_WAIT 上限全部由<strong>内存</strong>算出;CPU 与磁盘只作为
      写入前置条件与判断依据,不参与取值。
    </div>

    <div v-if="report.warnings.length" class="tn__warn">
      <div v-for="(w, i) in report.warnings" :key="i">· {{ w }}</div>
    </div>

    <div v-if="failedCount" class="tn__fail">
      有 {{ failedCount }} 项写进去了但读回来不是这个值。判定依据是<strong>读回的值</strong>,
      不是写入命令的退出码 —— 容器里写 /proc/sys 常常「成功」却不生效。
    </div>

    <div class="tn__list">
      <div v-for="section in grouped" :key="section.group" class="tn__section">
        <div class="tn__group">{{ section.group }}</div>
        <div v-for="item in section.items" :key="item.key" class="tn__row">
          <div class="tn__row-main">
            <span class="lb-mono tn__key">{{ item.key }}</span>
            <LbStatusTag :meta="stateMeta[item.state]" />
          </div>
          <div class="tn__row-values lb-mono">
            <span class="tn__cur">{{ item.current || '(空)' }}</span>
            <template v-if="item.state !== 'SAME'">
              <span class="tn__arrow">→</span>
              <span class="tn__want">{{ item.desired }}</span>
            </template>
          </div>
          <div class="tn__why">{{ item.reason }}</div>
          <div v-if="item.detail" class="tn__detail">{{ item.detail }}</div>
        </div>
      </div>
      <div v-if="!visibleItems.length" class="tn__none">
        这台机器上的内核参数已经全部是面板算出来的值,没有需要调整的项。
      </div>
    </div>

    <div class="tn__toggle">
      <a @click="showAll = !showAll">
        {{ showAll ? `只看需要留意的 ${notable.length} 项` : `显示全部 ${report.items.length} 项` }}
      </a>
      <span v-if="report.tuned_at" class="tn__stamp">
        节点上现有的配置生成于 {{ report.tuned_at }}
      </span>
    </div>

    <!-- 刻意没做的事。不写出来的话,下一个人会以为是漏了,
         然后把 tcp_mem 之类的东西补回去。 -->
    <details v-if="report.notes.length" class="tn__notes">
      <summary>面板刻意没做的几件事({{ report.notes.length }})</summary>
      <div v-for="(n, i) in report.notes" :key="i">· {{ n }}</div>
    </details>

    <div class="tn__acts">
      <a-button
        type="primary"
        size="small"
        :loading="running === '应用调优'"
        :disabled="!!running"
        @click="confirmApply"
      >
        应用调优
      </a-button>
      <a-button size="small" :loading="running === '重新检查'" :disabled="!!running" @click="refresh">
        重新检查
      </a-button>
      <!-- 没有快照就没有可靠的还原依据。按钮此时不出现,而不是点了再报错 ——
           这台机器上本来就没有可还原的东西。 -->
      <a-button
        v-if="report.baseline_present"
        size="small"
        danger
        :loading="running === '还原'"
        :disabled="!!running"
        @click="confirmRestore"
      >
        还原到调优前
      </a-button>
      <span class="tn__acts-note">应用不重启服务,也不断开任何在线连接</span>
    </div>
  </section>
</template>

<style scoped>
.tn {
  margin-top: 12px;
  padding: 12px 14px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.tn__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  font-size: 12.5px;
}

.tn__title {
  font-weight: 600;
}

.tn__facts {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  font-size: 11px;
  color: #6b7480;
}

.tn__profile {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 10px;
  font-size: 12px;
  color: #576070;
}

.tn__running {
  color: #2563b8;
}

.tn__basis {
  font-size: 11px;
  line-height: 1.7;
  color: #6b7480;
}

.tn__warn {
  padding: 9px 11px;
  background: #fcf3e3;
  border: 1px solid #efdcb4;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.75;
  color: #5c4405;
}

.tn__fail {
  padding: 9px 11px;
  background: #fdecea;
  border: 1px solid #f3cfc9;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.75;
  color: #8e2117;
}

.tn__list {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.tn__group {
  padding: 6px 11px;
  background: #f6f7f9;
  font-size: 11px;
  font-weight: 600;
  color: #576070;
}

.tn__row {
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 8px 11px;
  border-top: 1px solid #edeff2;
}

.tn__row-main {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.tn__key {
  font-size: 11.5px;
  color: #15181c;
}

.tn__row-values {
  display: flex;
  align-items: baseline;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 11px;
}

.tn__cur {
  color: #6b7480;
}

.tn__arrow {
  color: #a9b1bb;
}

.tn__want {
  color: #2563b8;
}

.tn__why {
  font-size: 10.5px;
  color: #6b7480;
}

.tn__detail {
  font-size: 10.5px;
  line-height: 1.65;
  color: #92610a;
}

.tn__none {
  padding: 12px;
  font-size: 12px;
  color: #6b7480;
}

.tn__toggle {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  font-size: 11.5px;
}

.tn__stamp {
  font-size: 10.5px;
  color: #6b7480;
}

.tn__notes {
  font-size: 11.5px;
  line-height: 1.75;
  color: #576070;
}

.tn__notes summary {
  cursor: pointer;
  color: #6b7480;
}

.tn__notes > div {
  margin-top: 5px;
}

.tn__acts {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.tn__acts-note {
  margin-left: auto;
  font-size: 11px;
  color: #6b7480;
}

@media (max-width: 767px) {
  .tn__acts-note {
    margin-left: 0;
  }
}
</style>
