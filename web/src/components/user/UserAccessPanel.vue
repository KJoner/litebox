<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessDiff,
  type AccessEntryInfo,
  type AccessMode,
  type EntryRef,
  type GrantEntryKind,
  type Node,
  type UserAccessView,
} from '@/api/client'
import { LbEmptyState, LbInfoTip, lbDangerConfirm } from '@/components/lb'

/**
 * 用户的直接入口授权(VNext AUTH01–04)。
 *
 * 规则只在后端视图里,这里不推导「他能不能用」:预览由后端在事务里写进去、读视图、回滚,
 * 与保存是同一份判据。界面上要说清三件事 ——
 *   一、直接授权越过的只是等级门槛,越不过停用、过期、超额与入口停用;
 *   二、撤权与授权一样要重新下发,被移出的机器上还留着他的凭据;
 *   三、Mieru 入口逐入口下发,不会自动做。
 */
const props = defineProps<{ userId: number; nodes: Node[] }>()
const emit = defineEmits<{ changed: [] }>()

interface Row {
  key: string
  kind: GrantEntryKind
  id: number
  name: string
  nodeId: number
  nodeName: string
  tier: string
  enabled: boolean
  restricted: boolean
}

const view = ref<UserAccessView | null>(null)
const loadError = ref(false)
const loading = ref(false)
const mode = ref<AccessMode>('INHERIT')
const picked = ref<Set<string>>(new Set())
const filter = ref('')
const diff = ref<AccessDiff | null>(null)
const previewError = ref('')
const saving = ref(false)

/**
 * 撤权收敛(AUTH04):保存之后逐台说清「撤销待下发 / 已下发 / 下发失败 / 离线」。
 * 只读库里的配置状态,不连节点;新连接与已有连接分开说 —— 凭据从配置里删掉只能证明新连接会被拒绝,
 * sing-box 下发会重启进程所以已有连接也断了,而 Mieru 的用户变更走 reload,旧会话可能继续存活。
 * 节点离线时不说已阻止任何东西。
 */
const lastRevoked = ref<AccessEntryInfo[]>([])
const convergeNodes = ref<Node[]>([])
const convergeError = ref(false)
async function refreshConverge() {
  convergeError.value = false
  try {
    convergeNodes.value = (await api.nodes()).items
  } catch {
    convergeError.value = true
  }
}
function convergeText(e: AccessEntryInfo): { text: string; tone: 'ok' | 'warn' | 'bad' } {
  const n = convergeNodes.value.find((x) => x.id === e.node_id)
  if (!n) return { text: '状态读不到', tone: 'warn' }
  if (n.status === 'OFFLINE') return { text: '机器离线:不能声称已经阻止任何连接,恢复连接后会优先下发', tone: 'bad' }
  // 共享凭据(R04):节点上没有他的逐用户身份可删,配置状态「已同步」说明不了他连不上。
  if (e.shared_credential)
    return {
      text:
        '没有撤销:这个入口用共享 psk,节点上没有他的单独凭据 —— 只是订阅里不再给他这个入口,他手里的 psk 仍然能连。' +
        '要真正收回:在入口上「轮换 psk」(所有人一起换、都要重新拉订阅)、改成逐用户凭据,或停用这个入口',
      tone: 'bad',
    }
  // 持久化下发队列(R05):有这一行就说明还没落到节点上,按它的状态说实话。
  const q = (n.pending_applies ?? []).find((a) => a.kind === (e.kind === 'MIERU' ? 'MIERU' : 'SINGBOX'))
  if (q?.state === 'BLOCKED')
    return { text: `自动下发停下了,他的凭据仍在节点上:${q.last_error || '原因见部署记录'}`, tone: 'bad' }
  if (q?.state === 'RETRY')
    return {
      text: `节点暂时没下发成功(${q.last_error || '连不上'}),第 ${q.attempts} 次失败,会自动重试;在那之前他的凭据仍在节点上`,
      tone: 'bad',
    }
  if (q) return { text: '撤销排队下发中:节点上还留着他的凭据', tone: 'warn' }
  if (e.kind === 'MIERU')
    return {
      text: '已下发(mita reload):新连接会被拒绝;已经建立的会话可能继续存活 —— reload 不断旧会话',
      tone: 'ok',
    }
  switch (n.config_state) {
    case 'IN_SYNC':
      return { text: '已撤销:凭据已从节点配置删除,新连接会被拒绝;下发时 sing-box 重启,已建立的连接随之断开', tone: 'ok' }
    case 'PENDING':
      return { text: '撤销待下发:节点上还留着他的凭据', tone: 'warn' }
    case 'DEPLOY_FAILED':
      return { text: '下发失败:节点在跑旧配置,他的凭据仍然有效', tone: 'bad' }
    default:
      return { text: '配置状态未知,不能确认已撤销', tone: 'warn' }
  }
}

