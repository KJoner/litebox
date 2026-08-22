<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  PROTOCOL_LABEL,
  SS_METHOD_LABEL,
  type AccessTier,
  type Node,
  type NodeInbound,
  type NodeProtocol,
  type NodeSSMethod,
} from '@/api/client'

/**
 * 新增 / 编辑一个 sing-box 入口。
 *
 * 独立成组件是因为现在有两个地方要打开它:节点详情的「入口」Tab,
 * 以及跨节点的「入口管理」页。两处各写一份表单的话,加一个字段时
 * 漏掉一处的表现是「在这个页面填得进去、在那个页面填不进去」——
 * 而两处改的是同一行数据。
 *
 * 新增与编辑共用一个表单,与后端 InboundParams 的约定一致:
 * 两边各写一份校验,某天加了一项只改到一处,就会出现同样的怪事。
 */
const props = defineProps<{
  open: boolean
  /** null 表示新增 */
  inbound: NodeInbound | null
  /** 入口所属的机器。新增时必填 —— 它决定往哪台机器上加。 */
  node: Node
  tiers: AccessTier[]
  /** 这台机器已有的入口数,新增时用来给排序一个不重复的默认值。 */
  existingCount: number
}>()

const emit = defineEmits<{
  'update:open': [boolean]
  /** 保存成功。调用方据此重新拉数据 —— 组件自己不知道该刷新谁。 */
  saved: []
  /** 有请求在跑时抬起,容器据此屏蔽遮罩点击与 ESC。 */
  busy: [label: string]
}>()

const running = ref('')

const form = ref({
  display_name: '',
  protocol: 'VLESS_REALITY' as NodeProtocol,
  ss_method: '' as NodeSSMethod | '',
  listen_port: 0,
  public_port: 0,
  ipv6_public_port: 0,
  tcp_fast_open: false,
  access_tier_id: 0,
  sort_order: 0,
  subscription_enabled: true,
  enabled: true,
  public_remark: '',
})

// 每次打开都按当前 inbound 重置。留着上一次的值会让"新增"带上刚编辑过的
// 那个入口的端口,而端口冲突是保存时才报的 —— 管理员会以为是自己填错了。
watch(
  () => props.open,
  (open) => {
    if (!open) return
    const i = props.inbound
    form.value = i
      ? {
          display_name: i.display_name,
          protocol: i.protocol,
          ss_method: i.ss_method,
          listen_port: i.listen_port,
          public_port: i.public_port,
          ipv6_public_port: i.ipv6_public_port,
          tcp_fast_open: i.tcp_fast_open,
          access_tier_id: i.access_tier_id,
          sort_order: i.sort_order,
          subscription_enabled: i.subscription_enabled,
          enabled: i.enabled,
          public_remark: i.public_remark,
        }
      : {
          display_name: '',
          protocol: 'VLESS_REALITY',
          ss_method: '',
          listen_port: 0,
          public_port: 0,
          ipv6_public_port: 0,
          tcp_fast_open: false,
          // 默认普通组。不从别处继承 —— 机器已经没有等级了(迁移 0020),
          // 而"跟着上一个入口走"会让新入口悄悄带上一个限制。
          access_tier_id: props.tiers[0]?.id ?? 1,
          sort_order: props.existingCount,
          subscription_enabled: true,
          enabled: true,
          public_remark: '',
        }
  },
  { immediate: true },
)

/**
 * 切到 VLESS 之前必须先实测过握手目标。
 *
 * 在这里就说出来,而不是等后端拒绝 —— 后端那句话出现在保存失败的红字里,
 * 而管理员此刻正盯着的是协议那个下拉框。
 */
const protocolSwitchBlocked = computed(() => {
  const cur = props.inbound
  if (!cur || form.value.protocol !== 'VLESS_REALITY') return ''
  if (cur.protocol === 'VLESS_REALITY') return ''
  if (!cur.handshake_checked_at) {
    return '这个入口还没有实测过握手目标。REALITY 要求目标返回的每个 TLS 记录不超过 8192 字节,超限时握手会静默失败:客户端连不上,而节点上一切正常。请先跑一次「实测握手目标」。'
  }
  return ''
})

