<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { portalApi, ApiError, type PortalNode } from '@/api/client'
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
const loading = ref(true)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    nodes.value = (await portalApi.nodes()).items
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '暂时读不到节点信息'
    nodes.value = []
  } finally {
    loading.value = false
  }
}

onMounted(load)

const summary = computed(() => {
  const ok = nodes.value.filter((n) => n.status === 'normal').length
  const maint = nodes.value.filter((n) => n.status === 'maintenance').length
  const off = nodes.value.filter((n) => n.status === 'disabled').length
  const parts = [`${ok} 个可用`]
  if (maint) parts.push(`${maint} 个维护中`)
  if (off) parts.push(`${off} 个已停用`)
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

    <div v-else-if="nodes.length === 0" class="pn__card">
      <LbEmptyState
        variant="empty"
        title="还没有可用节点"
        description="你的访问等级下暂时没有分配节点,或者全部节点都在维护中。订阅地址仍然有效,节点恢复后无需重新导入。请联系管理员。"
      />
    </div>

    <!-- 维护中的卡片不折叠、不排到最后 —— 用户正是因为「某个节点连不上」才打开这一页。 -->
    <div v-else class="pn__grid">
      <section v-for="n in nodes" :key="n.id" class="pn__node">
        <div class="pn__node-head">
          <span class="pn__node-name">{{ n.display_name }}</span>
          <LbStatusTag :meta="portalNodeStatusMeta[n.status]" />
          <span class="pn__node-tier">{{ n.tier_name }}</span>
          <!-- 地址本身不下发给用户,只说「你的订阅里这个节点有两条」。 -->
          <span v-if="n.supports_ipv6" class="pn__node-v6">IPv6</span>
        </div>

        <div class="pn__node-meta lb-mono">
          {{ n.protocol.toUpperCase() }} · 端口 {{ n.public_port }}
        </div>

        <div v-if="n.supports_ipv6" class="pn__node-hint">
          你的订阅里这个节点有两条:IPv4 和 IPv6,指向同一台机器,任选其一。
        </div>

        <!-- 维护是人为暂停(紫),公开备注是中性信息(灰)。两者语义不同,不能同色。 -->
        <div v-if="n.status === 'maintenance'" class="pn__node-maint">
          <div v-if="n.maintenance_message" class="pn__node-maint-msg">
            {{ n.maintenance_message }}
          </div>
          <div>该节点暂未下发到订阅,恢复后会自动出现,不需要你做任何操作。</div>
        </div>
        <div v-else-if="n.status === 'disabled'" class="pn__node-off">
          管理员已停用该节点。它不在你的订阅里,也不会自动恢复。
          与「维护中」的差别:维护是临时的、会自己回来;停用是长期的、需要管理员操作。
        </div>
        <div v-else-if="n.public_remark" class="pn__node-remark">{{ n.public_remark }}</div>

        <div class="pn__node-stats">
          <div><span>今日</span><b class="lb-mono">{{ formatBytes(n.today_bytes) }}</b></div>
          <div><span>本月</span><b class="lb-mono">{{ formatBytes(n.month_bytes) }}</b></div>
          <div><span>累计</span><b class="lb-mono">{{ formatBytes(n.total_bytes) }}</b></div>
        </div>

        <div class="pn__node-foot">
          最近更新 <LbTimeText :value="n.last_seen_at" empty="暂无数据" />
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
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

/* 桌面两列、窄屏单列。 */
.pn__grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px;
}

.pn__node {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 16px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.pn__node-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.pn__node-name {
  font-size: 14px;
  font-weight: 600;
}

.pn__node-tier {
  padding: 1px 6px;
  background: #f1f3f5;
  border: 1px solid #dfe3e8;
  border-radius: 3px;
  font-size: 11px;
  color: #5c6672;
}

.pn__node-v6 {
  padding: 1px 6px;
  background: #eef4fc;
  border: 1px solid #c9dcf3;
  border-radius: 3px;
  font-size: 11px;
  font-weight: 500;
  color: #1d4f96;
}

.pn__node-meta {
  font-size: 11.5px;
  color: #6b7480;
}

.pn__node-hint,
.pn__node-remark {
  padding: 9px 11px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #576070;
}

.pn__node-maint-msg {
  margin-bottom: 4px;
  font-weight: 500;
}

.pn__node-maint {
  padding: 9px 11px;
  background: #f0eef9;
  border: 1px solid #d6d0ee;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #443a7a;
}

.pn__node-off {
  padding: 9px 11px;
  background: #f1f3f5;
  border: 1px solid #dfe3e8;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #5c6672;
}

.pn__node-stats {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  padding-top: 4px;
}

.pn__node-stats > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.pn__node-stats span {
  font-size: 11px;
  color: #6b7480;
}

.pn__node-stats b {
  font-size: 13px;
  font-weight: 500;
}

.pn__node-foot {
  padding-top: 8px;
  border-top: 1px solid #edeff2;
  font-size: 11px;
  color: #6b7480;
}

@media (max-width: 767px) {
  .pn__grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
