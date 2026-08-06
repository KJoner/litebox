<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type AccessTier, type Node } from '@/api/client'
import { LbSensitiveField } from '@/components/lb'
import { fromBytes, toBytes, type LbQuotaUnit } from '@/components/user/quota'
import { formatUTCTime } from '@/utils/format'

/**
 * 新建 / 编辑节点。同一个表单两种 mode,差异有三处:
 *
 *   接入方式  只在新建时出现。它回答的是「怎么把面板公钥装进节点」,
 *             不是「用哪把密钥登录」—— 面板始终用自己那把专用密钥。
 *             建好之后再要重装公钥,走详情页的「重新引导」。
 *   运营字段  订阅开关、维护说明、公开备注只在编辑时出现:
 *             新建时节点还没部署过,压根不在任何人的订阅里。
 *   保存结果  新建有两种(公钥装上了 / 没装上),保存有三种(见 submit)。
 *
 * 握手目标不在这个表单里:它必须从节点本机实测通过才能写库,
 * 走详情页的「扫描握手目标」。放进来等于给了一个绕过 8192 字节校验的入口。
 */
const props = defineProps<{
  open: boolean
  /** null = 新建 */
  node: Node | null
  tiers: AccessTier[]
  /** 编辑时用来显示「下一次重置」,由后端算好 */
  nextResetAt?: string | null
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved', nodeID: number): void
  (e: 'deploy', nodeID: number): void
}>()

const isEdit = computed(() => props.node !== null)
const submitting = ref(false)
/** 只有服务端知道的错误(重名、SSH 不通)放表单顶部,不弹吐司。 */
const serverError = ref('')

/**
 * 接入方式:
 *   password  填节点口令,面板用一次就丢,不保存、不写日志;
 *   local-key 用主控本机 ~/.ssh 里的私钥去装;
 *   manual    给这个节点单配一把私钥。
 */
type AccessMode = 'password' | 'local-key' | 'manual'
const accessMode = ref<AccessMode>('password')

const blank = {
  name: '',
  display_name: '',
  access_tier_id: 1,
  sort_order: 0,
  host: '',
  ipv6_address: '',
  quota_value: null as number | null,
  quota_unit: 'GB' as LbQuotaUnit,
  traffic_reset_cycle: 'NONE' as 'NONE' | 'MONTHLY',
  traffic_reset_day: 1,
  ssh_port: 22,
  ssh_user: 'root',
  ssh_key: '',
  root_password: '',
  proxy_port: 443,
  listen_port: 0,
  api_port: 28080,
  subscription_enabled: true,
  public_remark: '',
  maintenance_message: '',
}
const form = reactive({ ...blank })
let snapshot = JSON.stringify(blank)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    serverError.value = ''
    const n = props.node
    if (!n) {
      accessMode.value = 'password'
      Object.assign(form, blank, { access_tier_id: props.tiers[0]?.id ?? 1 })
    } else {
      const q = fromBytes(n.traffic_quota_bytes)
      accessMode.value = 'manual'
      Object.assign(form, blank, {
        name: n.name,
        display_name: n.display_name,
        access_tier_id: n.access_tier_id,
        sort_order: n.sort_order,
        host: n.host,
        ipv6_address: n.ipv6_address,
        quota_value: q.value,
        quota_unit: q.unit,
        traffic_reset_cycle: n.traffic_reset_cycle,
        traffic_reset_day: n.traffic_reset_day,
        ssh_port: n.ssh_port,
        ssh_user: n.ssh_user,
        proxy_port: n.proxy_port,
        // 与公网端口相同时按「未配置转发」展示,免得看起来像特意填了两个一样的值。
        listen_port: n.listen_port === n.proxy_port ? 0 : n.listen_port,
        api_port: n.api_port,
        subscription_enabled: n.subscription_enabled,
        public_remark: n.public_remark,
        maintenance_message: n.maintenance_message,
      })
    }
    snapshot = JSON.stringify(form)
  },
)

