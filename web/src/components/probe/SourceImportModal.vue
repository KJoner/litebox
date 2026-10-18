<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ProbeFamily,
  type ProbeOrigin,
  type ProbePreview,
  type ProbePreviewItem,
  type ProbeProvince,
  type ProbeSource,
  type ProbeTCPMode,
} from '@/api/client'
import { LbIcon, LbInfoTip } from '@/components/lb'
import { familyText, ispText } from '@/components/asset/probeView'

/**
 * 从第三方来源导入:先生成或拉取候选、和目标库比对,再由管理员勾选写入。
 *
 *   - Zstatic:省级节点按 {省份}-{运营商}-v{4|6}.ip.zstaticcdn.com 在本地生成(端口 80,不联网);
 *     市级节点只能从目录读,端口按目录记录;
 *   - TcpQuality:手动拉一次 getNodes,只收 CDN 类;上游的改动逐条确认,手工改过的字段不覆盖,
 *     消失的只标记不删除;接口不可用时目标库不动,只标记来源状态。
 *
 * 导入的目标都不对任何节点生效;对节点启用前,来源要先确认授权。
 */
const props = defineProps<{
  open: boolean
  source: ProbeOrigin
  info: ProbeSource | null
  provinces: ProbeProvince[]
}>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'imported'): void }>()

const isZstatic = computed(() => props.source === 'ZSTATIC')
const tab = ref<'preset' | 'directory'>('preset')
const tcpMode = ref<ProbeTCPMode>('CONNECT')
const hostMode = ref<'DOMAIN' | 'IP'>('DOMAIN')
const preset = reactive({ provinces: [] as string[], isps: ['CT', 'CU', 'CM'] as string[], families: ['V4'] as ProbeFamily[] })
const preview = ref<ProbePreview | null>(null)
const pickImport = ref<string[]>([])
const pickUpdate = ref<string[]>([])
const markMissing = ref(false)
const busy = ref(false)
const applying = ref(false)
const error = ref('')
const q = ref('')
const level = ref('')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    preview.value = null
    error.value = ''
    tab.value = 'preset'
    preset.provinces = props.provinces.filter((p) => p.common).map((p) => p.code)
  },
)
watch([tcpMode, hostMode, tab], () => {
  preview.value = null
})

const regions = computed(() => {
  const m = new Map<string, ProbeProvince[]>()
  for (const p of props.provinces) {
    if (!m.has(p.region)) m.set(p.region, [])
    m.get(p.region)!.push(p)
  }
  return [...m.entries()]
})

function setPreview(pv: ProbePreview) {
  preview.value = pv
  // 默认勾上「新的主节点」;备用节点、上游改动、消失标记一律不默认勾 —— 要管理员逐条看过。
  pickImport.value = pv.items.filter((i) => i.status === 'NEW' && i.role !== 'BACKUP').map((i) => i.key)
  pickUpdate.value = []
  markMissing.value = false
}

async function generate() {
  busy.value = true
  error.value = ''
  try {
    if (isZstatic.value && tab.value === 'preset') {
      if (!preset.provinces.length || !preset.isps.length || !preset.families.length) {
        error.value = '省份、运营商、地址族都至少选一个'
        return
      }
      setPreview(await api.zstaticPresets({ provinces: preset.provinces, isps: preset.isps, families: preset.families, tcp_mode: tcpMode.value }))
    } else {
      setPreview(
        await api.fetchProbeSource(isZstatic.value ? 'zstatic' : 'tcpquality', { tcp_mode: tcpMode.value, host_mode: hostMode.value }),
      )
    }
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '读取失败'
    if (!isZstatic.value || tab.value === 'directory') error.value += '(目标库没有任何改动;来源状态已标记)'
  } finally {
    busy.value = false
  }
}

const shown = computed<ProbePreviewItem[]>(() => {
  const list = preview.value?.items ?? []
  const s = q.value.trim().toLowerCase()
  return list.filter(
    (i) =>
      (!level.value || i.level === level.value) &&
      (!s || [i.name, i.host, i.province, i.city].join(' ').toLowerCase().includes(s)),
  )
})
const levels = computed(() => [...new Set((preview.value?.items ?? []).map((i) => i.level))])

function toggleAll(on: boolean) {
  const keys = shown.value.filter((i) => i.status === 'NEW').map((i) => i.key)
  pickImport.value = on ? [...new Set([...pickImport.value, ...keys])] : pickImport.value.filter((k) => !keys.includes(k))
}