async function submit() {
  if (!form.value.display_name.trim()) {
    message.warning('请填写入口名称')
    return
  }
  if (!form.value.listen_port) {
    message.warning('请填写主机监听端口')
    return
  }
  running.value = props.inbound ? '正在保存入口' : '正在新增入口'
  emit('busy', running.value)
  try {
    if (props.inbound) {
      await api.updateInbound(props.inbound.id, { ...form.value })
    } else {
      await api.createInbound(props.node.id, { ...form.value })
    }
    emit('update:open', false)
    emit('saved')
    // 说「下次部署后生效」而不是「已保存」:自动部署会重启 sing-box,
    // 把这台机器上全部入口的在线连接一起踢掉,而他动的只是其中一个。
    message.success('已保存。入口的变更要重新部署这台机器才会生效')
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
    :title="inbound ? `编辑入口「${inbound.display_name}」` : '新增 sing-box 入口'"
    :confirm-loading="!!running"
    :ok-button-props="{ disabled: !!protocolSwitchBlocked }"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="submit"
  >
    <!-- 跨节点的入口管理页里,同名入口可以有好几个 —— 不写清楚是哪台机器,
         管理员会改到另一台上去,而那一下要重启的是别人的连接。 -->
    <p class="ifm__where">
      所属机器 <b>{{ node.display_name || node.name }}</b>
      <span class="ifm__dim"> · {{ node.host }}</span>
    </p>
    <a-form layout="vertical" size="small">
      <a-form-item label="入口名称(会发给用户)">
        <a-input v-model:value="form.display_name" placeholder="例如:洛杉矶 01-SS" />
      </a-form-item>
      <a-form-item label="落地协议">
        <a-radio-group v-model:value="form.protocol" size="small" button-style="solid">
          <a-radio-button value="VLESS_REALITY">{{ PROTOCOL_LABEL.VLESS_REALITY }}</a-radio-button>
          <a-radio-button value="SHADOWSOCKS">{{ PROTOCOL_LABEL.SHADOWSOCKS }}</a-radio-button>
        </a-radio-group>
        <div v-if="protocolSwitchBlocked" class="ifm__warn">{{ protocolSwitchBlocked }}</div>
      </a-form-item>
      <a-form-item v-if="form.protocol === 'SHADOWSOCKS'" label="加密方法">
        <a-select
          v-model:value="form.ss_method"
          :options="Object.entries(SS_METHOD_LABEL).map(([v, l]) => ({ value: v, label: l }))"
        />
      </a-form-item>
      <a-form-item label="主机监听端口(sing-box 真正 bind 的号码)">
        <a-input-number v-model:value="form.listen_port" :min="1" :max="65535" />
      </a-form-item>
      <a-form-item label="公网端口(留 0 表示与监听端口相同)">
        <a-input-number v-model:value="form.public_port" :min="0" :max="65535" />
        <div class="ifm__hint">
          NAT 主机上两者不同:公网 443 映射到主机的 20443 时,监听端口填 20443、公网端口填
          443。填反了 sing-box 会监听在转发链路另一端的号码上,而各项检查都会通过。
        </div>
      </a-form-item>
      <a-form-item v-if="node.ipv6_address" label="IPv6 公网端口(留 0 表示跟随 IPv4 公网端口)">
        <a-input-number v-model:value="form.ipv6_public_port" :min="0" :max="65535" />
      </a-form-item>
      <a-form-item>
        <a-checkbox v-model:checked="form.tcp_fast_open">
          TCP Fast Open(同时管两端:入站与订阅里下发的出站)
        </a-checkbox>
        <div class="ifm__hint">
          默认关。成败取决于用户到这台机器这一段路径上的中间设备,而那条路面板看不到。
        </div>
      </a-form-item>
      <a-form-item label="访问等级">
        <a-select
          v-model:value="form.access_tier_id"
          :options="tiers.map((t) => ({ value: t.id, label: t.name }))"
        />
        <div class="ifm__hint">
          等级不高于它的用户会自动拿到这个入口。<b>等级挂在入口上,不在机器上</b> ——
          同一台机器可以有一个对所有人开放的入口和一个只给 VIP 的入口。
          <br />
          管理员在用户详情里对整台机器的「额外授权」<b>穿透这一档</b>:
          那句话的意思就是「这台机器给他用」。
        </div>
      </a-form-item>
      <a-form-item label="排序">
        <a-input-number v-model:value="form.sort_order" />
      </a-form-item>
      <a-form-item label="对用户公开的备注">
        <a-input v-model:value="form.public_remark" />
      </a-form-item>
      <a-form-item>
        <a-checkbox v-model:checked="form.enabled">
          启用(关掉后下次部署时从节点上撤掉)
        </a-checkbox>
        <a-checkbox v-model:checked="form.subscription_enabled">下发到订阅</a-checkbox>
      </a-form-item>
    </a-form>
  </a-modal>
</template>

<style scoped>
/* 颜色只允许用 tokens.ts 里已有的那几个值 —— 这里是 text1 / text3 / danger。 */
.ifm__where {
  margin: 0 0 12px;
  font-size: 13px;
  color: #15181C;
}
.ifm__dim {
  color: #6B7480;
}
.ifm__hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: #6B7480;
}
.ifm__warn {
  margin-top: 6px;
  font-size: 12px;
  line-height: 1.6;
  color: #B4291D;
}
</style>