const keyOf = (kind: string, id: number) => `${kind}:${id}`

const rows = computed<Row[]>(() => {
  const out: Row[] = []
  for (const n of props.nodes) {
    if (n.role === 'RELAY') continue
    for (const i of n.inbounds ?? []) {
      out.push({
        key: keyOf('SINGBOX', i.id), kind: 'SINGBOX', id: i.id, name: i.display_name,
        nodeId: n.id, nodeName: n.name, tier: i.access_tier_name, enabled: i.enabled, restricted: !!i.restricted,
      })
    }
    for (const m of n.mieru_inbounds ?? []) {
      out.push({
        key: keyOf('MIERU', m.id), kind: 'MIERU', id: m.id, name: m.display_name,
        nodeId: n.id, nodeName: n.name, tier: m.access_tier_name, enabled: m.enabled, restricted: !!m.restricted,
      })
    }
  }
  return out
})

const groups = computed(() => {
  const q = filter.value.trim().toLowerCase()
  const map = new Map<number, { nodeName: string; rows: Row[] }>()
  for (const r of rows.value) {
    if (q && !`${r.nodeName} ${r.name}`.toLowerCase().includes(q)) continue
    const g = map.get(r.nodeId) ?? { nodeName: r.nodeName, rows: [] }
    g.rows.push(r)
    map.set(r.nodeId, g)
  }
  return [...map.values()]
})

const effectiveKeys = computed(() => new Set((view.value?.effective ?? []).map((e) => keyOf(e.kind, e.id))))

const savedKeys = computed(() => new Set((view.value?.grants ?? []).map((e) => keyOf(e.kind, e.id))))
const dirty = computed(() => {
  if (!view.value) return false
  if (mode.value !== view.value.access_mode) return true
  if (picked.value.size !== savedKeys.value.size) return true
  for (const k of picked.value) if (!savedKeys.value.has(k)) return true
  return false
})

function grantsBody(): EntryRef[] {
  return [...picked.value].map((k) => {
    const [kind, id] = k.split(':')
    return { kind: kind as GrantEntryKind, id: Number(id) }
  })
}

async function load() {
  loading.value = true
  loadError.value = false
  try {
    const v = await api.userAccess(props.userId)
    view.value = v
    mode.value = v.access_mode
    picked.value = new Set(v.grants.map((g) => keyOf(g.kind, g.id)))
    diff.value = null
  } catch {
    loadError.value = true
  } finally {
    loading.value = false
  }
}

let seq = 0
let timer: ReturnType<typeof setTimeout> | undefined
watch([mode, picked], () => {
  if (!dirty.value) {
    diff.value = null
    previewError.value = ''
    return
  }
  clearTimeout(timer)
  timer = setTimeout(async () => {
    const mine = ++seq
    try {
      const d = await api.previewUserAccess(props.userId, { access_mode: mode.value, grants: grantsBody() })
      if (mine === seq) {
        diff.value = d
        previewError.value = ''
      }
    } catch (e) {
      if (mine === seq) previewError.value = e instanceof ApiError ? e.message : '预览失败'
    }
  }, 250)
}, { deep: true })

watch(() => props.userId, () => void load(), { immediate: true })

function toggle(r: Row, on: boolean) {
  const next = new Set(picked.value)
  if (on) next.add(r.key)
  else next.delete(r.key)
  picked.value = next
}

function nodeNameOf(id: number) {
  return props.nodes.find((n) => n.id === id)?.name ?? `#${id}`
}

