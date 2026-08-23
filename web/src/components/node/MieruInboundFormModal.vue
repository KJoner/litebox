<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type MieruInbound,
  type MieruMultiplexing,
  type MieruTransport,
  type Node,
} from '@/api/client'
import { lbDangerConfirm } from '@/components/lb/lbDangerConfirm'

/**
 * 新增 / 编辑一个 Mieru 入口。
 *
 * 与 InboundFormModal 分成两个组件而不是加一个协议分支:两者的字段几乎
 * 不重叠(那边是 REALITY 握手目标、加密方法、TFO;这边是端口段、传输层、
 * 多路复用),合成一个表单会让一半的输入框在任何时刻都是灰的,
 * 而管理员分不出哪些是"这个协议没有"、哪些是"还没填"。
 *
 * **端口在这里是一段而不是一个数** —— 多端口跳跃是 mieru 的主要抗封锁特性。
 */
const props = defineProps<{
  open: boolean
  /** null 表示新增 */
  inbound: MieruInbound | null
  /** 入口所属的机器。新增时必填 —— 它决定往哪台机器上加。 */
  node: Node
  tiers: AccessTier[]
  /** 这台机器已有的入口数,新增时用来给排序一个不重复的默认值。 */
  existingCount: number
}>()

const emit = defineEmits<{
  'update:open': [boolean]
  saved: []
  busy: [label: string]
}>()

const running = ref('')

const MULTIPLEXING_LABEL: Record<MieruMultiplexing, string> = {
  MULTIPLEXING_OFF: '关闭',
  MULTIPLEXING_LOW: '低(默认)',
  MULTIPLEXING_MIDDLE: '中',
  MULTIPLEXING_HIGH: '高',
}

function blank() {
  return {
    display_name: '',
    listen_port_start: 0,
    listen_port_end: 0,
    public_port_start: 0,
    public_port_end: 0,
    ipv6_public_port_start: 0,
    ipv6_public_port_end: 0,
    ipv6_enabled: true,
    ipv6_display_name: '',
    transport: 'TCP' as MieruTransport,
    multiplexing: 'MULTIPLEXING_LOW' as MieruMultiplexing,
    mtu: 0,
    access_tier_id: props.tiers[0]?.id ?? 1,
    sort_order: props.existingCount,
    subscription_enabled: true,
    enabled: true,
    public_remark: '',
  }
}

const form = ref(blank())

// 每次打开都按当前入口重置。留着上一次的值会让"新增"带上刚编辑过的那一段端口,
// 而端口冲突是保存时才报的 —— 管理员会以为是自己填错了。
watch(
  () => props.open,
  (open) => {
    if (!open) return
    const i = props.inbound
    form.value = i
      ? {
          display_name: i.display_name,
          listen_port_start: i.listen_port_start,
          listen_port_end: i.listen_port_end,
          public_port_start: i.public_port_start,
          public_port_end: i.public_port_end,
          ipv6_public_port_start: i.ipv6_public_port_start,
          ipv6_public_port_end: i.ipv6_public_port_end,
          ipv6_enabled: i.ipv6_enabled,
          // 回填【原始值】而不是 ipv6_entry_name:后者是回落之后的结果,
          // 把它填回输入框等于把「跟随」固化成一个具体名字,
          // 而管理员只是打开看了一眼、连改都没改。
          ipv6_display_name: i.ipv6_display_name,
          transport: i.transport,
          multiplexing: i.multiplexing,
          mtu: i.mtu,
          access_tier_id: i.access_tier_id,
          sort_order: i.sort_order,
          subscription_enabled: i.subscription_enabled,
          enabled: i.enabled,
          public_remark: i.public_remark,
        }
      : blank()
  },
  { immediate: true },
)

/** 监听段包含几个端口。0 表示还没填完。 */
const listenCount = computed(() => {
  const { listen_port_start: a, listen_port_end: b } = form.value
  return a > 0 && b >= a ? b - a + 1 : 0
})

const publicCount = computed(() => {
  const { public_port_start: a, public_port_end: b } = form.value
  return a > 0 && b >= a ? b - a + 1 : 0
})

/**
 * 订阅段与监听段的端口数不一样时提醒一句。
 *
 * **只提醒,不拦。** NAT 机上服务商映射的外部段与本机监听段本来就可以是
 * 两个不相干的号码段,面板没有办法知道映射是几对几 —— 拦下来会让那种机器
 * 一个入口都配不出来。但数量不等时客户端会在一部分端口上连不通,
 * 而那种失败看起来像是"这条线路时好时坏",所以要说出来。
 */
const portCountMismatch = computed(
  () => publicCount.value > 0 && listenCount.value > 0 && publicCount.value !== listenCount.value,
)

/**
 * 这次保存会不会改掉 IPv6 条目在用户客户端里的名字。
 *
 * 判断只比较原始值,**不在这里拼 -IPV6 后缀** —— 回落规则只有
 * subscription.IPv6EntryName 一处实现,前端拼一遍就是第二处。
 */
