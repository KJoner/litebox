<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import {
  portalApi,
  ApiError,
  profileKindHint,
  profileKindLabel,
  type PortalSubscription,
} from '@/api/client'
import { LbCopyField, LbEmptyState, LbTimeText, lbDangerConfirm } from '@/components/lb'

/**
 * 我的订阅。这一页只有一件事:把地址交到客户端里。
 *
 * 三种格式改成主次分层 —— 原来三个并列、标签是「通用订阅(Base64)」
 * 这种混着技术词的写法。用户不需要知道 base64 是什么,只需要知道
 * 「我的客户端叫什么名字」。
 */
const data = ref<PortalSubscription | null>(null)
const loading = ref(true)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = await portalApi.subscription()
  } catch (err) {
    // 门户的错误文案不给请求 ID、不给状态码 —— 用户拿它做不了任何事。
    loadError.value = err instanceof ApiError ? err.message : '暂时读不到订阅信息'
    data.value = null
  } finally {
    loading.value = false
  }
}

onMounted(load)

const formats = computed(() => {
  const d = data.value
  if (!d || !d.available) return []
  return [
    {
      key: 'base64',
      name: '通用订阅',
      apps: 'v2rayN · Shadowrocket 等',
      url: d.url_base64,
      primary: true,
    },
    // Clash 用户单独给一条原生配置,而不是让他们用上面那条通用订阅。
    //
    // 两者不是同一份东西的两种包装:通用订阅是分享链接的列表,而**有些线路
    // 没有通用的分享链接**,在那条路上它们压根不存在 —— 用户看到的是节点数
    // 比别人少,而没有任何提示。这句差别要写在 apps 里,不然没人会换。
    {
      key: 'clash',
      name: 'Clash 配置',
      apps: 'Clash Meta · mihomo · Clash Verge 等,部分线路只有这一种能用',
      url: d.url_clash,
      primary: false,
    },
    {
      key: 'singbox',
      name: 'sing-box 配置',
      apps: 'sing-box 客户端直接导入',
      url: d.url_singbox,
      primary: false,
    },
    {
      key: 'uri',
      name: '明文链接',
      apps: '用于人工核对,一般不需要',
      url: d.url_uri,
      primary: false,
    },
  ]
})

/**
 * 配置文件。管理员一份都没配时,整块不出现 ——
 * 不显示灰掉的按钮,也不显示「暂未配置」:用户对此做不了任何事,
 * 写出来只会让他来问。
 */
// 只列出【当前用户能用】的配置文件。不能用的整条不显示 —— 不显示灰掉的
// 按钮,也不显示「这一份暂时不能用」:用户对一份自己等级里没有落地节点的
// 配置做不了任何事,列出来只会引出「为什么我不能用」。available 是后端真的
// 渲染过一遍得出来的(Service.ProfileLinks),不是按字段猜的。
const profiles = computed(() =>
  (data.value?.available ? (data.value.profiles ?? []) : []).filter((p) => p.available),
)

function regenerate() {
  lbDangerConfirm({
    title: '重新生成订阅地址?',
    okText: '重新生成',
    impacts: [
      '旧地址立即失效',
      '你的全部设备都要重新导入一次',
      '节点上的凭据不变,不影响正在连接的会话',
    ],
    footer: '只在怀疑地址泄露时使用。',
    onOk: async () => {
      try {
        data.value = await portalApi.regenerateSubscription()
        message.success('已重新生成,请在所有设备上重新导入')
      } catch (err) {
        message.error(err instanceof ApiError ? err.message : '重新生成失败')
      }
    },
  })
}
</script>

