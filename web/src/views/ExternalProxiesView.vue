<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type ExpiryKind,
  type ExternalProxy,
  type ProxySource,
  EXTERNAL_PROTOCOL_LABEL,
} from '@/api/client'
import ExternalProxyModal from '@/components/external/ExternalProxyModal.vue'
import ProxySourceModal from '@/components/external/ProxySourceModal.vue'
import ExpiryModal from '@/components/expiry/ExpiryModal.vue'
import {
  expiryFilterOptions,
  expiryStatusMeta,
  matchExpiryFilter,
  type ExpiryFilter,
} from '@/components/expiry/expiryMeta'
import {
  LbEmptyState,
  LbNameConfirm,
  LbRowCard,
  LbStatusTag,
  LbTimeText,
  expiredMeta,
  lbDangerConfirm,
  missingMeta,
  syncFailedMeta,
  type LbStatusMeta,
  LbInfoTip,
  LbSectionTitle,
  LbIcon,
} from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import { daysUntil, formatBytes, formatUTCTime } from '@/utils/format'

/**
 * 外部代理:不属于本面板、不由本面板部署的成品线路。
 *
 * 一个页面而不是两个:源很少(一般 1~3 个),条目很多(几十上百)。
 * 拆成两页的话「同步完看结果」要来回跳。源做成横排卡片,
 * 哪个源出问题一眼可见;点卡片即筛选该源的条目。
 *
 * 这一页刻意**没有**流量列:外部代理的流量走的是上游的服务器,
 * 面板统计不到。给一个 0 会被读成「没人用过」,那比不显示更糟。
 */
const narrow = useNarrow()

const sources = ref<ProxySource[]>([])
const proxies = ref<ExternalProxy[]>([])
const tiers = ref<AccessTier[]>([])
const excludedCount = ref(0)
const loading = ref(false)
const loadError = ref('')
const busy = reactive<Record<number, string>>({})

/** 阈值由后端下发,免得两边各写一个数字。 */
const syncFailThreshold = ref(3)
const missingThreshold = ref(3)

/** null = 全部来源;0 = 只看手工添加的。 */
const sourceFilter = ref<number | null>(null)
const showExcluded = ref(false)
const keyword = ref('')

const proxyModal = reactive({ open: false, target: null as ExternalProxy | null })
const sourceModal = reactive({ open: false, target: null as ProxySource | null })
/** 「续费 / 修改到期时间」弹窗(V20):条目与源共用一个。 */
const expiryModal = reactive({
  open: false,
  kind: 'EXTERNAL_PROXY' as ExpiryKind,
  id: 0,
  name: '',
  sourceName: '',
})
function openExpiryProxy(p: ExternalProxy) {
  Object.assign(expiryModal, {
    open: true, kind: 'EXTERNAL_PROXY', id: p.id, name: p.final_display_name,
    sourceName: p.source_id ? (p.source_name || '') : '',
  })
}
function openExpirySource(s: ProxySource) {
  Object.assign(expiryModal, { open: true, kind: 'PROXY_SOURCE', id: s.id, name: s.name, sourceName: '' })
}
/** 到期状态筛选(V20)。 */
const expiryFilter = ref<ExpiryFilter>('ALL')
const deleteTarget = ref<ExternalProxy | null>(null)
const deleteSourceTarget = ref<ProxySource | null>(null)
/** 删源时条目的去向。**没有默认值** —— 见 confirmDeleteSource。 */
const deleteSourceMode = ref<'delete' | 'detach' | ''>('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [s, p, t] = await Promise.all([
      api.proxySources(),
      api.externalProxies({ sourceId: sourceFilter.value, includeExcluded: showExcluded.value }),
      api.accessTiers(),
    ])
    sources.value = s.items
    syncFailThreshold.value = s.sync_failure_alert_threshold
    missingThreshold.value = s.missing_rounds_before_unlist
    proxies.value = p.items
    excludedCount.value = p.excluded_count
    tiers.value = t.items
  } catch (err) {
    // 整表失败时保持空白,不显示「暂无数据」—— 那会被读成「一条都没有」。
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
    sources.value = []
    proxies.value = []
  } finally {
    loading.value = false
  }
}

