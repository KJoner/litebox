<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  profileKindHint,
  profileKindLabel,
  type ProfileKind,
  type ProfilePlaceholderInfo,
  type ProxyUser,
  type SubscriptionProfile,
} from '@/api/client'
import { LbEmptyState, LbRowCard, LbStatusTag, LbTimeText, lbDangerConfirm } from '@/components/lb'
import { useNarrow } from '@/composables/useNarrow'
import SubscriptionProfileModal from '@/components/profile/SubscriptionProfileModal.vue'
import { color } from '@/theme/tokens'

/**
 * 订阅配置。管理员把自己调好的整份客户端配置放进来,用户按客户端类型各拉各的。
 *
 * **系统里不预置任何模板。** 内置一份就等于承诺维护它的分流规则、规则集地址
 * 与语法版本 —— 而这些每隔几个月就会变,坏掉的表现是用户的客户端起不来,
 * 用户会以为是面板坏了。没配的类型,门户上整块不出现。
 */
const items = ref<SubscriptionProfile[]>([])
const info = ref<ProfilePlaceholderInfo | null>(null)
const users = ref<ProxyUser[]>([])
const baseURL = ref('')
const loading = ref(true)
const loadError = ref('')
const narrow = useNarrow()

const modalOpen = ref(false)
const editing = ref<SubscriptionProfile | null>(null)

const enabledMeta = {
  text: '启用',
  shape: 'dot' as const,
  fg: color.success,
  bg: color.successBg,
  bd: color.successBorder,
}
const disabledMeta = {
  text: '停用',
  shape: 'square' as const,
  fg: color.neutral,
  bg: color.neutralBg,
  bd: color.neutralBorder,
}

/** 哪些类型已经配了(且启用)—— 没配的那几种用户压根看不到。 */
const configured = computed(() => {
  const set = new Set<ProfileKind>()
  for (const p of items.value) if (p.enabled) set.add(p.kind)
  return set
})

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    items.value = (await api.subscriptionProfiles()).items
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载配置文件失败'
    items.value = []
  } finally {
    loading.value = false
  }
  // 三份附属数据各自降级:占位符说明、用户列表、站点根都读不到也不影响看列表。
  api.profilePlaceholders().then((r) => (info.value = r)).catch(() => (info.value = null))
  api.users().then((r) => (users.value = r.items)).catch(() => (users.value = []))
  api
    .settings()
    .then((s) => (baseURL.value = s.subscription_base_url || s.config_base_url))
    .catch(() => (baseURL.value = ''))
}

onMounted(load)

function create() {
  editing.value = null
  modalOpen.value = true
}

async function edit(p: SubscriptionProfile) {
  try {
    // 列表里没有正文(十份模板就是几百 KB 跟着每次刷新走),编辑时才拉。
    editing.value = await api.subscriptionProfile(p.id)
    modalOpen.value = true
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '读取配置内容失败')
  }
}

async function toggle(p: SubscriptionProfile) {
  const next = !p.enabled
  const act = () =>
    api
      .setSubscriptionProfileEnabled(p.id, next)
      .then(() => {
        message.success(next ? '已启用' : '已停用')
        load()
      })
      .catch((err) => message.error(err instanceof ApiError ? err.message : '操作失败'))

  if (!next) {
    // 可逆但影响面大:全部用户手里的这条链接立刻 404。
    lbDangerConfirm({
      title: `停用「${p.name}」?`,
      okText: '停用',
      okType: 'primary',
      impacts: [
        '全部用户的订阅页上不再出现这一份',
        '已经复制出去的链接立即失效(返回 404)',
        '已经导入到客户端里的配置不受影响,直到他下次更新',
      ],
      footer: '内容留在库里,随时可以重新启用。',
      onOk: () => {
        void act()
      },
    })
    return
  }
  void act()
}

function remove(p: SubscriptionProfile) {
  lbDangerConfirm({
    title: `删除「${p.name}」?`,
    okText: '删除',
    impacts: [
      '全部用户手里的这条链接立即失效',
      '配置正文一并删除,面板里没有恢复入口',
      `这个编号(${p.id})不会被新配置复用,旧链接不会指向别的东西`,
    ],
    footer: '想临时下架用「停用」——那个可以随时恢复。删除前先把正文复制出来。',
    onOk: () => {
      void api
        .deleteSubscriptionProfile(p.id)
        .then(() => {
          message.success('已删除')
          load()
        })
        .catch((err) => message.error(err instanceof ApiError ? err.message : '删除失败'))
    },
  })
}

const columns = [
  { title: '类型', key: 'kind', width: 130 },
  { title: '名称', key: 'name' },
  { title: '文件名', key: 'filename', width: 170 },
  { title: '大小', key: 'size', width: 90 },
  { title: '状态', key: 'enabled', width: 90 },
  { title: '排序', key: 'sort', width: 70 },
  { title: '更新时间', key: 'updated', width: 160 },
  { title: '操作', key: 'actions', width: 180 },
]

function kb(bytes: number) {
  return `${(bytes / 1024).toFixed(1)} KB`
}
</script>