const ipv6NameWillChange = computed(() => {
  const cur = props.inbound
  if (!cur || !props.node.ipv6_address) return false
  if (!cur.ipv6_enabled || !form.value.ipv6_enabled) return false
  const before = cur.ipv6_display_name.trim()
  const after = form.value.ipv6_display_name.trim()
  if (before !== after) return true
  return after === '' && form.value.display_name.trim() !== cur.display_name
})

async function submit() {
  if (!form.value.display_name.trim()) {
    message.warning('请填写入口名称')
    return
  }
  if (!form.value.listen_port_start || !form.value.listen_port_end) {
    message.warning('请填写监听端口段的起止端口')
    return
  }
  if (form.value.listen_port_end < form.value.listen_port_start) {
    // 倒过来的区间在 mita 那边是一个空集合:服务照常起来、一个端口都不听,
    // 而面板显示"已下发"。这种失败完全静默,所以在这里就拦住。
    message.warning('监听端口段的结束端口不能小于起始端口')
    return
  }
  if (ipv6NameWillChange.value) {
    lbDangerConfirm({
      title: '确认修改 IPv6 条目的名称?',
      okType: 'primary',
      okText: '确认保存',
      impacts: [
        '已经导入过订阅的用户,客户端里会多出一份新的 IPv6 节点。',
        '旧的那一份不会自己消失,要用户手工删掉 —— 把名字改回去也删不掉它。',
        '两份都连得上同一台机器,所以不会有人因此断线。',
      ],
      footer: '还没导入订阅、或者之后会重新拉一次订阅的人不受影响。',
      onOk: () => doSave(),
    })
    return
  }
  await doSave()
}

async function doSave() {
  running.value = props.inbound ? '正在保存 Mieru 入口' : '正在新增 Mieru 入口'
  emit('busy', running.value)
  try {
    if (props.inbound) {
      await api.updateMieruInbound(props.inbound.id, { ...form.value })
    } else {
      await api.createMieruInbound(props.node.id, { ...form.value })
    }
    emit('update:open', false)
    emit('saved')
    // 说「下次下发后生效」而不是「已保存」:自动下发会重启 mita,
    // 把这台机器上全部 Mieru 连接一起踢掉,而他动的只是其中一个入口。
    message.success('已保存。Mieru 入口的变更要重新下发这台机器才会生效')
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    running.value = ''
    emit('busy', '')
  }
}
</script>