onMounted(load)

const visible = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return proxies.value.filter(
    (p) =>
      matchExpiryFilter(p.expiry, expiryFilter.value) &&
      (!kw ||
        p.name.toLowerCase().includes(kw) ||
        p.final_display_name.toLowerCase().includes(kw) ||
        p.server.toLowerCase().includes(kw)),
  )
})

/**
 * 条目状态。三种情况都是红的,靠形状与文案分开 ——
 * 「已到期」要去续费,「上游消失」要去问机场,两件完全不同的事。
 */
function statusTags(p: ExternalProxy): LbStatusMeta[] {
  const out: LbStatusMeta[] = []
  const expired = p.expires_at ? (daysUntil(p.expires_at) ?? 1) <= 0 : false
  if (expired) out.push(expiredMeta)
  if (p.missing_rounds > 0) out.push(missingMeta(p.missing_rounds, missingThreshold.value))
  return out
}

function sourceOf(p: ExternalProxy): ProxySource | undefined {
  return sources.value.find((s) => s.id === p.source_id)
}

/** 源到期后它下面全部条目一起退出订阅 —— 条目自己看不出来,要在这里说。 */
function sourceExpiredNote(p: ExternalProxy): string {
  const src = sourceOf(p)
  if (!src) return ''
  const at = src.expires_at || src.upstream_expires_at
  if (!at) return src.enabled ? '' : `来源「${src.name}」已停用,该条目不在订阅里`
  const left = daysUntil(at)
  if (left !== null && left <= 0) return `来源「${src.name}」已到期,该条目不在订阅里`
  return ''
}

function expiryText(at: string | null): string {
  if (!at) return '不过期'
  const left = daysUntil(at)
  const day = formatUTCTime(at)
  if (left === null) return day
  if (left <= 0) return `${day} · 已过期`
  return `${day} · ${left} 天后`
}

async function act(id: number, label: string, fn: () => Promise<unknown>) {
  busy[id] = label
  try {
    await fn()
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    delete busy[id]
  }
}

function toggleSubscription(p: ExternalProxy) {
  const next = !p.subscription_enabled
  act(p.id, next ? '恢复下发' : '停止下发', async () => {
    await api.setExternalProxySubscription(p.id, next)
    message.success(next ? '已恢复下发到订阅' : '已停止下发到订阅')
  })
}

function toggleEnabled(p: ExternalProxy) {
  const next = p.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE'
  act(p.id, '切换状态', async () => {
    await api.setExternalProxyStatus(p.id, next)
    message.success(next === 'ACTIVE' ? '已启用' : '已停用')
  })
}

async function checkReachable(p: ExternalProxy) {
  busy[p.id] = '测试连通性'
  try {
    const r = await api.checkExternalProxy(p.id)
    Modal[r.ok ? 'info' : 'warning']({
      title: r.ok ? '端口可达' : '连接失败',
      width: 480,
      content: `${r.message}\n\n${r.disclaimer}`,
      okText: '知道了',
    })
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '连通性检查失败')
  } finally {
    delete busy[p.id]
  }
}

/** 转为手工条目:不可逆,但影响面局限于这一条 —— 用 lbDangerConfirm。 */
function confirmDetach(p: ExternalProxy) {
  lbDangerConfirm({
    title: `把「${p.final_display_name}」转为手工条目?`,
    impacts: [
      `脱离来源「${p.source_name}」,此后同步不再碰它`,
      '机场那边改地址、改密码都不会再同步过来',
      '上游那一条会在下次同步时作为新条目重新进来',
      '之后你才能手工改它的地址与凭据',
    ],
    okText: '转为手工条目',
    footer: '这一步不可逆:转回去意味着重新与上游对齐,而那时上游的那一条可能早就不在了。',
    onOk: () =>
      act(p.id, '转为手工', async () => {
        await api.detachExternalProxy(p.id)
        message.success('已转为手工条目')
      }),
  })
}

/**
 * 手工同步。按 V3 的分档规则属于「可逆但影响面大」——
 * 它可能让一批条目退出订阅。
 */