const statusText: Record<string, string> = { NEW: '新', SAME: '已在库中', CHANGED: '上游有改动', EXISTS: '已存在', MISSING: '上游已消失' }

async function apply() {
  const pv = preview.value
  if (!pv) return
  if (!pickImport.value.length && !pickUpdate.value.length && !markMissing.value) {
    error.value = '没有勾选任何要导入或接受的项'
    return
  }
  applying.value = true
  error.value = ''
  try {
    const res = await api.applyProbeSource({
      source: props.source,
      snapshot_id: pv.snapshot_id || undefined,
      import: pickImport.value,
      update: pickUpdate.value,
      mark_missing: markMissing.value,
      tcp_mode: tcpMode.value,
      host_mode: hostMode.value,
    })
    const parts = [`导入 ${res.created} 个`]
    if (res.updated) parts.push(`接受改动 ${res.updated} 项`)
    if (res.missing) parts.push(`标记消失 ${res.missing} 项`)
    if (res.restored) parts.push(`恢复 ${res.restored} 项`)
    if (res.skipped.length) parts.push(`跳过 ${res.skipped.length} 项`)
    message.success(parts.join(',') + ';导入的目标还没有对任何节点启用')
    emit('imported')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '导入失败'
  } finally {
    applying.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="isZstatic ? 'Zstatic 节点 · 预置与目录' : 'TcpQuality · 同步节点'"
    :width="920"
    :mask-closable="false"
    @cancel="emit('close')"
  >
    <div class="sim" :data-testid="`source-import-${source}`">
      <div class="lb-notice sim__notice">
        <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
        <div class="lb-notice__body">
          <div class="lb-notice__title">
            第三方目标:导入只是登记,不会让任何节点开始探测
            <span v-if="info && !info.authorized" class="sim__bad">· 这个来源还没有确认授权</span>
          </div>
          <div class="lb-notice__text">
            <template v-if="isZstatic">
              Zstatic 页面注明节点目录仅供个人测试与研究参考,严禁扫描、压测等未经授权的用途;它不是无限制的监测服务。
            </template>
            <template v-else>
              getNodes 是 TcpQuality 作者运营的服务,README 没有声明允许第三方程序调用:这里只在你点击时拉取一次,不做定期同步;首版只收 CDN 类目标。
            </template>
            持续探测前请确认使用授权、频率与目标可用性;面板对第三方目标默认每 {{ info?.min_interval_sec ?? 300 }} 秒一次,全站另有预算与单目标限流。
          </div>
        </div>
      </div>

      <div v-if="isZstatic" class="lb-seg lb-seg--sm sim__tabs" role="tablist">
        <button type="button" role="tab" class="lb-seg__item" :class="{ 'lb-seg__item--on': tab === 'preset' }" @click="tab = 'preset'">
          省级预置(本地生成)
        </button>
        <button type="button" role="tab" class="lb-seg__item" :class="{ 'lb-seg__item--on': tab === 'directory' }" @click="tab = 'directory'">
          读取目录(含市级节点)
        </button>
      </div>

      <div class="sim__opts">
        <template v-if="isZstatic && tab === 'preset'">
          <div class="sim__opt">
            <span class="sim__label">
              省份
              <button type="button" class="lb-linkbtn" @click="preset.provinces = provinces.filter((p) => p.common).map((p) => p.code)">常用(北京 / 上海 / 广东)</button>
              <button type="button" class="lb-linkbtn" @click="preset.provinces = provinces.map((p) => p.code)">全国</button>
              <button type="button" class="lb-linkbtn" @click="preset.provinces = []">清空</button>
            </span>
            <a-checkbox-group v-model:value="preset.provinces" class="sim__provs">
              <div v-for="[region, list] in regions" :key="region" class="sim__region">
                <span class="sim__region-name">{{ region }}</span>
                <a-checkbox v-for="p in list" :key="p.code" :value="p.code">{{ p.name }}</a-checkbox>
              </div>
            </a-checkbox-group>
          </div>
          <div class="sim__opt sim__opt--row">
            <span class="sim__label">运营商</span>
            <a-checkbox-group v-model:value="preset.isps" :options="[{ label: '电信', value: 'CT' }, { label: '联通', value: 'CU' }, { label: '移动', value: 'CM' }]" />
            <span class="sim__label">地址族</span>
            <a-checkbox-group v-model:value="preset.families" :options="[{ label: 'IPv4', value: 'V4' }, { label: 'IPv6', value: 'V6' }]" />
          </div>
        </template>
        <div class="sim__opt sim__opt--row">
          <span class="sim__label">
            TCP 方式
            <LbInfoTip
              :width="320"
              text="建连:一次完整建连,看连接失败率(与 NetQuality 的 TCP 测试同一类测量)。握手近似:每轮几次短超时握手,看握手超时无应答率 —— 基于系统的 TCP 建连,不是原始套接字的 SYN 探测,也不是真实丢包率,每次握手都计入预算。第三方节点的 ICMP 常被限速或不回,所以这里只导入 TCP。"
            />
          </span>
          <a-radio-group v-model:value="tcpMode" button-style="solid" size="small">
            <a-radio-button value="CONNECT">建连</a-radio-button>
            <a-radio-button value="SYN">握手近似</a-radio-button>
          </a-radio-group>
          <template v-if="!isZstatic">
            <span class="sim__label">
              探测地址
              <LbInfoTip
                :width="300"
                text="按域名(默认):每台机器自己解析 Zstatic 的域名,上游换 IP 不用重新同步。按上游 IP:用 getNodes 给的固定 IP,上游 IP 变了要在这里确认改动。"
              />
            </span>
            <a-radio-group v-model:value="hostMode" button-style="solid" size="small">
              <a-radio-button value="DOMAIN">按域名</a-radio-button>
              <a-radio-button value="IP">按上游 IP</a-radio-button>
            </a-radio-group>
          </template>
          <a-button type="primary" size="small" :loading="busy" class="sim__go" @click="generate">
            {{ isZstatic && tab === 'preset' ? '生成预览' : '拉取并预览' }}
          </a-button>
        </div>
      </div>

      <div v-if="error" class="lb-error-strip">{{ error }}</div>

      <template v-if="preview">
        <div v-if="preview.bulk_warning" class="lb-error-strip">{{ preview.bulk_warning }}</div>
        <div class="sim__sum lb-tabular">
          <span>新 {{ preview.counts.NEW ?? 0 }}</span>
          <span>已在库中 {{ preview.counts.SAME ?? 0 }}</span>
          <span :class="{ sim__warn: preview.counts.CHANGED }">上游有改动 {{ preview.counts.CHANGED ?? 0 }}</span>
          <span>已存在(别的来源 / 手工){{ preview.counts.EXISTS ?? 0 }}</span>
          <span v-if="preview.counts.MISSING">上游已消失 {{ preview.counts.MISSING }}</span>
          <span v-if="preview.rejected.length" class="sim__bad">拒绝 {{ preview.rejected.length }}</span>
        </div>
        <div v-if="Object.keys(preview.unsupported).length" class="sim__muted">
          首版不提供批量导入:{{ Object.entries(preview.unsupported).map(([k, v]) => `${k} ${v} 条`).join('、') }}。
          这些是第三方业务站点,不是专门的测速节点;确有授权时请用「新增目标」单独登记。
        </div>
        <div class="sim__filter">
          <a-input v-model:value="q" allow-clear size="small" placeholder="搜索名称 / 主机" class="sim__q" />
          <a-select v-if="levels.length > 1" v-model:value="level" size="small" class="sim__level">
            <a-select-option value="">全部级别</a-select-option>
            <a-select-option v-for="l in levels" :key="l" :value="l">{{ l === 'CITY' ? '市级' : l === 'PROVINCE' ? '省级' : l }}</a-select-option>
          </a-select>
          <button type="button" class="lb-linkbtn" @click="toggleAll(true)">勾选显示的新项</button>
          <button type="button" class="lb-linkbtn" @click="toggleAll(false)">取消</button>
          <span class="sim__muted lb-tabular">已选导入 {{ pickImport.length }} · 接受改动 {{ pickUpdate.length }}</span>
        </div>
        <div class="sim__table">
          <table>
            <thead>
              <tr><th></th><th>名称</th><th>地址</th><th v-if="!isZstatic">上游 IP</th><th>地区</th><th>状态</th><th>说明</th></tr>
            </thead>
            <tbody>
              <tr v-for="i in shown" :key="i.key" :data-key="i.key">
                <td>
                  <a-checkbox
                    v-if="i.status === 'NEW'"
                    :checked="pickImport.includes(i.key)"
                    @update:checked="(v: boolean) => (pickImport = v ? [...pickImport, i.key] : pickImport.filter((k) => k !== i.key))"
                  />
                  <a-checkbox
                    v-else-if="i.status === 'CHANGED'"
                    :checked="pickUpdate.includes(i.key)"
                    @update:checked="(v: boolean) => (pickUpdate = v ? [...pickUpdate, i.key] : pickUpdate.filter((k) => k !== i.key))"
                  />
                </td>
                <td>
                  {{ i.name }}
                  <span v-if="i.role === 'BACKUP'" class="lb-chip">备用</span>
                  <span v-if="i.level === 'CITY'" class="lb-chip">市级</span>
                </td>
                <td class="lb-mono">{{ i.host }}:{{ i.port }} <span class="sim__muted">{{ familyText(i.family) }}</span></td>
                <td v-if="!isZstatic" class="lb-mono">{{ i.ip || '—' }}</td>
                <td>{{ [i.province, i.city, ispText(i.isp)].filter(Boolean).join(' ') }}</td>
                <td :class="{ sim__warn: i.status === 'CHANGED', sim__muted: i.status === 'SAME' || i.status === 'EXISTS' }">
                  {{ statusText[i.status] }}
                </td>
                <td class="sim__note">
                  <template v-if="i.status === 'CHANGED'">
                    {{ i.changes.join(';') }}
                    <span v-if="i.suspicious.length" class="sim__bad">· 可疑:{{ i.suspicious.join('、') }}</span>
                    <span v-if="i.manual_kept.length" class="sim__muted">· 手工改过的 {{ i.manual_kept.join('、') }} 不会覆盖</span>
                  </template>
                  <template v-else-if="i.status === 'EXISTS'">{{ i.exists_as }}</template>
                  <template v-else-if="i.missing_since">之前被标记为上游已消失,接受后恢复</template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="preview.missing.length" class="sim__missing">
          <a-checkbox v-model:checked="markMissing">
            把上游列表里不见了的 {{ preview.missing.length }} 项标记为「上游已消失」(只标记,不删除、不停测)
          </a-checkbox>
          <div class="sim__muted">{{ preview.missing.map((m) => m.name).join('、') }}</div>
        </div>
        <details v-if="preview.rejected.length" class="sim__rejected">
          <summary>被拒绝的 {{ preview.rejected.length }} 条(地址、端口或域名形状不合规)</summary>
          <div v-for="(r, k) in preview.rejected.slice(0, 50)" :key="k" class="sim__muted">
            <template v-if="r.line">第 {{ r.line }} 行 · </template>{{ r.reason }} · <span class="lb-mono">{{ r.text }}</span>
          </div>
        </details>
      </template>
    </div>
    <template #footer>
      <a-button @click="emit('close')">关闭</a-button>
      <a-button type="primary" :disabled="!preview" :loading="applying" @click="apply">写入目标库</a-button>
    </template>
  </a-modal>
</template>

<style scoped>
.sim {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.sim__notice {
  margin: 0;
}
.sim__tabs {
  align-self: flex-start;
}
.sim__opts {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.sim__opt {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.sim__opt--row {
  flex-direction: row;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2) var(--sp-3);
}
.sim__label {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text2);
}
.sim__go {
  margin-left: auto;
}
.sim__provs {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.sim__region {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 2px var(--sp-2);
}
.sim__region-name {
  width: 40px;
  font-size: var(--fs-xs);
  color: var(--text3);
}
.sim__sum {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
  font-size: var(--fs-sm);
}
.sim__filter {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.sim__q {
  width: 220px;
}
.sim__level {
  width: 110px;
}
.sim__table {
  max-height: 340px;
  overflow: auto;
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.sim__table table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-xs);
}
.sim__table th,
.sim__table td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--sep2);
  text-align: left;
  vertical-align: top;
}
.sim__table th {
  position: sticky;
  top: 0;
  z-index: 1;
  background: var(--surface2);
  font-weight: 600;
  color: var(--text2);
}
.sim__note {
  min-width: 180px;
}
.sim__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.sim__warn {
  color: var(--warn);
}
.sim__bad {
  color: var(--bad);
}
.sim__missing {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.sim__rejected summary {
  cursor: pointer;
  font-size: var(--fs-xs);
  color: var(--text2);
}
</style>
