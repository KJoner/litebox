<script setup lang="ts">
import { computed, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type HostInterface, type NodeCycleUsage } from '@/api/client'
import { formatBytes, formatUTCTime } from '@/utils/format'
import { lbDangerConfirm } from '@/components/lb'

/**
 * 节点额度的数据来源与计量网卡(VNext TRAFFIC01 / 03 / 07)。
 *
 * 两种来源都如实标明:代理折算是 sing-box 计数 × 口径倍数(估算,不含 SSH、更新与不走代理的流量);
 * 网卡实测是 vnStat 的网卡字节。切到网卡实测时之前那一段仍按代理折算算并标出分界,不把旧估算改写成实测。
 * 只影响节点额度的预警,不碰用户额度,不进节点配置、不部署。网卡列表只在点开时读一次(一次 SSH)。
 */
const props = defineProps<{ nodeId: number; cycle: NodeCycleUsage }>()
const emit = defineEmits<{ changed: [] }>()

const ifaces = ref<HostInterface[] | null>(null)
const pinned = ref('')
const current = ref('')
const loadingIf = ref(false)
const ifError = ref('')

const isNIC = computed(() => props.cycle.quota_source === 'HOST_NIC')
const qualityText = computed(() => {
  switch (props.cycle.quality) {
    case 'STALE':
      return '数据过期'
    case 'NO_DATA':
      return '从未拉到过'
    case 'NOT_INSTALLED':
      return '没有 vnStat'
    default:
      return ''
  }
})

function switchSource(to: 'PROXY_ESTIMATE' | 'HOST_NIC') {
  lbDangerConfirm({
    title: to === 'HOST_NIC' ? '节点额度改用网卡实测?' : '节点额度改回代理折算?',
    okText: '改',
    okType: 'primary',
    impacts:
      to === 'HOST_NIC'
        ? [
            '从此刻起按 vnStat 的网卡字节计(出站计费只算 TX,进出合计算 RX + TX)',
            '本周期在此之前的那一段仍按代理折算算,两段分开显示 —— 不把旧估算改写成实测',
            '分界所在的那一小时不计(面板拿到的最细粒度是小时桶)',
            '只影响节点额度预警;用户额度与用户历史账本一个字节都不变',
          ]
        : ['整个周期回到代理折算(sing-box 计数 × 口径倍数),账本一直在,改回去就能还原', '只影响节点额度预警'],
    onOk: async () => {
      try {
        await api.setNodeQuotaSource(props.nodeId, to)
        message.success('已改')
        emit('changed')
      } catch (e) {
        message.error(e instanceof ApiError ? e.message : '修改失败')
      }
    },
  })
}

async function loadIfaces() {
  loadingIf.value = true
  ifError.value = ''
  try {
    const r = await api.nodeHostInterfaces(props.nodeId)
    ifaces.value = r.interfaces
    pinned.value = r.state.iface_pinned
    current.value = r.state.iface
  } catch (e) {
    ifError.value = e instanceof ApiError ? e.message : '读不到网卡列表'
  } finally {
    loadingIf.value = false
  }
}

async function pin(iface: string) {
  try {
    await api.setNodeHostInterface(props.nodeId, iface)
    pinned.value = iface
    message.success(iface ? `计量网卡改为 ${iface},下一次同步起生效` : '改回跟随默认路由')
    emit('changed')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '修改失败')
  }
}
</script>

<template>
  <div class="qs">
    <div class="qs__row">
      <span class="qs__label">额度数据来源</span>
      <b>{{ isNIC ? '网卡实测(vnStat)' : '代理折算(sing-box 计数 × 口径倍数,估算)' }}</b>
      <span v-if="isNIC && qualityText" class="qs__bad" :title="cycle.quality_note">{{ qualityText }}</span>
      <a-button size="small" class="qs__op" @click="switchSource(isNIC ? 'PROXY_ESTIMATE' : 'HOST_NIC')">
        {{ isNIC ? '改回代理折算' : '改用网卡实测' }}
      </a-button>
    </div>
    <div v-if="isNIC" class="qs__segments">
      <div>
        <span>代理折算段</span><b class="lb-mono">{{ formatBytes(cycle.estimate_bytes) }}</b>
        <em v-if="cycle.quota_source_since">(到 {{ formatUTCTime(cycle.quota_source_since) }} 为止)</em>
      </div>
      <div>
        <span>网卡实测段</span><b class="lb-mono">{{ formatBytes(cycle.host_bytes) }}</b>
        <em>RX {{ formatBytes(cycle.host_rx_bytes) }} · TX {{ formatBytes(cycle.host_tx_bytes) }}{{ cycle.host_iface ? ` · ${cycle.host_iface}` : '' }}</em>
      </div>
      <div v-if="cycle.quality_note" class="qs__bad">{{ cycle.quality_note }}</div>
      <div v-else-if="cycle.host_synced_at" class="qs__muted">网卡数据更新于 {{ formatUTCTime(cycle.host_synced_at) }}</div>
    </div>
    <div class="qs__row">
      <span class="qs__label">计量网卡</span>
      <a-button size="small" :loading="loadingIf" @click="loadIfaces">{{ ifaces ? '重新读取' : '查看 / 指定' }}</a-button>
      <span v-if="ifError" class="qs__bad">{{ ifError }}</span>
    </div>
    <div v-if="ifaces" class="qs__ifaces">
      <label class="qs__if">
        <input type="radio" name="qs-iface" :checked="pinned === ''" @change="pin('')" />
        <span>跟随默认路由(现在是 {{ current || '—' }})</span>
      </label>
      <label v-for="it in ifaces" :key="it.name" class="qs__if" :title="it.excluded">
        <input type="radio" name="qs-iface" :checked="pinned === it.name" @change="pin(it.name)" />
        <span class="lb-mono">{{ it.name }}</span>
        <span class="qs__muted">RX {{ formatBytes(it.rx_bytes) }} · TX {{ formatBytes(it.tx_bytes) }}</span>
        <span v-if="it.default_v4" class="qs__chip">IPv4 默认路由</span>
        <span v-if="it.default_v6" class="qs__chip">IPv6 默认路由</span>
        <span v-if="it.excluded" class="qs__muted">· 不建议:{{ it.excluded }}</span>
      </label>
      <div class="qs__muted">
        默认路由只是候选:IPv4 / IPv6 可以从两块网卡出去,LXC 里看到的名字也未必是宿主上那一块。换网卡之后,
        之前的柱子来自另一块网卡;没有常驻采集器,机器离线或重启的那段会是缺口。
      </div>
    </div>
  </div>
</template>

<style scoped>
.qs {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  margin-top: var(--sp-3);
  padding-top: var(--sp-3);
  border-top: 1px solid var(--sep2);
  font-size: var(--fs-xs);
}
.qs__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}
.qs__label {
  color: var(--text2);
}
.qs__op {
  margin-left: auto;
}
.qs__segments {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.qs__segments span {
  color: var(--text2);
  margin-right: var(--sp-2);
}
.qs__segments em,
.qs__muted {
  color: var(--text3);
  font-style: normal;
}
.qs__bad {
  color: var(--warn);
}
.qs__ifaces {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.qs__if {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
  cursor: pointer;
}
.qs__chip {
  padding: 0 6px;
  border-radius: var(--r-xs);
  background: var(--surface2);
  color: var(--text2);
}
</style>