const fieldLabels: Record<string, string> = {
  name: '内部名称',
  display_name: '展示名称',
  access_tier_id: '访问等级',
  sort_order: '排序',
  host: 'IPv4 地址',
  ipv6_address: 'IPv6 地址',
  quota_value: '流量限额',
  quota_unit: '流量限额',
  traffic_reset_cycle: '重置周期',
  traffic_reset_day: '每月重置日',
  ssh_port: 'SSH 端口',
  ssh_user: 'SSH 用户',
  ssh_key: 'SSH 私钥',
  proxy_port: '公网代理端口',
  listen_port: '主机代理端口',
  api_port: 'API 端口',
  subscription_enabled: '下发到用户订阅',
  public_remark: '公开备注',
  maintenance_message: '维护说明',
}

/** 只在确实脏了时拦截关闭,并列出改了哪几项 —— 只说「有未保存的修改」还得回去找。 */
const dirtyFields = computed(() => {
  const before = JSON.parse(snapshot) as typeof form
  const out = new Set<string>()
  for (const k of Object.keys(fieldLabels)) {
    if (JSON.stringify((before as never)[k]) !== JSON.stringify((form as never)[k])) {
      out.add(fieldLabels[k])
    }
  }
  return [...out]
})

/** 周期边界只有后端算。表单改了但还没保存时,上面那个时间就不再对应当前选择。 */
const cycleDirty = computed(
  () =>
    !!props.node &&
    (props.node.traffic_reset_cycle !== form.traffic_reset_cycle ||
      props.node.traffic_reset_day !== form.traffic_reset_day),
)

function close() {
  emit('update:open', false)
}

function tryClose() {
  if (dirtyFields.value.length === 0) return close()
  Modal.confirm({
    title: '放弃未保存的修改?',
    content: `已改动 ${dirtyFields.value.length} 项:${dirtyFields.value.join('、')}。`,
    okText: '放弃',
    okType: 'danger',
    cancelText: '继续编辑',
    autoFocusButton: 'cancel',
    onOk: close,
  })
}

