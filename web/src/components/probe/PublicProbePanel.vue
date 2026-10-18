<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ProbeOverview,
  type ProbeProtocol,
  type ProbeFamily,
  type PublicProbeInput,
  type PublicProbeNode,
  type PublicProbeScope,
  type PublicProbeTarget,
} from '@/api/client'
import { LbIcon, LbInfoTip, LbTimeText } from '@/components/lb'
import { familyText } from '@/components/asset/probeView'

/**
 * 设置页里的「公共探测目标」区块:出站网络质量的目标库。
 *
 * 一个公共目标 = 一个 IP / 域名 + 协议(ICMP / TCP)+ 端口 + 间隔。登记与启用是两步 ——
 * 登记之后不对任何机器生效,要另外选「全部节点」或具体节点(这里批量选,节点详情里逐台开关)。
 * 私网、回环、云元数据这类地址与邮件、SMB 端口默认拒绝,确实要测时显式勾选。
 * 协议与地址族创建后不能改:换了就是另一条测量,旧的历史与它不可比。
 */
const items = ref<PublicProbeTarget[]>([])
const overview = ref<ProbeOverview | null>(null)
const loading = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const r = await api.publicProbeTargets()
    items.value = r.targets
    overview.value = r.probe
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '读取公共探测目标失败'
  } finally {
    loading.value = false
  }
}
onMounted(load)

const missingHelpers = computed(() => {
  const h = overview.value?.helpers ?? {}
  return ['amd64', 'arm64'].filter((a) => !h[a])
})

function scopeText(p: PublicProbeTarget): string {
  if (!p.enabled) return `已暂停(${p.node_count} 台不再测)`
  if (p.apply_all) return p.off_count ? `全部节点,${p.off_count} 台单独停用` : `全部节点(${p.node_count} 台)`
  if (p.node_count) return `部分节点:${p.node_count} 台`
  return '还没有对任何节点启用'
}

function paramText(p: PublicProbeTarget): string {
  const parts = [`每 ${p.interval_sec} 秒`]
  if (p.protocol === 'ICMP') parts.push(`每轮 ${p.count} 个包`)
  parts.push(`超时 ${p.timeout_ms} ms`)
  return parts.join(' · ')
}

function toInput(p: PublicProbeTarget): PublicProbeInput {
  return {
    name: p.name, host: p.host, protocol: p.protocol, port: p.port, family: p.family,
    interval_sec: p.interval_sec, count: p.count, timeout_ms: p.timeout_ms, enabled: p.enabled,
    allow_restricted: p.allow_restricted, sort_order: p.sort_order, note: p.note,
  }
}

// ---------- 新建 / 编辑 ----------

const open = ref(false)
const editing = ref<PublicProbeTarget | null>(null)
const saving = ref(false)
const formError = ref('')
const form = reactive({
  name: '',
  host: '',
  protocol: 'ICMP' as ProbeProtocol,
  port: null as number | null,
  family: 'V4' as ProbeFamily,
  interval_sec: 60 as number | null,
  count: 4 as number | null,
  timeout_ms: 1000 as number | null,
  sort_order: 0 as number | null,
  note: '',
  allow_restricted: false,
  enabled: true,
})

function openCreate() {
  editing.value = null
  Object.assign(form, {
    name: '', host: '', protocol: 'ICMP', port: null, family: 'V4', interval_sec: 60, count: 4,
    timeout_ms: 1000, sort_order: items.value.length, note: '', allow_restricted: false, enabled: true,
  })
  formError.value = ''
  open.value = true
}

function openEdit(p: PublicProbeTarget) {
  editing.value = p
  Object.assign(form, { ...toInput(p), port: p.protocol === 'TCP' ? p.port : null, enabled: p.enabled })
  formError.value = ''
  open.value = true
}

