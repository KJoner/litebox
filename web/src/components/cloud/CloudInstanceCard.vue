<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type CloudNodeView,
  type CloudPowerEvent,
  type Node,
} from '@/api/client'
import {
  LbQuotaBar,
  LbSparkline,
  LbStatusTag,
  LbTimeText,
  lbDangerConfirm,
  type LbPoint,
  type LbStatusMeta,
} from '@/components/lb'
import { formatBytes } from '@/utils/format'
import { color } from '@/theme/tokens'
import { cloudStatusMeta, cloudUsageLevel } from './cloudMeta'

/**
 * 节点详情里的「云实例」卡片(V17):实例状态、所在池子的用量、手动开关机、
 * 最近的开关机记录,以及本月累计用量的小曲线。
 *
 * 只在 node.cloud 非空时由父页面渲染。用量是**账号级**的,与同账号下别的实例共用,
 * 卡片上要把这句话写出来。
 */
const props = defineProps<{ node: Node }>()
const emit = defineEmits<{ (e: 'changed'): void }>()

const cloud = computed(() => props.node.cloud as CloudNodeView)
const meta = computed<LbStatusMeta>(() => cloudStatusMeta(cloud.value))
const busy = ref('')
const events = ref<CloudPowerEvent[]>([])
const points = ref<LbPoint[]>([])
const extrasError = ref('')

const eventMeta: Record<CloudPowerEvent['status'], LbStatusMeta> = {
  SENT: { text: '已发送', shape: 'check', fg: color.success, bg: color.successBg, bd: color.successBorder },
  FAILED: { text: '失败', shape: 'cross', fg: color.danger, bg: color.dangerBg, bd: color.dangerBorder },
  SKIPPED: { text: '已跳过', shape: 'minus', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder },
}

async function loadExtras() {
  const c = cloud.value
  if (!c) return
  extrasError.value = ''
  try {
    const [ev, sm] = await Promise.all([
      api.nodeCloudEvents(props.node.id, 10),
      api.cloudSamples(c.account_id, c.traffic_class, 31),
    ])
    events.value = ev.items
    points.value = sm.items.map((s) => ({
      at: new Date(s.bucket_ts * 1000).toISOString(),
      value: s.bytes,
    }))
  } catch (err) {
    extrasError.value = err instanceof ApiError ? err.message : '读不到开关机记录'
  }
}

watch(
  () => [props.node.id, cloud.value?.account_id, cloud.value?.instance_status, cloud.value?.sampled_at],
  () => void loadExtras(),
  { immediate: true },
)

function hourLabel(at: string): string {
  const d = new Date(at)
  return `${d.getUTCMonth() + 1} 月 ${d.getUTCDate()} 日 ${String(d.getUTCHours()).padStart(2, '0')}:00 UTC`
}