function confirmSync(src: ProxySource) {
  lbDangerConfirm({
    title: `立即同步「${src.name}」?`,
    impacts: [
      '会按上游的最新列表更新地址与凭据(你锁定过的字段不会被覆盖)',
      '上游新增的条目会自动加进来',
      `连续 ${missingThreshold.value} 轮没在上游出现的条目会自动退出订阅`,
      '同步失败时一条都不会改动',
    ],
    okText: '立即同步',
    okType: 'primary',
    footer: '不会删除任何条目 —— 上游消失的条目只是退出订阅,记录与你配的等级、排序全部保留。',
    onOk: () =>
      act(src.id, '同步', async () => {
        const r = await api.syncProxySource(src.id)
        const parts = [`新增 ${r.added}`, `更新 ${r.updated}`, `不变 ${r.unchanged}`]
        if (r.missing) parts.push(`上游未出现 ${r.missing}`)
        if (r.skipped) parts.push(`跳过 ${r.skipped}`)
        message.success(`同步完成:${parts.join(',')}`)
        if (r.unlisted.length) {
          Modal.warning({
            title: `${r.unlisted.length} 条已退出订阅`,
            width: 480,
            content: `连续 ${missingThreshold.value} 轮没在上游出现:${r.unlisted.join('、')}。\n\n记录仍然保留,机场恢复后可在列表里手工恢复下发。`,
            okText: '知道了',
          })
        }
      }),
  })
}

/**
 * 删除代理源。条目的去向**必须由管理员选,没有默认值**:
 * 默认删除会让手滑一次丢掉几十条配置,默认保留会留下一堆无主条目。
 */
function confirmDeleteSource(src: ProxySource) {
  deleteSourceMode.value = ''
  deleteSourceTarget.value = src
}

async function doDeleteSource() {
  const src = deleteSourceTarget.value
  const mode = deleteSourceMode.value
  if (!src || !mode) return
  try {
    const r = await api.deleteProxySource(src.id, mode)
    deleteSourceTarget.value = null
    await load()
    message.success(
      `已删除代理源,${r.affected} 条条目${mode === 'delete' ? '一并删除' : '转为手工条目'}`,
    )
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '删除失败')
  }
}

async function doDeleteProxy() {
  const p = deleteTarget.value
  if (!p) return
  try {
    await api.deleteExternalProxy(p.id)
    deleteTarget.value = null
    await load()
    message.success('已删除')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '删除失败')
  }
}

async function showCredentials(p: ExternalProxy) {
  try {
    const r = await api.externalProxyCredentials(p.id)
    Modal.info({
      title: `${p.final_display_name} 的凭据`,
      width: 560,
      content: `加密方法:${r.method}\n密码:${r.password}${r.plugin ? `\n插件:${r.plugin} ${r.plugin_opts}` : ''}${r.share_uri ? `\n\n原始链接:\n${r.share_uri}` : ''}\n\n这次查看已写入审计日志。`,
      okText: '关闭',
    })
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '读取凭据失败')
  }
}

function sourceStatusTags(src: ProxySource): LbStatusMeta[] {
  const out: LbStatusMeta[] = []
  if (src.consecutive_failures >= syncFailThreshold.value) {
    out.push(syncFailedMeta(src.consecutive_failures))
  }
  const at = src.expires_at || src.upstream_expires_at
  if (at && (daysUntil(at) ?? 1) <= 0) out.push(expiredMeta)
  return out
}

/**
 * 走 QUIC 的那两种(Hysteria2 / TUIC)不能当自建入口的出口,也不能被 nginx 透传。
 * 界限不在协议身上,在我们这一侧 —— 说清楚是哪一侧,管理员才知道这不是他配错了。
 */
function protocolLabel(p: ExternalProxy): string {
  return EXTERNAL_PROTOCOL_LABEL[p.protocol] ?? p.protocol
}

const quicNote =
  '走 QUIC:节点上的 sing-box 是精简构建(不含 with_quic)拨不动它,' +
  'nginx 透传也只搬 TCP 字节。照常下发给用户直连,但不能当入口的出口或转发落地。'