async function submit() {
  formError.value = ''
  const body: PublicProbeInput = {
    name: form.name.trim(),
    host: form.host.trim(),
    protocol: form.protocol,
    port: form.protocol === 'TCP' ? form.port ?? 0 : 0,
    family: form.family,
    interval_sec: form.interval_sec ?? 0,
    count: form.protocol === 'ICMP' ? form.count ?? 0 : 0,
    timeout_ms: form.timeout_ms ?? 0,
    allow_restricted: form.allow_restricted,
    sort_order: form.sort_order ?? 0,
    note: form.note.trim(),
  }
  if (editing.value) body.enabled = form.enabled
  saving.value = true
  try {
    if (editing.value) {
      await api.updatePublicProbeTarget(editing.value.id, body)
      message.success('已保存')
      open.value = false
      await load()
    } else {
      const p = await api.createPublicProbeTarget(body)
      open.value = false
      message.success('已登记,还没有对任何节点启用 —— 接着选要测它的节点')
      await load()
      openScope(items.value.find((x) => x.id === p.id) ?? p)
    }
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

// ---------- 启用范围 ----------

const scopeOpen = ref(false)
const scopeTarget = ref<PublicProbeTarget | null>(null)
const scope = ref<PublicProbeScope>('NONE')
const scopeNodes = ref<PublicProbeNode[]>([])
const scopePicked = ref<number[]>([])
const scopeLoading = ref(false)
const scopeSaving = ref(false)
const scopeError = ref('')

async function openScope(p: PublicProbeTarget) {
  scopeTarget.value = p
  scopeError.value = ''
  scopeOpen.value = true
  scopeLoading.value = true
  try {
    const r = await api.publicProbeTargetNodes(p.id)
    scopeNodes.value = r.nodes
    scopePicked.value = r.nodes.filter((n) => n.effective).map((n) => n.node_id)
    scope.value = p.apply_all && !p.off_count ? 'ALL' : scopePicked.value.length ? 'SELECTED' : 'NONE'
  } catch (err) {
    scopeError.value = err instanceof ApiError ? err.message : '读取节点失败'
  } finally {
    scopeLoading.value = false
  }
}

async function saveScope() {
  const p = scopeTarget.value
  if (!p) return
  if (scope.value === 'SELECTED' && !scopePicked.value.length) {
    scopeError.value = '至少选一台节点,或者改选「都不启用」'
    return
  }
  scopeSaving.value = true
  scopeError.value = ''
  try {
    await api.setPublicProbeScope(p.id, {
      scope: scope.value,
      node_ids: scope.value === 'SELECTED' ? scopePicked.value : [],
    })
    message.success(
      scope.value === 'ALL'
        ? `「${p.name}」已对全部自建节点启用`
        : scope.value === 'SELECTED'
          ? `「${p.name}」已对 ${scopePicked.value.length} 台节点启用`
          : `「${p.name}」已对所有节点停用(历史保留)`,
    )
    scopeOpen.value = false
    await load()
  } catch (err) {
    scopeError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    scopeSaving.value = false
  }
}

// ---------- 暂停 / 删除 ----------

const busy = ref<number | null>(null)
async function togglePause(p: PublicProbeTarget) {
  busy.value = p.id
  try {
    await api.updatePublicProbeTarget(p.id, { ...toInput(p), enabled: !p.enabled })
    message.success(p.enabled ? `已暂停「${p.name}」:哪台都不再测,历史保留` : `已恢复「${p.name}」`)
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    busy.value = null
  }
}

function remove(p: PublicProbeTarget) {
  Modal.confirm({
    title: `删除公共探测目标「${p.name}」?`,
    content:
      '它在各台机器上的出站探测历史(明细与小时汇总)会一起删除,节点上单独设过的开关也会清掉。' +
      '只想暂时停下用「暂停」,历史会保留。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => {
      void (async () => {
        try {
          await api.deletePublicProbeTarget(p.id)
          message.success('已删除')
          await load()
        } catch (err) {
          message.error(err instanceof ApiError ? err.message : '删除失败')
        }
      })()
    },
  })
}
</script>

<template>
  <div class="pp" data-testid="public-probe-panel">
    <div v-if="loadError" class="lb-card"><div class="lb-error-strip">{{ loadError }}</div></div>
    <div v-else class="lb-card lb-card--flush pp__card">
      <div class="pp__status">
        <template v-if="overview">
          <span v-if="!overview.outbound_available" class="pp__muted">这个面板没有接入出站探测引擎</span>
          <template v-else>
            <span class="pp__muted">
              出站探测
              <template v-if="overview.outbound_last_run.last_run_at">
                上一拍 <LbTimeText :value="overview.outbound_last_run.last_run_at" /> · 执行
                {{ overview.outbound_last_run.nodes }} 台
                <template v-if="overview.outbound_last_run.busy">· 正忙让路 {{ overview.outbound_last_run.busy }} 台</template>
                <template v-if="overview.outbound_last_run.ssh_errors">· 采集失败 {{ overview.outbound_last_run.ssh_errors }}</template>
                <template v-if="overview.outbound_last_run.tool_errors">· 工具不可用 {{ overview.outbound_last_run.tool_errors }}</template>
              </template>
              <template v-else>还没有跑过</template>
            </span>
            <span v-if="missingHelpers.length" class="pp__warn">
              <LbIcon name="circle-alert" :size="13" />主控缺少 {{ missingHelpers.join('、') }} 的辅助程序(执行 scripts/build-probe.sh)
            </span>
            <span v-else class="pp__muted">辅助程序:amd64 · arm64 已就绪</span>
          </template>
          <span v-if="!overview.enabled" class="pp__warn">网络探测已在下面的「网络探测」里全局关闭</span>
        </template>
      </div>

      <div v-if="!loading && !items.length" class="pp__empty">
        <div class="pp__empty-title">还没有公共探测目标</div>
        <div class="pp__muted">
          登记几个公共地址(比如公共 DNS 的 ICMP、某个网站的 TCP 443),再选择对哪些节点启用。
          每台机器在本机测它们,卡片上一个目标一行。升级不会替任何机器自动开启探测。
        </div>
      </div>
      <ul v-else class="pp__list">
        <li v-for="p in items" :key="p.id" class="pp__item" :data-testid="`public-target-${p.id}`">
          <div class="pp__main">
            <div class="pp__line">
              <span class="pp__name lb-ellipsis">{{ p.name }}</span>
              <span class="lb-chip">{{ p.protocol === 'ICMP' ? 'ICMP' : `TCP ${p.port}` }}</span>
              <span class="lb-chip">{{ familyText(p.family) }}</span>
              <span class="lb-mono pp__host">{{ p.host }}</span>
              <span v-if="!p.enabled" class="lb-chip">已暂停</span>
              <span v-if="p.allow_restricted" class="lb-chip lb-chip--warn" title="这个目标允许私网 / 保留地址或受限端口">允许受限地址</span>
            </div>
            <div class="pp__meta lb-tabular">
              <span>{{ paramText(p) }}</span>
              <span :class="{ 'pp__warn-text': p.enabled && !p.node_count }">{{ scopeText(p) }}</span>
              <span v-if="p.note" class="lb-ellipsis pp__note" :title="p.note">{{ p.note }}</span>
            </div>
          </div>
          <div class="pp__ops">
            <a-button size="small" class="lb-btn-ghost" @click="openScope(p)">启用节点</a-button>
            <a-button size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="openEdit(p)">编辑</a-button>
            <a-button size="small" class="lb-btn-ghost lb-btn-ghost--text" :loading="busy === p.id" @click="togglePause(p)">
              {{ p.enabled ? '暂停' : '恢复' }}
            </a-button>
            <a-button size="small" class="lb-btn-ghost lb-btn-ghost--bad" @click="remove(p)">删除</a-button>
          </div>
        </li>
      </ul>
      <div class="pp__foot">
        <a-button size="small" class="lb-btn-ghost" data-testid="public-target-add" @click="openCreate">+ 新增公共目标</a-button>
      </div>
    </div>

    <!-- 新建 / 编辑 -->
    <a-modal
      :open="open"
      :title="editing ? `编辑公共目标 · ${editing.name}` : '新增公共探测目标'"
      :width="540"
      :confirm-loading="saving"
      :ok-text="editing ? '保存' : '登记'"
      cancel-text="取消"
      :mask-closable="false"
      @cancel="open = false"
      @ok="submit"
    >
      <div class="pp__sheet">
        <div v-if="formError" class="lb-error-strip">{{ formError }}</div>
        <div class="lb-group">
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label"><span class="pp__req">*</span> 名称</span>
            <a-input v-model:value="form.name" placeholder="例如 Cloudflare DNS" :maxlength="64" />
          </div>
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label">
              <span class="pp__req">*</span> 协议
              <LbInfoTip
                :width="300"
                text="ICMP 测往返时间与丢包率(每轮连发几个包);TCP 测建连时间与失败率(每轮建连一次,只握手、不发数据)。创建后不能改。"
              />
            </span>
            <a-radio-group v-model:value="form.protocol" :disabled="!!editing" button-style="solid" size="small">
              <a-radio-button value="ICMP">ICMP</a-radio-button>
              <a-radio-button value="TCP">TCP</a-radio-button>
            </a-radio-group>
          </div>
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label"><span class="pp__req">*</span> 地址族</span>
            <a-radio-group v-model:value="form.family" :disabled="!!editing" button-style="solid" size="small">
              <a-radio-button value="V4">IPv4</a-radio-button>
              <a-radio-button value="V6">IPv6</a-radio-button>
            </a-radio-group>
          </div>
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label">
              <span class="pp__req">*</span> 主机
              <LbInfoTip
                :width="320"
                text="IP 或域名。域名由每台机器在本机解析(只查 A 或 AAAA),换了指向就跟着测新地址,最近一次实际连的 IP 会记下来。没有 IPv6 出口的机器测 IPv6 目标会是「网络不可达」,可以在那台机器上单独停用。"
              />
            </span>
            <a-input v-model:value="form.host" class="lb-mono" placeholder="1.1.1.1 或 one.one.one.one" />
          </div>
          <div v-if="form.protocol === 'TCP'" class="lb-group__row pp__frow">
            <span class="lb-group__label"><span class="pp__req">*</span> 端口</span>
            <a-input-number v-model:value="form.port" :min="1" :max="65535" :precision="0" class="pp__num" placeholder="443" />
          </div>
        </div>
        <div class="lb-group">
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label">探测间隔</span>
            <a-input-number v-model:value="form.interval_sec" :min="30" :max="3600" :precision="0" addon-after="秒" class="pp__num" />
          </div>
          <div v-if="form.protocol === 'ICMP'" class="lb-group__row pp__frow">
            <span class="lb-group__label">每轮包数</span>
            <a-input-number v-model:value="form.count" :min="1" :max="10" :precision="0" addon-after="个" class="pp__num" />
          </div>
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label">
              超时
              <LbInfoTip :width="260" text="单个包 / 单次建连的超时,200~3000 毫秒。" />
            </span>
            <a-input-number v-model:value="form.timeout_ms" :min="200" :max="3000" :precision="0" addon-after="毫秒" class="pp__num" />
          </div>
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label">排序</span>
            <a-input-number v-model:value="form.sort_order" :precision="0" class="pp__num" />
          </div>
          <div class="lb-group__row pp__frow">
            <span class="lb-group__label">备注</span>
            <a-input v-model:value="form.note" :maxlength="200" placeholder="选填,只给管理员看" />
          </div>
          <div class="lb-group__row pp__frow pp__frow--switch">
            <span class="lb-group__label">
              允许受限地址与端口
              <LbInfoTip
                warn
                :width="320"
                text="默认拒绝:私网、回环、链路本地(含云元数据 169.254.169.254)、CGNAT、保留与文档地址段,以及邮件(25 / 465 / 587)与 SMB / NetBIOS(135~139 / 445)端口 —— 也包括域名解析到这些地址的情况。勾选之后各台机器会去测它,确认这是你想要的。"
              />
            </span>
            <a-switch v-model:checked="form.allow_restricted" />
          </div>
          <div v-if="editing" class="lb-group__row pp__frow pp__frow--switch">
            <span class="lb-group__label">
              启用
              <LbInfoTip :width="260" text="关掉 = 全局暂停:哪台机器都不再测,历史与节点开关都保留。" />
            </span>
            <a-switch v-model:checked="form.enabled" />
          </div>
        </div>
        <div v-if="editing" class="pp__muted">改主机或端口之后,各台机器上的统计从改动那一刻重新起算(旧地址上的丢包与新地址无关)。</div>
      </div>
    </a-modal>

    <!-- 启用范围 -->
    <a-modal
      :open="scopeOpen"
      :title="scopeTarget ? `启用节点 · ${scopeTarget.name}` : '启用节点'"
      :width="520"
      :confirm-loading="scopeSaving"
      ok-text="保存"
      cancel-text="取消"
      :mask-closable="false"
      @cancel="scopeOpen = false"
      @ok="saveScope"
    >
      <div class="pp__sheet">
        <div v-if="scopeError" class="lb-error-strip">{{ scopeError }}</div>
        <a-skeleton v-if="scopeLoading" active :paragraph="{ rows: 3 }" />
        <template v-else>
          <a-radio-group v-model:value="scope" class="pp__scope">
            <a-radio value="ALL">全部自建节点(含以后新建的)</a-radio>
            <a-radio value="SELECTED">只对选中的节点</a-radio>
            <a-radio value="NONE">都不启用</a-radio>
          </a-radio-group>
          <div v-if="scope === 'SELECTED'" class="pp__nodes">
            <div class="pp__nodes-head">
              <span class="pp__muted">已选 {{ scopePicked.length }} / {{ scopeNodes.length }}</span>
              <button type="button" class="lb-linkbtn" @click="scopePicked = scopeNodes.map((n) => n.node_id)">全选</button>
              <button type="button" class="lb-linkbtn" @click="scopePicked = []">全不选</button>
            </div>
            <a-checkbox-group v-model:value="scopePicked" class="pp__node-list">
              <a-checkbox v-for="n in scopeNodes" :key="n.node_id" :value="n.node_id">
                {{ n.display_name || n.name }} <span class="pp__muted">{{ n.name }}</span>
              </a-checkbox>
            </a-checkbox-group>
          </div>
          <div class="pp__muted">
            这是「换成这个集合」:节点详情里单独设过的启用 / 停用会一并清掉。启用之后,面板会在这些机器上放一个约 3 MB 的辅助程序
            (/opt/litebox/litebox-probe),每轮经已有的 SSH 连接启动一次、跑完即退出,不常驻、不监听端口;节点正在部署或同步时那一轮让路。
          </div>
        </template>
      </div>
    </a-modal>
  </div>
</template>

<style scoped>
.pp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.pp__status {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1) var(--sp-3);
  padding: var(--sp-3) var(--sp-5);
  border-bottom: 1px solid var(--sep2);
}
.pp__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
  line-height: 1.6;
}
.pp__warn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.pp__warn-text {
  color: var(--warn);
}
.pp__empty {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: var(--sp-4) var(--sp-5);
}
.pp__empty-title {
  font-weight: 600;
}
.pp__list {
  list-style: none;
  margin: 0;
  padding: 0;
}
.pp__item {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2) var(--sp-3);
  padding: var(--sp-3) var(--sp-5);
  border-bottom: 1px solid var(--sep2);
}
.pp__main {
  flex: 1 1 320px;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.pp__line {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-1) var(--sp-2);
  min-width: 0;
}
.pp__name {
  font-weight: 600;
  max-width: 100%;
}
.pp__host {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.pp__meta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1) var(--sp-3);
  font-size: var(--fs-xs);
  color: var(--text3);
  min-width: 0;
}
.pp__note {
  max-width: 280px;
}
.pp__ops {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
}
.pp__foot {
  padding: 10px var(--sp-5);
  background: var(--surface2);
  border-radius: 0 0 var(--r-card) var(--r-card);
}
.pp__sheet {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.pp__frow {
  grid-template-columns: 130px minmax(0, 1fr);
}
.pp__frow--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.pp__num {
  width: 180px;
}
.pp__req {
  color: var(--bad);
}
.pp__scope {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.pp__nodes {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  max-height: 280px;
  overflow: auto;
}
.pp__nodes-head {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}
.pp__node-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
@media (max-width: 767px) {
  .pp__frow {
    grid-template-columns: 1fr;
  }
}
</style>