<template>
  <div class="ps">
    <div class="ps__head">
      <h2 class="ps__title">我的订阅</h2>
      <!-- 不可用时后端把节点数一并置零,这行写出来只会变成第二次「你没有节点」。 -->
      <div v-if="data?.available" class="ps__sub">
        {{ data.node_count }} 个节点 · 订阅共 {{ data.entry_count }} 条<template
          v-if="data.ipv6_count"
        >
          ({{ data.ipv6_count }} 个节点额外提供 IPv6)</template
        >
      </div>
    </div>

    <LbEmptyState v-if="loadError" variant="error" :title="loadError" @retry="load" />

    <div v-else-if="loading || !data" class="ps__skel">
      <a-skeleton active :paragraph="{ rows: 4 }" />
    </div>

    <!--
      不可用时整块替换,而不是把复制按钮置灰。
      置灰的按钮只传达「现在不能点」,传达不了「为什么」和「什么时候能」。
    -->
    <template v-else-if="!data.available">
      <div class="ps__card">
        <div class="ps__unavailable">
          <div class="ps__unavailable-title">订阅当前不可用</div>
          <div class="ps__unavailable-body">{{ data.reason }}</div>
        </div>
        <div class="ps__card-body">
          <div class="ps__note">
            恢复后这里会重新显示三种格式的地址。<strong>地址本身不会变</strong> ——
            不需要重新导入,现在复制出去也只会拿到一个说明原因的响应。
          </div>
        </div>
      </div>
    </template>

    <template v-else>
      <section class="ps__card">
        <div class="ps__card-head">
          <span>节点订阅</span>
          <span class="ps__card-note">不确定选哪个?多数客户端用第一个</span>
        </div>
        <div class="ps__card-body">
          <div v-for="f in formats" :key="f.key" class="ps__fmt" :class="{ 'ps__fmt--main': f.primary }">
            <div class="ps__fmt-head">
              <span class="ps__fmt-name">{{ f.name }}</span>
              <span v-if="f.primary" class="ps__fmt-badge">推荐</span>
              <span class="ps__fmt-apps">{{ f.apps }}</span>
            </div>
            <LbCopyField
              :value="f.url"
              middle-ellipsis
              :primary="f.primary"
              :button-text="f.primary ? '复制订阅地址' : '复制'"
            />
          </div>

          <!--
            订阅可用但一条节点都没有:地址导进客户端会得到一个空列表,
            用户多半会以为是自己复制错了。这句话必须写出来。
          -->
          <div v-if="data.entry_count === 0" class="ps__note ps__note--warn">
            <strong>你的订阅里目前没有任何节点。</strong>
            地址是有效的,但导入客户端后会是空的 —— 你的访问等级下暂时没有分配节点,
            或者全部节点都在维护中。节点恢复后会自动出现,不需要重新导入。请联系管理员。
          </div>

          <div class="ps__note ps__note--warn">
            这些地址<strong>等同于你的密码</strong>。任何拿到它的人都能用你的流量,
            不要转发、不要发到群里、不要截图。怀疑泄露时用下面的「重新生成」。
          </div>
        </div>
      </section>

      <!--
        配置文件与上面的节点订阅是两件事:上面导进去是一串节点,
        这里导进去会替换掉整份配置(分流规则、DNS、代理方式都在里面)。
        并排放在同一张卡里会让人随便点一个。
      -->
      <section v-if="profiles.length" class="ps__card">
        <div class="ps__card-head">
          <span>配置文件</span>
          <span class="ps__card-note">已经配好分流规则,适合不想自己折腾的人</span>
        </div>
        <div class="ps__card-body">
          <div class="ps__note">
            这些是<strong>整份配置</strong>,导入后会替换客户端里的规则设置,
            和上面的节点订阅不是一回事。按你用的客户端选一个,
            <strong>只需要选一个</strong>。
          </div>

          <div v-for="p in profiles" :key="p.id" class="ps__fmt">
            <div class="ps__fmt-head">
              <span class="ps__fmt-name">{{ p.name }}</span>
              <span class="ps__fmt-apps">{{ profileKindLabel[p.kind] }}</span>
            </div>
            <div class="ps__fmt-hint">{{ p.description || profileKindHint[p.kind] }}</div>

            <!-- 列表已按 available 过滤,这里出现的都是能用的。 -->
            <LbCopyField :value="p.url" middle-ellipsis button-text="复制" />
          </div>
        </div>
      </section>

      <section class="ps__card">
        <div class="ps__card-head"><span>怎么用</span></div>
        <div class="ps__card-body">
          <ol class="ps__steps">
            <li>复制上方对应客户端的地址。</li>
            <li>在客户端里新增订阅并粘贴。</li>
            <li>节点有变动时,客户端里手动更新一次订阅才能看到 —— 面板不会主动推给你。</li>
          </ol>
          <!--
            小火箭把「配置」与「节点」分成两处,这是它最容易卡住人的地方:
            只导了配置会发现一个节点都没有,而界面上没有任何提示。
          -->
          <div v-if="profiles.some((p) => p.kind === 'SHADOWROCKET')" class="ps__note">
            用<strong>小火箭</strong>的话要做两次:配置文件在「配置」里添加,
            节点用上面的<strong>通用订阅</strong>地址在首页添加。少做一步就会发现没有节点可选。
          </div>
        </div>
      </section>

      <section class="ps__card">
        <div class="ps__card-head"><span>拉取情况</span></div>
        <div class="ps__card-body">
          <div class="ps__facts">
            <div>
              <span>最近拉取</span>
              <b><LbTimeText :value="data.last_access_at" empty="从未拉取" /></b>
            </div>
            <div>
              <span>累计拉取</span>
              <b class="lb-mono">{{ data.access_count }} 次</b>
            </div>
          </div>
          <div class="ps__note">
            如果这里长时间没有更新,说明客户端没有在拉订阅 —— 检查客户端里的自动更新设置。
          </div>
        </div>
      </section>

      <section class="ps__card">
        <div class="ps__card-head"><span>重新生成订阅地址</span></div>
        <div class="ps__card-body">
          <div class="ps__note">
            只在怀疑地址泄露时使用。旧地址立即失效,所有设备都要重新导入。
          </div>
          <div>
            <a-button danger @click="regenerate">重新生成</a-button>
          </div>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.ps {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.ps__head {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.ps__title {
  margin: 0;
  font-size: 19px;
  font-weight: 600;
}

.ps__sub {
  font-size: 12.5px;
  color: #6b7480;
}

.ps__skel {
  padding: 16px;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.ps__card {
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
  overflow: hidden;
}

.ps__card-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  padding: 12px 16px;
  border-bottom: 1px solid #edeff2;
  font-size: 13px;
  font-weight: 600;
}

.ps__card-note {
  font-size: 11.5px;
  font-weight: 400;
  color: #6b7480;
}

.ps__card-body {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 16px;
}

.ps__unavailable {
  padding: 14px 16px;
  background: #fdecea;
  border-bottom: 1px solid #f3cfc9;
}

.ps__unavailable-title {
  font-size: 13px;
  font-weight: 600;
  color: #8e2117;
}

.ps__unavailable-body {
  margin-top: 5px;
  font-size: 12.5px;
  line-height: 1.8;
  color: #8e2117;
}

.ps__fmt {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 13px;
  border: 1px solid #edeff2;
  border-radius: 6px;
}

.ps__fmt--main {
  background: #f6f9fd;
  border-color: #c9dcf3;
}

.ps__fmt-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
}

.ps__fmt-name {
  font-size: 13px;
  font-weight: 600;
}

.ps__fmt-badge {
  padding: 1px 6px;
  background: #2563b8;
  border-radius: 3px;
  font-size: 10.5px;
  font-weight: 500;
  color: #fff;
}

.ps__fmt-apps {
  font-size: 11.5px;
  color: #6b7480;
}

.ps__fmt-hint {
  font-size: 11.5px;
  line-height: 1.7;
  color: #576070;
}

.ps__fmt-blocked {
  padding: 9px 11px;
  background: #f1f3f5;
  border: 1px solid #dfe3e8;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.8;
  color: #5c6672;
}

.ps__note {
  padding: 11px 13px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 6px;
  font-size: 12px;
  line-height: 1.8;
  color: #576070;
}

.ps__note--warn {
  background: #fcf3e3;
  border-color: #efdcb4;
  color: #5c4405;
}

.ps__steps {
  margin: 0;
  padding-left: 20px;
  font-size: 12.5px;
  line-height: 2;
  color: #576070;
}

.ps__facts {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.ps__facts > div {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.ps__facts span {
  font-size: 11.5px;
  color: #6b7480;
}

.ps__facts b {
  font-size: 12.5px;
  font-weight: 500;
}
</style>