<template>
  <a-modal
    :open="open"
    :title="inbound ? `编辑 Mieru 入口「${inbound.display_name}」` : '新增 Mieru 入口'"
    :confirm-loading="!!running"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="submit"
  >
    <p class="mfm__where">
      所属机器 <b>{{ node.display_name || node.name }}</b>
      <span class="mfm__dim"> · {{ node.host }}</span>
    </p>

    <a-alert type="info" show-icon banner class="mfm__intro">
      <template #message>Mieru 跑在独立的 mita 服务上,不是 sing-box 入站</template>
      <template #description>
        它的凭据、下发方式与流量采集都与 sing-box 那几个入口互不相干,
        所以下发它不会影响同机的 sing-box 入口,反过来也一样。
        <b>sing-box 客户端不支持 Mieru</b> —— 用户要用 Clash / mihomo
        或者 mieru 自己的客户端。
      </template>
    </a-alert>

    <a-form layout="vertical" size="small">
      <a-form-item label="入口名称(会发给用户)">
        <a-input v-model:value="form.display_name" placeholder="例如:东京 Mieru" />
      </a-form-item>

      <a-form-item label="主机监听端口段(mita 真正 bind 的那一批)">
        <a-space>
          <a-input-number v-model:value="form.listen_port_start" :min="1" :max="65535" />
          <span>—</span>
          <a-input-number v-model:value="form.listen_port_end" :min="1" :max="65535" />
        </a-space>
        <div class="mfm__hint">
          <b>多端口跳跃是 Mieru 的主要抗封锁特性</b>:客户端会在这一段里换着端口连。
          只要一个端口的话,起止填同一个号码。
          <template v-if="listenCount"> 当前共 {{ listenCount }} 个端口。</template>
          <br />
          这一段不能与这台机器上任何 sing-box 入口、nginx 转发规则或别的 Mieru 入口重叠 ——
          撞上了保存时就会拒绝,不会等到下发失败。
        </div>
      </a-form-item>

      <a-form-item label="公网端口段(两端都留 0 表示与监听段相同)">
        <a-space>
          <a-input-number v-model:value="form.public_port_start" :min="0" :max="65535" />
          <span>—</span>
          <a-input-number v-model:value="form.public_port_end" :min="0" :max="65535" />
        </a-space>
        <div class="mfm__hint">
          只写进订阅,不进 mita 的配置。NAT 主机上服务商映射的外部段与本机监听段
          可以是两个完全不相干的号码段。
          <template v-if="publicCount"> 当前共 {{ publicCount }} 个端口。</template>
        </div>
        <div v-if="portCountMismatch" class="mfm__warn">
          公网段有 {{ publicCount }} 个端口,而监听段有 {{ listenCount }} 个 ——
          数量不一致时,客户端会在一部分端口上连不通,表现像是「这条线路时好时坏」。
          确认服务商的映射确实是这样再保存。
        </div>
      </a-form-item>

      <a-form-item label="传输层">
        <a-radio-group v-model:value="form.transport" size="small" button-style="solid">
          <a-radio-button value="TCP">TCP</a-radio-button>
          <a-radio-button value="UDP">UDP</a-radio-button>
        </a-radio-group>
        <div class="mfm__hint">改它要重新下发这台机器的 mita 配置。</div>
      </a-form-item>

      <a-form-item label="多路复用">
        <a-select
          v-model:value="form.multiplexing"
          :options="
            Object.entries(MULTIPLEXING_LABEL).map(([v, l]) => ({ value: v, label: l }))
          "
        />
        <div class="mfm__hint">
          档位越高越省握手,但也越容易在流量特征上聚成一团 —— 而那与这个协议
          存在的理由正好相反。拿不准就留默认。
        </div>
      </a-form-item>

      <a-form-item label="MTU(留 0 表示用 Mieru 的默认值)">
        <a-input-number v-model:value="form.mtu" :min="0" :max="1500" />
        <div class="mfm__hint">只对 UDP 传输有意义。填 0 之外的值时范围是 1280–1500。</div>
      </a-form-item>

      <!-- IPv6 那一块只在机器填了 IPv6 地址时出现:没填的话这几个输入框
           一个都不会生效,摆在那里只会让人以为配了却没起作用。 -->
      <template v-if="node.ipv6_address">
        <a-form-item label="IPv6 条目">
          <a-switch v-model:checked="form.ipv6_enabled" size="small" />
          <span class="mfm__inline">关掉后这个入口不再生成 IPv6 条目</span>
        </a-form-item>
        <template v-if="form.ipv6_enabled">
          <a-form-item label="IPv6 条目名称(留空表示在入口名后加 -IPV6)">
            <a-input v-model:value="form.ipv6_display_name" placeholder="留空即跟随" />
          </a-form-item>
          <a-form-item label="IPv6 公网端口段(两端都留 0 表示与 IPv4 公网段相同)">
            <a-space>
              <a-input-number
                v-model:value="form.ipv6_public_port_start"
                :min="0"
                :max="65535"
              />
              <span>—</span>
              <a-input-number v-model:value="form.ipv6_public_port_end" :min="0" :max="65535" />
            </a-space>
          </a-form-item>
        </template>
        <div class="mfm__hint mfm__hint--block">
          IPv6 条目<b>不是第二个入口</b>:它和 IPv4 是同一批监听端口的两个地址,
          用户凭据与流量统计全部共用,只有名字和公网端口段能单独设。
        </div>
      </template>

      <a-form-item label="访问等级">
        <a-select
          v-model:value="form.access_tier_id"
          :options="tiers.map((t) => ({ value: t.id, label: t.name }))"
        />
        <div class="mfm__hint">
          改等级会<b>立刻标脏并重新下发</b>这台机器 —— 那是安全问题:
          等级调高后,被移出的用户凭据还留在 mita 的用户列表里,拖多久就多能用多久。
        </div>
      </a-form-item>

      <a-form-item label="排序">
        <a-input-number v-model:value="form.sort_order" :min="0" />
      </a-form-item>

      <a-form-item label="下发到订阅">
        <a-switch v-model:checked="form.subscription_enabled" size="small" />
        <span class="mfm__inline">关掉后不再进新生成的订阅,已拿到订阅的人照常能连</span>
      </a-form-item>

      <a-form-item label="启用">
        <a-switch v-model:checked="form.enabled" size="small" />
        <span class="mfm__inline">关掉后下次下发时从 mita 配置里移除</span>
      </a-form-item>

      <a-form-item label="公开备注">
        <a-input v-model:value="form.public_remark" :maxlength="128" placeholder="用户可见" />
      </a-form-item>
    </a-form>
  </a-modal>
</template>

<style scoped>
/* 颜色只允许用 tokens.ts 里已有的那几个值 —— 这里是 text1 / text3 / warning。 */
.mfm__where {
  margin: 0 0 12px;
  font-size: 13px;
  color: #15181C;
}
.mfm__dim {
  color: #6B7480;
}
.mfm__intro {
  margin-bottom: 12px;
}
.mfm__hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: #6B7480;
}
.mfm__hint--block {
  margin: -4px 0 12px;
}
/* 端口数不一致是「值得看一眼」而不是「错了」,所以用 warning 不用 danger。 */
.mfm__warn {
  margin-top: 6px;
  font-size: 12px;
  line-height: 1.6;
  color: #92610A;
}
.mfm__inline {
  margin-left: 8px;
  font-size: 12px;
  color: #6B7480;
}
</style>
