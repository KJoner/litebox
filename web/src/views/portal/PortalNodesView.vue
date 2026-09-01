<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  portalApi,
  ApiError,
  type PortalExternalNode,
  type PortalRelayNode,
  type PortalNode,
} from '@/api/client'
import { formatBytes } from '@/utils/format'
import { LbEmptyState, LbStatusTag, LbTimeText, portalNodeStatusMeta } from '@/components/lb'

/**
 * 我的节点。用户要判断的是「现在该连哪个」。
 *
 * 门户不显示节点的 IP、SSH、配置版本、sing-box 版本、CPU 内存 ——
 * 用户对这些既无权限也无用处,露出来只会引来
 * 「你的机器内存 96% 了是不是要炸」这类问题。
 * 端口和协议留着,客户端排障时用得上。
 */
const nodes = ref<PortalNode[]>([])
/**
 * 外部代理。它们能连、能进订阅,但**面板统计不到流量** ——
 * 流量走的是上游的服务器。所以这一段不显示任何流量数字:
 * 给一个 0 会被读成「我一点都没用过」,那比不显示更糟。
 */
const external = ref<PortalExternalNode[]>([])
const relays = ref<PortalRelayNode[]>([])
const loading = ref(true)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const r = await portalApi.nodes()
    nodes.value = r.items
    external.value = r.external ?? []
    relays.value = r.relays ?? []
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '暂时读不到节点信息'
    nodes.value = []
    external.value = []
    relays.value = []
  } finally {
    loading.value = false
  }
}

onMounted(load)

// 只展示【当前在订阅里、能用】的:自建节点取 status 正常(维护/停用的机器
// 本来就不在订阅里),中转与外部线路取 in_subscription。不可用的整条不显示 ——
// 用户对一条连不上、也不在自己订阅里的线路做不了任何事,列出来只会引出
// 「为什么我连不上」。
const visibleNodes = computed(() => nodes.value.filter((n) => n.status === 'normal'))
const visibleRelays = computed(() => relays.value.filter((r) => r.in_subscription))
const visibleExternal = computed(() => external.value.filter((x) => x.in_subscription))
const hasNone = computed(
  () =>
    visibleNodes.value.length === 0 &&
    visibleRelays.value.length === 0 &&
    visibleExternal.value.length === 0,
)

const summary = computed(() => {
  const parts = [`${visibleNodes.value.length} 个可用节点`]
  if (visibleRelays.value.length) parts.push(`${visibleRelays.value.length} 条中转`)
  if (visibleExternal.value.length) parts.push(`${visibleExternal.value.length} 条其他线路`)
  return parts.join(' · ')
})
</script>