function save() {
  const d = diff.value
  const impacts: string[] = []
  if (d) {
    if (d.added.length) impacts.push(`新增 ${d.added.length} 个入口:${d.added.map((e) => `${e.node_name}/${e.name}`).join('、')}`)
    if (d.removed.length)
      impacts.push(`**撤销 ${d.removed.length} 个入口**:${d.removed.map((e) => `${e.node_name}/${e.name}`).join('、')}`)
    const singbox = [...new Set([...d.added, ...d.removed].filter((e) => e.kind === 'SINGBOX').map((e) => e.node_id))]
    if (singbox.length)
      impacts.push(`会自动重新下发 sing-box 的机器:${singbox.map(nodeNameOf).join('、')} —— 重启时这些机器上**全部入口**的在线连接会断一下`)
    const shared = d.removed.filter((e) => e.shared_credential)
    if (shared.length)
      impacts.unshift(
        `**${shared.map((e) => `${e.node_name}/${e.name}`).join('、')} 用的是共享 psk:移出授权收不回他手里的凭据**,` +
          '只是订阅里不再出现;要真正收回请轮换那个入口的 psk 或停用它',
      )
    const mieru = [...d.added, ...d.removed].filter((e) => e.kind === 'MIERU')
    if (mieru.length)
      impacts.push(
        `Mieru 入口会自动 reload(只动了用户的那几个,一条连接都不断,已建立的会话可能继续存活):` +
          `${mieru.map((e) => `${e.node_name}/${e.name}`).join('、')}。` +
          '同时还有端口 / 传输层 / 出口改动没下发的入口不会自动做,会在下方点名',
      )
  }
  if (!impacts.length) impacts.push('他能用的入口集合不变,不会触发任何下发')
  lbDangerConfirm({
    title: '保存入口授权?',
    okText: '保存',
    okType: d && d.removed.length ? 'danger' : 'primary',
    impacts,
    footer: '直接授权越过的只是等级门槛:账号停用、过期、超额与入口停用照样生效。',
    onOk: () => {
      void doSave()
    },
  })
}