<template>
  <div class="sp">
    <div class="sp__head">
      <div>
        <h2 class="sp__title">订阅配置</h2>
        <div class="sp__sub">
          整份客户端配置,面板按用户替换里面的占位符。系统不预置任何模板 ——
          没配的类型,用户的订阅页上不会出现。
        </div>
      </div>
      <a-button type="primary" @click="create">新增配置</a-button>
    </div>

    <!-- 三种类型的机制完全不同,这一段决定管理员会不会配错。 -->
    <section class="sp__kinds">
      <div v-for="(label, k) in profileKindLabel" :key="k" class="sp__kind">
        <div class="sp__kind-head">
          <span class="sp__kind-name">{{ label }}</span>
          <span v-if="configured.has(k as ProfileKind)" class="sp__kind-on">已配置</span>
          <span v-else class="sp__kind-off">未配置</span>
        </div>
        <div class="sp__kind-body">{{ profileKindHint[k as ProfileKind] }}</div>
      </div>
    </section>

    <LbEmptyState v-if="loadError" variant="error" :title="loadError" @retry="load" />

    <div v-else-if="loading" class="sp__card">
      <a-skeleton active :paragraph="{ rows: 4 }" />
    </div>

    <div v-else-if="items.length === 0" class="sp__card">
      <LbEmptyState
        variant="empty"
        title="还没有任何配置文件"
        description="用户的订阅页上现在只有节点订阅那三条地址。把你调好的 Clash / sing-box / 小火箭配置放进来,里面跟节点和订阅有关的地方换成占位符,面板会按人替换。"
      />
    </div>

    <!-- 窄屏整表换卡片:AntD Table 的横向滚动会把「操作」列推到屏幕外。 -->
    <div v-else-if="narrow" class="sp__cards">
      <LbRowCard v-for="p in items" :key="p.id">
        <template #head>
          <span class="sp__cell-name">{{ p.name }}</span>
          <LbStatusTag :meta="p.enabled ? enabledMeta : disabledMeta" />
        </template>
        <div class="sp__cell-meta">
          {{ profileKindLabel[p.kind] }} · <span class="lb-mono">{{ p.filename }}</span> ·
          {{ kb(p.content_bytes) }} · 排序 {{ p.sort_order }}
        </div>
        <div v-if="p.description" class="sp__cell-desc">{{ p.description }}</div>
        <div class="sp__cell-meta">
          更新 <LbTimeText :value="p.updated_at" />
        </div>
        <template #foot>
          <a-button size="small" @click="edit(p)">编辑</a-button>
          <a-button size="small" @click="toggle(p)">{{ p.enabled ? '停用' : '启用' }}</a-button>
          <a-button size="small" danger @click="remove(p)">删除</a-button>
        </template>
      </LbRowCard>
    </div>

    <a-table
      v-else
      :columns="columns"
      :data-source="items"
      :pagination="false"
      row-key="id"
      size="middle"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'kind'">
          {{ profileKindLabel[(record as SubscriptionProfile).kind] }}
        </template>
        <template v-else-if="column.key === 'name'">
          <div class="sp__cell-name">{{ (record as SubscriptionProfile).name }}</div>
          <div v-if="(record as SubscriptionProfile).display_name" class="sp__cell-meta">
            门户上显示为「{{ (record as SubscriptionProfile).display_name }}」
          </div>
          <div v-if="(record as SubscriptionProfile).remark" class="sp__cell-meta">
            {{ (record as SubscriptionProfile).remark }}
          </div>
        </template>
        <template v-else-if="column.key === 'filename'">
          <span class="lb-mono">{{ (record as SubscriptionProfile).filename }}</span>
        </template>
        <template v-else-if="column.key === 'size'">
          <span class="lb-mono">{{ kb((record as SubscriptionProfile).content_bytes) }}</span>
        </template>
        <template v-else-if="column.key === 'enabled'">
          <LbStatusTag
            :meta="(record as SubscriptionProfile).enabled ? enabledMeta : disabledMeta"
          />
        </template>
        <template v-else-if="column.key === 'sort'">
          <span class="lb-mono">{{ (record as SubscriptionProfile).sort_order }}</span>
        </template>
        <template v-else-if="column.key === 'updated'">
          <LbTimeText :value="(record as SubscriptionProfile).updated_at" />
        </template>
        <template v-else-if="column.key === 'actions'">
          <div class="sp__actions">
            <a-button size="small" type="link" @click="edit(record as SubscriptionProfile)">
              编辑
            </a-button>
            <a-button size="small" type="link" @click="toggle(record as SubscriptionProfile)">
              {{ (record as SubscriptionProfile).enabled ? '停用' : '启用' }}
            </a-button>
            <a-button size="small" type="link" danger @click="remove(record as SubscriptionProfile)">
              删除
            </a-button>
          </div>
        </template>
      </template>
    </a-table>

    <SubscriptionProfileModal
      v-model:open="modalOpen"
      :profile="editing"
      :info="info"
      :users="users"
      :base-url="baseURL"
      @saved="load"
    />
  </div>
</template>

<style scoped>
.sp {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.sp__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.sp__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.sp__sub {
  margin-top: 3px;
  max-width: 720px;
  font-size: 12.5px;
  line-height: 1.75;
  color: #6b7480;
}

.sp__kinds {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
}

.sp__kind {
  padding: 12px 14px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.sp__kind-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 5px;
}

.sp__kind-name {
  font-size: 13px;
  font-weight: 600;
}

.sp__kind-on {
  padding: 1px 6px;
  background: #e9f5ee;
  border: 1px solid #c3e3d0;
  border-radius: 3px;
  font-size: 10.5px;
  color: #1b7a4b;
}

.sp__kind-off {
  padding: 1px 6px;
  background: #f1f3f5;
  border: 1px solid #dfe3e8;
  border-radius: 3px;
  font-size: 10.5px;
  color: #5c6672;
}

.sp__kind-body {
  font-size: 11.5px;
  line-height: 1.75;
  color: #6b7480;
}

.sp__card {
  padding: 16px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.sp__cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.sp__cell-name {
  font-size: 13px;
  font-weight: 500;
}

.sp__cell-meta {
  font-size: 11.5px;
  color: #6b7480;
}

.sp__cell-desc {
  font-size: 11.5px;
  line-height: 1.7;
  color: #576070;
}

.sp__actions {
  display: flex;
  gap: 2px;
}

@media (max-width: 1023px) {
  .sp__kinds {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