// 两种「到期」分两列:可用截止是面板控制的(到期即退出订阅),商家到期只提醒。
// 混在一格里的话,管理员分不清"用户已经连不上了"和"该去续费了"。
const columns = [
  { title: '条目', key: 'item' },
  { title: '地址', key: 'addr', width: 220 },
  { title: '访问等级', key: 'tier', width: 100 },
  { title: '可用截止', key: 'expiry', width: 170 },
  { title: '商家到期', key: 'vendor', width: 150 },
  { title: '订阅', key: 'sub', width: 80 },
  { title: '操作', key: 'ops', width: 150 },
]
</script>

<template>
  <div class="lb-page xp">
    <div class="lb-page__head">
      <div class="lb-page__title-wrap">
        <h1 class="lb-page__title">
          <span>外部代理</span>
          <LbInfoTip :width="320">
            不属于本面板、不由本面板部署的成品线路。可以合并进用户的订阅,但<b>统计不到流量</b> —— 流量走的是上游的服务器。
            订阅源卡片点一下即按来源筛选;「同步」会按上游列表更新条目,上游消失的条目连续 3 轮才退出订阅,永不自动删除。
          </LbInfoTip>
        </h1>
        <div class="lb-page__summary">
          {{ proxies.length }} 条线路,来自 {{ sources.length }} 个订阅源。
        </div>
      </div>
      <div class="lb-page__actions">
        <a-button @click="load">刷 新</a-button>
        <a-button @click="proxyModal.target = null; proxyModal.open = true">添加代理</a-button>
        <a-button type="primary" @click="sourceModal.target = null; sourceModal.open = true">
          导入订阅源
        </a-button>
      </div>
    </div>

    <a-alert
      v-if="loadError"
      type="error"
      show-icon
      class="xp__alert"
      message="加载失败"
      :description="loadError"
    />

    <!-- 源卡片横排:哪个源出问题一眼可见,点卡片即筛选 -->
    <div class="xp__sources">
      <div
        class="xp__card"
        :class="{ 'xp__card--on': sourceFilter === null }"
        @click="sourceFilter = null; load()"
      >
        <div class="xp__card-name">全部来源</div>
        <div class="xp__card-num">{{ proxies.length }}</div>
        <div class="xp__card-note">含手工添加</div>
      </div>

      <div
        v-for="src in sources"
        :key="src.id"
        class="xp__card"
        :class="{ 'xp__card--on': sourceFilter === src.id }"
        @click="sourceFilter = src.id; load()"
      >
        <div class="xp__card-name">
          {{ src.name }}
          <span v-if="!src.enabled" class="xp__off">已停用</span>
        </div>
        <div class="xp__card-num">{{ src.proxy_count }}</div>
        <div class="xp__card-tags">
          <LbStatusTag v-for="(m, i) in sourceStatusTags(src)" :key="i" :meta="m" />
        </div>
        <div class="xp__card-note">
          <template v-if="src.last_sync_at">
            <LbTimeText :value="src.last_sync_at" /> 同步
          </template>
          <template v-else>从未同步</template>
          <br />
          可用截止 {{ expiryText(src.expires_at || src.upstream_expires_at) }}
          <br />
          商家到期 <LbStatusTag :meta="expiryStatusMeta(src.expiry)" />
          <template v-if="src.upstream_total_bytes">
            <br />
            上游 {{ formatBytes(src.upstream_used_bytes) }} /
            {{ formatBytes(src.upstream_total_bytes) }}
          </template>
        </div>
        <div class="xp__card-ops" @click.stop>
          <a :class="{ 'xp__dim': busy[src.id] }" @click="confirmSync(src)">
            {{ busy[src.id] === '同步' ? '同步中…' : '同步' }}
          </a>
          <a @click="sourceModal.target = src; sourceModal.open = true">编辑</a>
          <a @click="openExpirySource(src)">续费</a>
          <a class="xp__danger" @click="confirmDeleteSource(src)">删除</a>
        </div>
      </div>

      <div class="xp__card xp__card--add" @click="sourceModal.target = null; sourceModal.open = true">
        + 添加代理源
      </div>
    </div>

    <section>
    <LbSectionTitle title="全部线路" :count="`${visible.length} / ${proxies.length} 条`" />
    <div class="lb-card lb-card--flush xp__table">
    <div class="lb-filter xp__filter">
      <a-input v-model:value="keyword" placeholder="名称 / 地址" allow-clear>
        <template #prefix><LbIcon name="search" :size="14" /></template>
      </a-input>
      <a-select v-model:value="expiryFilter" class="lb-filter__select" style="min-width: 150px">
        <a-select-option v-for="o in expiryFilterOptions" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
      </a-select>
      <label class="lb-filter__toggle" :class="{ 'lb-filter__toggle--on': showExcluded }">
        <a-switch v-model:checked="showExcluded" size="small" @change="load" />
        显示已排除({{ excludedCount }})
      </label>
    </div>

    <!-- <768 整表换卡片:横向滚动会把「操作」列推到屏幕外 -->
    <div v-if="narrow" class="xp__cards">
      <LbRowCard v-for="p in visible" :key="p.id">
        <template #head>
          <span class="xp__sort lb-mono">#{{ p.sort_order }}</span>
          <a class="xp__name" @click="proxyModal.target = p; proxyModal.open = true">
            {{ p.final_display_name }}
          </a>
          <LbStatusTag kind="externalProxy" :status="p.status" />
        </template>
        <div class="xp__addr lb-mono">{{ p.server }}:{{ p.port }} · {{ p.access_tier_name }}</div>
        <div class="xp__tagrow">
          <LbStatusTag v-for="(m, i) in statusTags(p)" :key="i" :meta="m" />
        </div>
        <div class="xp__note">
          {{ p.source_id ? `来自 ${p.source_name}` : '手工添加' }} ·
          可用截止 {{ expiryText(p.expires_at) }}
        </div>
        <div class="xp__note">
          商家到期 <LbStatusTag :meta="expiryStatusMeta(p.expiry)" />
          <a @click="openExpiryProxy(p)">续费</a>
        </div>
        <div v-if="sourceExpiredNote(p)" class="xp__warn">{{ sourceExpiredNote(p) }}</div>
        <template #foot>
          <a @click="proxyModal.target = p; proxyModal.open = true">编辑</a>
          <a @click="toggleSubscription(p)">
            {{ p.subscription_enabled ? '停止下发' : '恢复下发' }}
          </a>
          <a class="xp__danger" @click="deleteTarget = p">删除</a>
        </template>
      </LbRowCard>
      <LbEmptyState
        v-if="!visible.length && !loading && !loadError"
        variant="empty"
        title="还没有外部代理"
        description="可以粘贴一条分享链接直接添加,或者导入一个机场订阅。"
      />
    </div>

    <a-table
      v-else
      :columns="columns"
      :data-source="visible"
      :loading="loading"
      row-key="id"
      size="middle"
      :pagination="{ pageSize: 20, showSizeChanger: true }"
      :scroll="{ x: 1000 }"
    >
      <template #emptyText>
        <LbEmptyState
          v-if="!loadError"
          variant="empty"
          title="还没有外部代理"
          description="可以粘贴一条分享链接直接添加,或者导入一个机场订阅。"
        />
        <span v-else />
      </template>

      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'item'">
          <span class="xp__sort lb-mono" :title="`排序值 ${record.sort_order}`">
            #{{ record.sort_order }}
          </span>
          <a @click="proxyModal.target = record; proxyModal.open = true">
            {{ record.final_display_name }}
          </a>
          <span v-if="record.name !== record.final_display_name" class="xp__inner">
            {{ record.name }}
          </span>
          <div class="xp__tagrow">
            <LbStatusTag kind="externalProxy" :status="record.status" />
            <LbStatusTag v-for="(m, i) in statusTags(record)" :key="i" :meta="m" />
            <span v-if="record.locked_list.length" class="xp__lock" title="这些字段同步时不会被上游覆盖">
              已锁定 {{ record.locked_list.length }} 项
            </span>
          </div>
          <div class="xp__note">
            {{ protocolLabel(record) }} ·
            {{ record.source_id ? `来自 ${record.source_name}` : '手工添加' }}
            <!-- 「不能当出口」要在列表里就看得见。等管理员在节点详情里选到一半
                 才发现列表里没有这一条,他会先怀疑是权限或者页面坏了。 -->
            <span
              v-if="!record.dialable_by_node"
              class="xp__warn"
              :title="record.dialable_reason || quicNote"
            >
              · 只能直连
            </span>
          </div>
          <div v-if="sourceExpiredNote(record)" class="xp__warn">
            {{ sourceExpiredNote(record) }}
          </div>
        </template>

        <template v-else-if="column.key === 'addr'">
          <div class="xp__addr lb-mono">{{ record.server }}:{{ record.port }}</div>
          <div class="xp__note">
            <a @click="showCredentials(record)">查看凭据</a>
            <template v-if="record.last_check_at">
              · 上次检查
              <span :class="record.last_check_ok ? 'xp__ok' : 'xp__danger'">
                {{ record.last_check_ok ? '可达' : '失败' }}
              </span>
            </template>
          </div>
        </template>

        <template v-else-if="column.key === 'tier'">{{ record.access_tier_name }}</template>

        <template v-else-if="column.key === 'expiry'">
          <span :class="{ xp__danger: record.expires_at && (daysUntil(record.expires_at) ?? 1) <= 0 }">
            {{ expiryText(record.expires_at) }}
          </span>
        </template>

        <template v-else-if="column.key === 'vendor'">
          <a class="xp__vendor" @click="openExpiryProxy(record)">
            <LbStatusTag :meta="expiryStatusMeta(record.expiry)" />
            <span v-if="record.expiry?.inherited" class="xp__note">跟随来源</span>
            <span v-if="record.expiry?.auto_renew && record.expiry?.expires_at" class="xp__note">自动续费</span>
          </a>
        </template>

        <template v-else-if="column.key === 'sub'">
          <a-switch
            :checked="record.subscription_enabled"
            :loading="!!busy[record.id]"
            size="small"
            @change="toggleSubscription(record)"
          />
        </template>

        <template v-else-if="column.key === 'ops'">
          <a-button size="small" @click="proxyModal.target = record; proxyModal.open = true">
            编 辑
          </a-button>
          <a-dropdown>
            <a-button size="small" style="margin-left: 6px">···</a-button>
            <template #overlay>
              <a-menu>
                <a-menu-item @click="openExpiryProxy(record)">续费 / 修改到期时间</a-menu-item>
                <a-menu-item @click="checkReachable(record)">测试连通性</a-menu-item>
                <a-menu-item @click="toggleEnabled(record)">
                  {{ record.status === 'ACTIVE' ? '停用' : '启用' }}
                </a-menu-item>
                <a-menu-item v-if="record.origin === 'IMPORTED'" @click="confirmDetach(record)">
                  转为手工条目
                </a-menu-item>
                <a-menu-item
                  v-if="record.locked_list.length"
                  @click="act(record.id, '解锁', () => api.setExternalProxyLocks(record.id, []))"
                >
                  解除全部字段锁定
                </a-menu-item>
                <a-menu-divider />
                <a-menu-item danger @click="deleteTarget = record">删除</a-menu-item>
              </a-menu>
            </template>
          </a-dropdown>
        </template>
      </template>
    </a-table>
    </div>
    </section>

    <ExternalProxyModal
      v-model:open="proxyModal.open"
      :proxy="proxyModal.target"
      :tiers="tiers"
      @saved="load"
    />
    <ProxySourceModal
      v-model:open="sourceModal.open"
      :source="sourceModal.target"
      :tiers="tiers"
      @saved="load"
    />
    <ExpiryModal
      v-model:open="expiryModal.open"
      :kind="expiryModal.kind"
      :object-id="expiryModal.id"
      :name="expiryModal.name"
      :source-name="expiryModal.sourceName"
      @changed="load"
    />

    <!-- 删除条目不可逆 → 要求输入内部名称 -->
    <LbNameConfirm
      v-if="deleteTarget"
      :open="true"
      title="删除外部代理"
      :name="deleteTarget.name"
      :impacts="[
        '该条目立刻从所有用户的订阅中消失',
        '它的展示名、访问等级、排序与备注一并丢失',
        deleteTarget.source_id
          ? '下次同步时上游那一条会作为新条目重新进来(但你配的东西不会回来)'
          : '手工添加的条目删掉就没有了',
      ]"
      @confirm="doDeleteProxy"
      @update:open="(v: boolean) => { if (!v) deleteTarget = null }"
    />

    <!-- 删源:条目去向必须选,没有默认值 -->
    <a-modal
      :open="!!deleteSourceTarget"
      :title="`删除代理源「${deleteSourceTarget?.name}」`"
      :width="480"
      :ok-button-props="{ disabled: !deleteSourceMode, danger: true }"
      ok-text="删除代理源"
      cancel-text="取消"
      @ok="doDeleteSource"
      @cancel="deleteSourceTarget = null"
    >
      <p class="xp__modal-p">
        它下面有 <strong>{{ deleteSourceTarget?.proxy_count }}</strong> 条代理。
        这些条目怎么处理必须由你决定 —— 这个选择没有安全的默认值:
        默认删除会让手滑一次丢掉几十条配置,默认保留会留下一堆无主条目。
      </p>
      <a-radio-group v-model:value="deleteSourceMode" class="xp__modal-radio">
        <a-radio value="delete">一并删除这些条目</a-radio>
        <a-radio value="detach">保留并转为手工条目(此后不再同步)</a-radio>
      </a-radio-group>
    </a-modal>
  </div>