async function run(what: string, fn: () => Promise<unknown>, done: string) {
  busy.value = what
  try {
    await fn()
    message.success(done)
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${what}失败`)
  } finally {
    busy.value = ''
  }
}

function refresh() {
  void run('刷新', () => api.refreshNodeCloud(props.node.id), '已重新查询实例状态')
}

function start() {
  lbDangerConfirm({
    title: `开机:${props.node.display_name || props.node.name}`,
    okType: 'primary',
    okText: '开机',
    impacts: [
      `向阿里云发送 StartInstance,实例 **${cloud.value.instance_id}** 会在几秒到一分钟内进入运行中。`,
      cloud.value.stopped_by
        ? `这台实例是「${cloud.value.stopped_by_label}」停下来的。手动开机之后,本月内面板不会再因为同一次超阈值停它。`
        : '实例不是面板停的,开机后保活 / 定时按各自的规则继续。',
      cloud.value.stopped_mode === 'StopCharging' && !cloud.value.has_eip
        ? '**这台实例没有 EIP**:节省停机后开机,系统分配的公网 IP 可能会变。面板会在实例运行后比对一次并提醒。'
        : '实例有 EIP(或用普通停机),开机后地址不变。',
      cloud.value.spot
        ? '抢占式实例:节省停机后开机等于重新申请资源,库存不足时会失败(NoStock)。'
        : '开机后这台机器上的 sing-box / mita 由服务巡检负责拉起或重新下发。',
    ],
    onOk: () => run('开机', () => api.startNodeCloud(props.node.id), '已发送开机命令'),
  })
}

function stop() {
  lbDangerConfirm({
    title: `停机:${props.node.display_name || props.node.name}`,
    okText: '停机',
    impacts: [
      '**这台机器上全部用户立刻断线**,直到下次开机;订阅不变,用户客户端里这条节点会显示连不上。',
      `停机模式:${cloud.value.stopped_mode_label}(${cloud.value.stopped_mode === 'StopCharging' ? '不计算力费用,磁盘与 EIP 照常计费' : '实例继续计费'})。`,
      cloud.value.stopped_mode === 'StopCharging' && !cloud.value.has_eip
        ? '**这台实例没有 EIP**:节省停机会释放系统分配的公网 IP,开机后地址可能变,那时订阅里的地址就连不上了。'
        : '实例有 EIP(或用普通停机),开机后地址不变。',
      '停之前面板会先尽力同步一次这台机器的代理流量;同步失败不阻止停机,最后一段流量可能没入账。',
      '手动停的机器保活不会去开它;定时开机与再次手动开机可以。',
    ],
    onOk: () => run('停机', () => api.stopNodeCloud(props.node.id), '已发送停机命令'),
  })
}
</script>

<template>
  <section class="cc">
    <div class="cc__head">
      <span>
        云实例
        <span class="cc__note">阿里云 · {{ cloud.account_name }} · {{ cloud.class_label }}</span>
      </span>
      <LbStatusTag :meta="meta" size="md" />
    </div>
    <div class="cc__body">
      <div v-if="cloud.ip_mismatch" class="cc__alert cc__alert--danger">
        实例现在的公网地址是 <b class="lb-mono">{{ cloud.public_ip }}</b>,而节点的管理地址是
        <b class="lb-mono">{{ node.host }}</b> —— 订阅里下发的是后者,用户连不上。到「编辑节点」里把管理地址改过来,
        或者给实例绑一个 EIP / 用域名当管理地址。
      </div>
      <div v-if="cloud.stopped_by" class="cc__alert cc__alert--warn">
        这台实例是<strong>{{ cloud.stopped_by_label }}</strong>
        <template v-if="cloud.stopped_at">(<LbTimeText :value="cloud.stopped_at" />)</template>。
        面板在它停着期间不巡检、不采集、不同步流量。
      </div>
      <div v-else-if="cloud.instance_status === 'Stopped'" class="cc__alert cc__alert--warn">
        实例处于停止状态,但不是面板停的 —— 可能是在阿里云控制台手动停的,或者抢占式实例被回收了。
        <template v-if="cloud.keepalive">保活已开,面板会按退避重试开机。</template>
        <template v-else>保活没开,面板不会自动开它。</template>
      </div>
      <div v-if="cloud.last_error" class="cc__alert cc__alert--danger">上次操作出错:{{ cloud.last_error }}</div>

      <div class="cc__kv">
        <div><span>实例 ID</span><b class="lb-mono">{{ cloud.instance_id }}</b></div>
        <div><span>区域</span><b class="lb-mono">{{ cloud.region_id }}</b></div>
        <div>
          <span>对外地址</span>
          <b class="lb-mono">{{ cloud.public_ip || '—' }}<template v-if="cloud.has_eip"> (EIP)</template></b>
        </div>
        <div>
          <span>计费</span>
          <b>{{ cloud.charge_type === 'PrePaid' ? '包年包月' : cloud.charge_type === 'PostPaid' ? '按量付费' : '—' }}<template v-if="cloud.spot"> · 抢占式</template></b>
        </div>
        <div><span>停机模式</span><b>{{ cloud.stopped_mode_label }}</b></div>
        <div><span>超阈值</span><b>{{ cloud.threshold_action === 'STOP' ? '自动停机' : '仅通知' }}</b></div>
        <div>
          <span>定时</span>
          <b v-if="cloud.schedule_enabled">
            {{ cloud.start_time ? `开 ${cloud.start_time}` : '' }}{{ cloud.start_time && cloud.stop_time ? ' · ' : '' }}{{ cloud.stop_time ? `关 ${cloud.stop_time}` : '' }}
          </b>
          <b v-else>未开</b>
        </div>
        <div>
          <span>保活</span>
          <b>
            {{ cloud.keepalive ? '开' : '关' }}
            <template v-if="cloud.keepalive && cloud.keepalive_failures > 0">
              · 连续失败 {{ cloud.keepalive_failures }} 次
            </template>
          </b>
        </div>
        <div class="cc__kv-wide">
          <span>状态查询于</span>
          <b><LbTimeText :value="cloud.status_at" /></b>
        </div>
      </div>

      <div>
        <div class="cc__pool-title">
          本月 CDT 用量 · {{ cloud.class_label }}(账号级,与同账号下别的实例共用)
          <span v-if="cloud.over" class="cc__chip cc__chip--bad">已达阈值</span>
        </div>
        <LbQuotaBar
          :used-bytes="cloud.sampled ? cloud.used_bytes : null"
          :quota-bytes="cloud.quota_bytes"
          :warning-level="cloudUsageLevel(cloud)"
          size="md"
        />
        <div class="cc__note">
          <template v-if="cloud.sampled">采样 <LbTimeText :value="cloud.sampled_at" /> · CDT 的数据有延迟,不是实时值</template>
          <template v-else>还没采到用量</template>
          <template v-if="cloud.query_error"> · 上次查询失败:{{ cloud.query_error }}</template>
        </div>
        <LbSparkline
          v-if="points.length >= 2"
          :points="points"
          type="line"
          :height="56"
          :format="formatBytes"
          :label-format="hourLabel"
        />
      </div>

      <div class="cc__actions">
        <a-button size="small" :loading="busy === '刷新'" :disabled="!!busy" @click="refresh">刷新状态</a-button>
        <a-button
          size="small"
          type="primary"
          :loading="busy === '开机'"
          :disabled="!!busy || cloud.instance_status === 'Running' || cloud.instance_status === 'Starting'"
          @click="start"
        >
          开机
        </a-button>
        <a-button
          size="small"
          danger
          :loading="busy === '停机'"
          :disabled="!!busy || cloud.instance_status === 'Stopped' || cloud.instance_status === 'Stopping'"
          @click="stop"
        >
          停机
        </a-button>
      </div>

      <div v-if="extrasError" class="cc__note">{{ extrasError }}</div>
      <div v-else-if="events.length" class="cc__events">
        <div class="cc__pool-title">最近的开关机记录</div>
        <div v-for="ev in events" :key="ev.id" class="cc__event">
          <LbStatusTag :meta="eventMeta[ev.status]" />
          <span class="cc__event-kind">{{ ev.kind_label }}</span>
          <span class="cc__event-detail" :title="ev.detail">{{ ev.detail }}</span>
          <LbTimeText :value="ev.created_at" />
        </div>
      </div>
    </div>
    <div class="cc__foot">
      云端的动作只管实例(开机 / 关机);实例运行后,这台机器上 sing-box / mita 的恢复由服务巡检负责。
      超阈值、定时与保活的规则在「编辑节点」里改。
    </div>
  </section>
</template>

<style scoped>
.cc {
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  background: #ffffff;
}
.cc__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 11px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}
.cc__note {
  font-size: 11px;
  font-weight: 400;
  color: #6b7480;
}
.cc__body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
}
.cc__foot {
  padding: 10px 16px;
  border-top: 1px solid #edeff2;
  font-size: 11px;
  line-height: 1.7;
  color: #6b7480;
}
.cc__alert {
  border-radius: 6px;
  padding: 8px 11px;
  font-size: 12px;
  line-height: 1.6;
}
.cc__alert--warn {
  background: #fcf3e3;
  border: 1px solid #efdcb4;
}
.cc__alert--danger {
  background: #fdecea;
  border: 1px solid #f3cfc9;
}
.cc__kv {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
  gap: 8px 16px;
  font-size: 12px;
}
.cc__kv > div {
  display: flex;
  justify-content: space-between;
  gap: 8px;
}
.cc__kv-wide {
  grid-column: 1 / -1;
}
.cc__kv span {
  color: #6b7480;
}
.cc__pool-title {
  font-size: 12px;
  margin-bottom: 6px;
  display: flex;
  gap: 6px;
  align-items: center;
  flex-wrap: wrap;
}
.cc__chip {
  font-size: 11px;
  padding: 0 6px;
  border-radius: 10px;
}
.cc__chip--bad {
  border: 1px solid #f3cfc9;
  background: #fdecea;
  color: #b4291d;
}
.cc__actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.cc__events {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.cc__event {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
}
.cc__event-kind {
  white-space: nowrap;
}
.cc__event-detail {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: #6b7480;
}
</style>