<template>
  <div class="pn">
    <div class="pn__head">
      <div>
        <h2 class="pn__title">我的节点</h2>
        <div class="pn__sub">{{ summary }} · 流量数字按 UTC 日统计</div>
      </div>
      <a-button size="small" :loading="loading" @click="load">刷新</a-button>
    </div>

    <LbEmptyState v-if="loadError" variant="error" :title="loadError" @retry="load" />

    <div v-else-if="loading" class="pn__skel">
      <a-skeleton active :paragraph="{ rows: 2 }" />
      <a-skeleton active :paragraph="{ rows: 2 }" />
    </div>

    <div v-else-if="hasNone" class="pn__card">
      <LbEmptyState
        variant="empty"
        title="还没有可用节点"
        description="你的访问等级下暂时没有分配节点,或者全部节点都在维护中。订阅地址仍然有效,节点恢复后无需重新导入。请联系管理员。"
      />
    </div>

    <!--
      列表展示(不再用卡片)。只列当前能用、在订阅里的节点 ——
      维护中与已停用的机器本来就不在订阅里,这里不显示。
    -->
    <div v-else-if="visibleNodes.length" class="pn__list">
      <div v-for="n in visibleNodes" :key="n.id" class="pn__row">
        <div class="pn__row-main">
          <div class="pn__row-head">
            <span class="pn__row-name">{{ n.display_name }}</span>
            <LbStatusTag :meta="portalNodeStatusMeta[n.status]" />
            <span class="pn__row-tier">{{ n.tier_name }}</span>
            <!-- 地址本身不下发给用户,只说「你的订阅里这个节点有两条」。 -->
            <span v-if="n.supports_ipv6" class="pn__row-tag">IPv6</span>
            <span v-if="n.unmetered" class="pn__row-tag">不计流量</span>
          </div>
          <div class="pn__row-meta lb-mono">
            {{ n.protocol.toUpperCase() }} · 端口 {{ n.public_port }} · 更新
            <LbTimeText :value="n.last_seen_at" empty="暂无数据" />
          </div>
          <div v-if="n.supports_ipv6" class="pn__row-hint">
            你的订阅里这个节点有两条:IPv4 和 IPv6,指向同一台机器,任选其一。
          </div>
          <div v-if="n.unmetered" class="pn__row-hint">经这个节点的流量不计入你的额度。</div>
          <div v-if="n.public_remark" class="pn__row-remark">{{ n.public_remark }}</div>
        </div>
        <div class="pn__row-stats">
          <div><span>今日</span><b class="lb-mono">{{ formatBytes(n.today_bytes) }}</b></div>
          <div><span>本月</span><b class="lb-mono">{{ formatBytes(n.month_bytes) }}</b></div>
          <div><span>累计</span><b class="lb-mono">{{ formatBytes(n.total_bytes) }}</b></div>
        </div>
      </div>
    </div>

    <!-- 中转线路单独一段。
         与外部代理分成两段而不是合并:那些是买来的成品,这些的凭据是我们发的,
         而两者对用户唯一的共同点只是「没有流量数字」。
         这里不出现落地是谁 —— 那是内部拓扑。 -->
    <template v-if="!loading && !loadError && visibleRelays.length">
      <div class="pn__section">
        <span class="pn__section-title">中转线路</span>
        <span class="pn__section-note">
          这些线路经过一台中转主机再到落地,因此没有单独的流量统计。用法与上面的节点完全一样。
        </span>
      </div>
      <div class="pn__list">
        <div v-for="x in visibleRelays" :key="x.id" class="pn__row">
          <div class="pn__row-main">
            <div class="pn__row-head">
              <span class="pn__row-name">{{ x.display_name }}</span>
              <LbStatusTag :meta="portalNodeStatusMeta[x.status]" />
              <span class="pn__row-tier">{{ x.tier_name }}</span>
            </div>
            <div v-if="x.public_remark" class="pn__row-remark">{{ x.public_remark }}</div>
          </div>
        </div>
      </div>
    </template>

    <!-- 外部代理单独一段。
         不混进上面的列表:那些行有流量数字,而这些没有。 -->
    <template v-if="!loading && !loadError && visibleExternal.length">
      <div class="pn__section">
        <span class="pn__section-title">其他线路</span>
        <span class="pn__section-note">
          这些线路不由本面板托管,因此没有流量统计。用法与上面的节点完全一样。
        </span>
      </div>
      <div class="pn__list">
        <div v-for="x in visibleExternal" :key="x.id" class="pn__row">
          <div class="pn__row-main">
            <div class="pn__row-head">
              <span class="pn__row-name">{{ x.display_name }}</span>
              <LbStatusTag :meta="portalNodeStatusMeta[x.status]" />
              <span class="pn__row-tier">{{ x.tier_name }}</span>
            </div>
            <div v-if="x.public_remark" class="pn__row-remark">{{ x.public_remark }}</div>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.pn__section {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 4px;
}
.pn__section-title {
  font-size: 14px;
  font-weight: 600;
}
.pn__section-note {
  font-size: 12px;
  color: #6b7480;
}

.pn {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.pn__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.pn__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.pn__sub {
  margin-top: 3px;
  font-size: 12.5px;
  color: #6b7480;
}

.pn__skel {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 16px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.pn__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

/* 列表:一台机器一行。桌面上左边是信息、右边是流量三格,窄屏时纵向堆叠。 */
.pn__list {
  display: flex;
  flex-direction: column;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  overflow: hidden;
}

.pn__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 12px 16px;
  border-top: 1px solid #edeff2;
}
.pn__row:first-child {
  border-top: none;
}

.pn__row-main {
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 0;
}

.pn__row-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.pn__row-name {
  font-size: 14px;
  font-weight: 600;
}

.pn__row-tier {
  padding: 1px 6px;
  background: #f1f3f5;
  border: 1px solid #dfe3e8;
  border-radius: 3px;
  font-size: 11px;
  color: #5c6672;
}

.pn__row-tag {
  padding: 1px 6px;
  background: #eef4fc;
  border: 1px solid #c9dcf3;
  border-radius: 3px;
  font-size: 11px;
  font-weight: 500;
  color: #1d4f96;
}

.pn__row-meta {
  font-size: 11.5px;
  color: #6b7480;
}

.pn__row-hint,
.pn__row-remark {
  padding: 7px 10px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #576070;
}

.pn__row-stats {
  display: flex;
  gap: 18px;
  flex-shrink: 0;
}

.pn__row-stats > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
  text-align: right;
}

.pn__row-stats span {
  font-size: 11px;
  color: #6b7480;
}

.pn__row-stats b {
  font-size: 13px;
  font-weight: 500;
}

@media (max-width: 767px) {
  .pn__row {
    flex-direction: column;
    align-items: stretch;
    gap: 10px;
  }
  .pn__row-stats {
    gap: 14px;
    justify-content: space-between;
  }
  .pn__row-stats > div {
    text-align: left;
  }
}
</style>