</template>

<style scoped>
.xp__alert {
  margin: 0;
}

/* 订阅源卡片区放在指标位:每源一张小卡,圆角 16。 */
.xp__sources {
  display: flex;
  gap: 12px;
  overflow-x: auto;
  padding: 4px;
  margin: -4px;
}
.xp__card {
  flex: 0 0 auto;
  width: 220px;
  padding: 14px 20px;
  box-shadow: var(--shadow);
  border-radius: 16px;
  background: var(--surface);
  cursor: pointer;
  transition: transform 0.25s var(--ease), box-shadow 0.25s var(--ease);
}
.xp__card:hover {
  transform: translateY(-2px);
  box-shadow: var(--shadow-hover);
}
.xp__card--on {
  box-shadow: var(--shadow), 0 0 0 2px var(--brand);
}
.xp__card--add {
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text3);
  box-shadow: none;
  border: 1px dashed var(--sep);
  background: transparent;
}
.xp__card-name {
  font-size: 13px;
  font-weight: 600;
}
.xp__card-num {
  font-size: 22px;
  line-height: 1.2;
  font-family: var(--mono);
}
.xp__card-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  margin: 4px 0;
}
.xp__card-note {
  font-size: 11px;
  line-height: 1.6;
  color: var(--text3);
}
.xp__card-ops {
  display: flex;
  gap: 10px;
  margin-top: 8px;
  font-size: 12px;
}
.xp__off {
  margin-left: 6px;
  font-size: 10.5px;
  color: var(--text3);
}

.xp__filter {
  margin: 0;
}

.xp__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.xp__sort {
  display: inline-block;
  margin-right: 6px;
  padding: 0 4px;
  border-radius: var(--r-pill);
  background: var(--fill);
  color: var(--text2);
  font-size: 10px;
}
.xp__inner {
  margin-left: 6px;
  font-family: var(--mono);
  font-size: 10.5px;
  color: var(--text3);
}
.xp__name {
  font-size: 13px;
}
.xp__addr {
  font-size: 11px;
  color: var(--text3);
}
.xp__tagrow {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  margin-top: 3px;
}
.xp__note {
  margin-top: 2px;
  font-size: 11px;
  color: var(--text3);
}
.xp__warn {
  margin-top: 2px;
  font-size: 11px;
  color: var(--warn);
}
.xp__lock {
  padding: 0 5px;
  border-radius: var(--r-pill);
  background: var(--fill);
  color: var(--text2);
  font-size: 10.5px;
}
.xp__danger {
  color: var(--bad);
}
.xp__ok {
  color: var(--ok);
}
.xp__dim {
  color: var(--text3);
}
.xp__modal-p {
  font-size: 13px;
  line-height: 1.8;
  color: var(--text2);
}
.xp__modal-radio {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
</style>
