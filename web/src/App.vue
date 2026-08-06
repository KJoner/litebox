<script setup lang="ts">
import { RouterView } from 'vue-router'
import zhCN from 'ant-design-vue/es/locale/zh_CN'
import { antdTheme } from '@/theme/antd'
</script>

<template>
  <!--
    视觉全部通过 ConfigProvider :theme 下发,不写覆盖 .ant-* 的全局样式表。
    颜色一律来自 @/theme/tokens,组件的 scoped CSS 里不再出现散落的十六进制色值 ——
    改一次要改四处的东西迟早会不一致。
  -->
  <a-config-provider :locale="zhCN" :theme="antdTheme">
    <a href="#lb-main" class="lb-skip-link">跳到主内容</a>
    <RouterView />
  </a-config-provider>
</template>