async function submit() {
  if (!form.name.trim()) {
    serverError.value = '请填写内部名称'
    return
  }
  if (!form.host.trim()) {
    serverError.value = '请填写 IPv4 地址'
    return
  }
  if (!isEdit.value && accessMode.value === 'password' && !form.root_password) {
    serverError.value = '请填写节点登录密码,或改用其他接入方式'
    return
  }
  if (!isEdit.value && accessMode.value === 'manual' && !form.ssh_key.trim()) {
    serverError.value = '请粘贴该节点的 SSH 私钥,或改用其他接入方式'
    return
  }

  submitting.value = true
  serverError.value = ''
  const quota = toBytes(form.quota_value, form.quota_unit)

  try {
    if (!isEdit.value) {
      const result = await api.createNode({
        name: form.name,
        display_name: form.display_name,
        access_tier_id: form.access_tier_id,
        sort_order: form.sort_order,
        host: form.host,
        ipv6_address: form.ipv6_address,
        traffic_quota_bytes: quota,
        traffic_reset_cycle: form.traffic_reset_cycle,
        traffic_reset_day: form.traffic_reset_day,
        ssh_port: form.ssh_port,
        ssh_user: form.ssh_user,
        proxy_port: form.proxy_port,
        listen_port: form.listen_port,
        api_port: form.api_port,
        // 接入方式决定后端走哪条引导路径。两者不能一起发过去。
        ssh_key: accessMode.value === 'manual' ? form.ssh_key : '',
        root_password: accessMode.value === 'password' ? form.root_password : '',
      })
      // 口令与私钥只在这一次请求里用到,立刻从表单状态里抹掉。
      form.root_password = ''
      form.ssh_key = ''
      close()

      if (result.bootstrap_error) {
        // 主对象成功、附属步骤失败,必须分开说 —— 否则管理员会再建一个节点,
        // 而一台机器只能承载一个节点,两条记录会互相覆盖配置。
        Modal.warning({
          title: '节点已创建,但公钥没能装上去',
          width: 560,
          content: `${result.bootstrap_error}\n\n节点记录已保留。处理好之后在节点详情里点「重新引导」重试,不要重新创建节点。`,
          okText: '知道了',
        })
      } else {
        message.success('节点已创建,公钥已装好。接下来依次执行「探测」和「安装 sing-box」')
      }
      emit('saved', result.node.id)
      return
    }

    const id = props.node!.id
    // 逐字段列出而不是 { ...form }:更新接口对未知字段是拒绝的
    // (DisallowUnknownFields),root_password 这类只属于新建的字段会让整个提交失败。
    const { effect } = await api.updateNode(id, {
      name: form.name,
      display_name: form.display_name,
      host: form.host,
      // 留空即清空 IPv6,订阅里的 IPv6 条目随即消失。
      ipv6_address: form.ipv6_address,
      traffic_quota_bytes: quota,
      traffic_reset_cycle: form.traffic_reset_cycle,
      traffic_reset_day: form.traffic_reset_day,
      ssh_port: form.ssh_port,
      ssh_user: form.ssh_user,
      ssh_key: form.ssh_key,
      proxy_port: form.proxy_port,
      listen_port: form.listen_port,
      api_port: form.api_port,
      access_tier_id: form.access_tier_id,
      sort_order: form.sort_order,
      subscription_enabled: form.subscription_enabled,
      public_remark: form.public_remark,
      maintenance_message: form.maintenance_message,
    })
    form.ssh_key = ''
    close()
    emit('saved', id)

    // 三种保存结果,差别来自后端返回的 effect,前端不自己判断。
    if (effect.tier_changed) {
      // 等级变更由面板自动标脏。这一种不给「稍后部署」的选项 ——
      // 拖着不部署等于被移出的用户还能继续用,那是权限没有真正收回。
      message.success(
        `已保存:${effect.changes.join(';')}。访问等级变了,受影响节点已排入自动重新部署。`,
      )
    } else if (effect.needs_deploy) {
      Modal.confirm({
        title: '配置已保存,但尚未在节点上生效',
        width: 460,
        content: `${effect.changes.join(';')}。部署会重启这台机器上的 sing-box,断开它当前的全部在线连接。`,
        okText: '立即部署',
        cancelText: '稍后手动部署',
        onOk: () => emit('deploy', id),
      })
    } else {
      message.success(
        effect.changes.length
          ? `已保存 · 无需部署(${effect.changes.join(';')})`
          : '没有任何改动',
      )
    }
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="isEdit ? `编辑节点 · ${props.node?.display_name || props.node?.name}` : '新增节点'"
    :width="560"
    :confirm-loading="submitting"
    :ok-text="isEdit ? '保存' : '创建节点'"
    :ok-button-props="{ disabled: isEdit && dirtyFields.length === 0 }"
    cancel-text="取消"
    :mask-closable="false"
    @cancel="tryClose"
    @ok="submit"
  >
    <a-alert
      v-if="serverError"
      type="error"
      show-icon
      class="nf__err"
      :message="serverError"
      description="节点没有被创建或修改,表单内容已保留。"
    />

    <a-alert
      v-if="!isEdit"
      type="info"
      show-icon
      class="nf__err"
      message="一台机器只能作为一个节点"
      description="节点上的路径与服务名是固定的,两个节点指向同一主机会互相覆盖配置。"
    />

    <a-form layout="vertical">
      <a-form-item label="内部名称" required>
        <a-input v-model:value="form.name" placeholder="例如:LAX-cn2gia-到期20261201" />
        <div class="nf__help">只在管理后台出现。可以写机房、供应商、到期日。</div>
      </a-form-item>

      <a-form-item label="展示名称">
        <a-input v-model:value="form.display_name" placeholder="例如:洛杉矶 01" />
        <div class="nf__help">
          用户订阅与门户里显示的就是它。留空则与内部名称相同 —— 那样用户会看到「到期20261201」。
        </div>
      </a-form-item>

      <a-row :gutter="12">
        <a-col :span="14">
          <a-form-item label="访问等级">
            <a-select v-model:value="form.access_tier_id">
              <a-select-option v-for="t in props.tiers" :key="t.id" :value="t.id">
                {{ t.name }}
              </a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
        <a-col :span="10">
          <a-form-item label="排序">
            <a-input-number v-model:value="form.sort_order" style="width: 100%" />
          </a-form-item>
        </a-col>
      </a-row>
      <div class="nf__help nf__help--row">
        等级不高于该等级的用户会自动继承这个节点。数值小的排在订阅前面。
      </div>

      <template v-if="isEdit">
        <a-form-item>
          <a-checkbox v-model:checked="form.subscription_enabled">下发到用户订阅</a-checkbox>
          <div class="nf__help">
            关掉后不再进入新生成的订阅。节点、历史流量与部署记录都保留,sing-box 照常运行,
            已导入旧订阅的客户端仍能连上。<strong>这不是「禁用节点」。</strong>
          </div>
        </a-form-item>

        <a-form-item label="维护说明">
          <a-input v-model:value="form.maintenance_message" :maxlength="128" placeholder="例如:机房 8/6 02:00 UTC 计划断电" />
          <div class="nf__help">
            用户可见。留空表示无 —— 门户上用户只看到「维护中」,不知道原因。
          </div>
        </a-form-item>

        <a-form-item label="公开备注">
          <a-input v-model:value="form.public_remark" :maxlength="128" placeholder="例如:流媒体解锁" />
          <div class="nf__help">
            用户可见,最多 128 字。与维护说明是两回事:备注常驻,维护说明只在停发订阅时才有意义。
          </div>
        </a-form-item>
      </template>

      <a-form-item label="IPv4 地址" required>
        <a-input v-model:value="form.host" placeholder="例如:45.77.12.90" />
        <div class="nf__help">用于 SSH 管理、节点部署和 IPv4 订阅,必须填写。</div>
      </a-form-item>

      <a-form-item label="IPv6 地址">
        <a-input v-model:value="form.ipv6_address" placeholder="例如:2602:fed2:7116:2110::1" />
        <div class="nf__help">
          选填,只用于订阅下发。填写后订阅中额外生成「{{ form.display_name || form.name || '展示名称' }}-IPV6」条目,
          清空即撤下 —— 两种改动都不需要重新部署。
        </div>
      </a-form-item>

      <a-row :gutter="12">
        <a-col :span="9">
          <a-form-item label="流量限额">
            <a-input-number
              v-model:value="form.quota_value"
              :min="0"
              :precision="2"
              placeholder="不限量"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
        <a-col :span="6">
          <a-form-item label="单位">
            <a-select v-model:value="form.quota_unit">
              <a-select-option value="GB">GB</a-select-option>
              <a-select-option value="TB">TB</a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
        <a-col :span="9">
          <a-form-item label="重置周期">
            <a-select v-model:value="form.traffic_reset_cycle">
              <a-select-option value="NONE">不重置</a-select-option>
              <a-select-option value="MONTHLY">每月</a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
      </a-row>

      <a-form-item v-if="form.traffic_reset_cycle === 'MONTHLY'" label="每月重置日">
        <a-input-number v-model:value="form.traffic_reset_day" :min="1" :max="31" style="width: 160px" />
        <!-- 节点是 1~31,用户是 1~28。两处规则不同,各自写准确的帮助文字。 -->
        <div class="nf__help">
          1~31 日。当月没有该日时按当月最后一天处理(31 日在二月即 28 或 29 日)。边界统一取 UTC 00:00。
        </div>
      </a-form-item>

      <div v-if="isEdit && props.nextResetAt" class="nf__help nf__help--row">
        下一次重置:{{ formatUTCTime(props.nextResetAt) }}
        <span v-if="cycleDirty">(这是按已保存的设置算的,保存后会按新设置重新计算)</span>
      </div>

      <div class="nf__note">
        节点额度只用于统计与预警:超额会在仪表盘和列表里标红,但<strong>不会</strong>停掉 sing-box、
        不会禁用节点,也不会把节点从订阅里摘掉 —— 那会同时打断这台机器上的全部用户。
      </div>

      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="SSH 端口">
            <a-input-number v-model:value="form.ssh_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="SSH 用户">
            <a-input v-model:value="form.ssh_user" />
          </a-form-item>
        </a-col>
      </a-row>

      <template v-if="!isEdit">
        <a-form-item label="接入方式">
          <a-radio-group v-model:value="accessMode" button-style="solid">
            <a-radio-button value="password">节点密码</a-radio-button>
            <a-radio-button value="local-key">主控本机私钥</a-radio-button>
            <a-radio-button value="manual">手工指定私钥</a-radio-button>
          </a-radio-group>
          <div class="nf__help">
            面板有一把自己的专用密钥。这里选的是<strong>怎么把它的公钥装进节点</strong>,
            不是用哪把密钥登录。
          </div>
        </a-form-item>

        <LbSensitiveField
          v-if="accessMode === 'password'"
          v-model:value="form.root_password"
          label="节点登录密码"
          mode="create"
          required
          help="只用于把面板公钥装进节点的那一次连接。不会保存,也不会写进日志或审计详情。"
        />

        <!-- 这条分支没有输入框。用 Alert 而不是留白 —— 空白区会让人以为表单还没加载完。 -->
        <a-alert
          v-else-if="accessMode === 'local-key'"
          type="info"
          show-icon
          class="nf__err"
          message="用主控本机的私钥去装公钥"
          description="面板会在自己进程的 ~/.ssh 与 /etc/litebox/keys 下找一把能登录该节点的私钥。找不到或登录不上时,改用「节点密码」。"
        />

        <LbSensitiveField
          v-else
          v-model:value="form.ssh_key"
          label="SSH 私钥"
          mode="create"
          multiline
          required
          help="给这个节点单配一把私钥,用主密钥加密后存储,不会再次显示。"
        />
      </template>

      <LbSensitiveField
        v-else
        v-model:value="form.ssh_key"
        label="SSH 私钥"
        mode="edit"
        multiline
        help="用面板专用密钥的节点请一直留空;填入新私钥则给这个节点单独换一把。编辑态永不回显,空框不代表密钥丢了。"
      />

      <a-row :gutter="12">
        <a-col :span="8">
          <a-form-item label="公网代理端口">
            <a-input-number v-model:value="form.proxy_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
        <a-col :span="8">
          <a-form-item label="主机代理端口">
            <a-input-number
              v-model:value="form.listen_port"
              :min="0"
              :max="65535"
              placeholder="不填"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
        <a-col :span="8">
          <a-form-item label="API 端口">
            <a-input-number v-model:value="form.api_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
      </a-row>
      <div class="nf__help nf__help--row">
        公网端口写进订阅;主机端口是 sing-box 实际监听的那个,留空表示与公网相同;API 端口仅监听节点回环。
      </div>

      <a-alert
        v-if="form.listen_port && form.listen_port !== form.proxy_port"
        type="warning"
        show-icon
        :message="`需要自行把 ${form.host || '节点'}:${form.proxy_port} 转发到本机 ${form.listen_port}`"
        description="面板不会创建这条转发规则。NAT 主机由服务商的端口映射完成,自建则用 nginx stream 或 iptables DNAT;sing-box 只负责监听主机端口。"
      />

      <div v-if="isEdit" class="nf__help nf__help--row">
        REALITY 握手目标不在这里改:它必须从节点本机实测通过才能保存。
        到节点详情里「扫描握手目标」,检测后应用。
      </div>
    </a-form>

    <div v-if="isEdit && dirtyFields.length" class="nf__foot">
      已改动 {{ dirtyFields.length }} 项:{{ dirtyFields.join('、') }}
    </div>
  </a-modal>
</template>

<style scoped>
.nf__err {
  margin-bottom: 16px;
}

.nf__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}

.nf__help--row {
  margin: -12px 0 20px;
}

.nf__note {
  margin-bottom: 20px;
  padding: 10px 12px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.7;
  color: #576070;
}

.nf__foot {
  padding-top: 4px;
  font-size: 11.5px;
  color: #6b7480;
}
</style>