async function doSave() {
  saving.value = true
  try {
    const res = await api.setUserAccess(props.userId, { access_mode: mode.value, grants: grantsBody() })
    lastRevoked.value = res.removed
    if (res.removed.length) void refreshConverge()
    if (res.shared_revoked?.length)
      message.warning(`已保存,但 ${res.shared_revoked.length} 个共享 psk 入口没有真正收回凭据,见下方「撤权收敛」`)
    else
      message.success(
        res.mieru_pending.length
          ? `已保存。${res.mieru_pending.length} 个 Mieru 入口会自动 reload,结果见下方「撤权收敛」`
          : '已保存',
      )
    await load()
    emit('changed')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}

function reset() {
  if (!view.value) return
  mode.value = view.value.access_mode
  picked.value = new Set(view.value.grants.map((g) => keyOf(g.kind, g.id)))
}
</script>

<template>
  <div class="ua">
    <LbEmptyState
      v-if="loadError"
      variant="error"
      title="入口授权暂时读不到"
      description="不代表他没有任何授权;档案里的「可用节点」仍按库里的数据显示。"
      @retry="load"
    />
    <a-skeleton v-else-if="loading && !view" active :paragraph="{ rows: 4 }" />
    <template v-else-if="view">
      <section class="ua__card">
        <div class="ua__head">
          授权模式
          <LbInfoTip
            text="继承:等级继承 + 整机额外授权 + 直接授权三者合起来。仅直接授权:只认下面勾选的入口,等级与整机授权都不算。「仅指定用户」的入口在两种模式下都只经直接授权出现。"
          />
        </div>
        <a-radio-group v-model:value="mode" class="ua__mode">
          <a-radio value="INHERIT">继承等级 + 直接授权</a-radio>
          <a-radio value="DIRECT_ONLY">仅直接授权</a-radio>
        </a-radio-group>
      </section>

      <section class="ua__card">
        <div class="ua__head">
          直接授权的入口 <span class="ua__count">{{ picked.size }}</span>
          <a-input v-model:value="filter" size="small" allow-clear placeholder="按机器或入口名筛选" class="ua__filter" />
        </div>
        <div v-if="!groups.length" class="ua__note">没有可授权的入口(中转机与转发不走这里)。</div>
        <div v-for="g in groups" :key="g.nodeName" class="ua__group">
          <div class="ua__node">{{ g.nodeName }}</div>
          <label v-for="r in g.rows" :key="r.key" class="ua__row">
            <a-checkbox :checked="picked.has(r.key)" @change="(e: any) => toggle(r, e.target.checked)" />
            <span class="ua__name">{{ r.name }}</span>
            <span class="ua__chip">{{ r.kind === 'MIERU' ? 'Mieru' : 'sing-box' }}</span>
            <span class="ua__chip">{{ r.tier }}</span>
            <span v-if="r.restricted" class="ua__chip ua__chip--warn">仅指定用户</span>
            <span v-if="!r.enabled" class="ua__chip ua__chip--muted">已停用</span>
            <span v-if="effectiveKeys.has(r.key)" class="ua__now">当前可用</span>
          </label>
        </div>
      </section>

      <section v-if="lastRevoked.length" class="ua__card">
        <div class="ua__head">
          撤权收敛
          <a-button size="small" class="ua__filter" @click="refreshConverge">刷新状态</a-button>
        </div>
        <div v-if="convergeError" class="ua__note ua__note--bad">机器状态读不到,不代表撤权失败。</div>
        <div v-for="e in lastRevoked" :key="e.kind + e.id" class="ua__conv">
          <span class="ua__name">{{ e.node_name }}/{{ e.name }}</span>
          <span class="ua__note" :class="`ua__tone--${convergeText(e).tone}`">{{ convergeText(e).text }}</span>
        </div>
        <div class="ua__note">历史流量与审计不受影响。只改订阅可见性不算撤权 —— 这里看的是节点配置。</div>
      </section>

      <section v-if="dirty" class="ua__card ua__card--diff">
        <div class="ua__head">保存之后的变化</div>
        <div v-if="previewError" class="ua__note ua__note--bad">{{ previewError }}</div>
        <template v-else-if="diff">
          <div v-if="!diff.added.length && !diff.removed.length" class="ua__note">能用的入口集合不变。</div>
          <div v-if="diff.added.length" class="ua__diff">
            <b class="ua__plus">+ {{ diff.added.length }}</b>
            <span v-for="e in diff.added" :key="e.kind + e.id" class="ua__chip">{{ e.node_name }}/{{ e.name }}</span>
          </div>
          <div v-if="diff.removed.length" class="ua__diff">
            <b class="ua__minus">− {{ diff.removed.length }}</b>
            <span v-for="e in diff.removed" :key="e.kind + e.id" class="ua__chip">{{ e.node_name }}/{{ e.name }}</span>
          </div>
          <div v-if="diff.affected_node_ids.length" class="ua__note">
            涉及机器:{{ diff.affected_node_ids.map(nodeNameOf).join('、') }}
          </div>
        </template>
        <div v-else class="ua__note">正在计算…</div>
        <div class="ua__ops">
          <a-button size="small" @click="reset">还原</a-button>
          <a-button size="small" type="primary" :loading="saving" :disabled="!!previewError" @click="save">保存</a-button>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.ua {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.ua__card {
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  background: var(--surface);
  padding: var(--sp-3) var(--sp-4);
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.ua__card--diff {
  border-color: var(--brand-border);
}
.ua__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-weight: 600;
  font-size: var(--fs-sm);
}
.ua__count {
  color: var(--text3);
  font-weight: 500;
}
.ua__filter {
  margin-left: auto;
  max-width: 200px;
}
.ua__mode {
  display: flex;
  gap: var(--sp-4);
  flex-wrap: wrap;
}
.ua__group {
  display: flex;
  flex-direction: column;
}
.ua__node {
  font-size: var(--fs-xs);
  color: var(--text3);
  padding: var(--sp-1) 0;
}
.ua__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 4px 0;
  cursor: pointer;
  flex-wrap: wrap;
}
.ua__name {
  font-size: var(--fs-sm);
}
.ua__chip {
  font-size: var(--fs-xs);
  padding: 0 6px;
  border-radius: var(--r-xs);
  background: var(--surface2);
  color: var(--text2);
}
.ua__chip--warn {
  background: var(--warn-bg);
  color: var(--warn);
}
.ua__chip--muted {
  color: var(--text3);
}
.ua__now {
  margin-left: auto;
  font-size: var(--fs-xs);
  color: var(--ok);
}
.ua__note {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.ua__note--bad {
  color: var(--bad);
}
.ua__diff {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  align-items: center;
}
.ua__conv {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.ua__tone--ok {
  color: var(--ok);
}
.ua__tone--warn {
  color: var(--warn);
}
.ua__tone--bad {
  color: var(--bad);
}
.ua__plus {
  color: var(--ok);
}
.ua__minus {
  color: var(--bad);
}
.ua__ops {
  display: flex;
  justify-content: flex-end;
  gap: var(--sp-2);
}
</style>
