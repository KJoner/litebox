import { theme } from 'ant-design-vue'
import type { ThemeConfig } from 'ant-design-vue/es/config-provider/context'
import { font, palette, radius, type ThemeMode } from './tokens'

/**
 * 通过 ConfigProvider :theme 下发。按主题现算 —— 深色时换 darkAlgorithm,
 * 同时把 tokens.ts 里那组深色实值灌进去(算法只负责派生,基色仍由我们定)。
 *
 * 注意版本差异:设计稿的 Token 映射表是照 AntD React v5 写的
 * (Table.headerBg、Layout.siderBg、Menu.itemSelectedBg…),
 * 而 ant-design-vue 4.2.6 的 component token 还停在 v5.0 那一代命名。
 * 那些键在这个版本里既过不了类型检查、运行期也被静默忽略 ——
 * 照抄只会得到「配了但没生效」。所以这里一律用本版本真正认的键。
 *
 * 还有一类键这个版本根本不给覆盖:Table 与 Card 在 styleFn 内部用 mergeToken
 * 重算了一遍自己的派生 token,components.Table 传进去的值会被当场盖掉。
 * 它们改走 alias token(见 colorFillAlter)或 styles/antd-tune.css。
 *
 * components.* 里除了各组件自己的 token,还能塞任意 alias token(类型是
 * `Partial<ComponentToken> & Partial<AliasToken>`)—— 按钮的胶囊圆角就是这么来的:
 * 全局 borderRadiusSM 仍是 8(下拉项、分段控件用),只有 Button 三档全 999。
 */
export function antdTheme(mode: ThemeMode): ThemeConfig {
  const p = palette[mode]
  return {
    algorithm: mode === 'dark' ? theme.darkAlgorithm : theme.defaultAlgorithm,
    token: {
      colorPrimary: p.brand,
      colorInfo: p.brand,
      colorSuccess: p.ok,
      colorWarning: p.warn,
      colorError: p.bad,
      colorLink: p.brand,
      colorLinkHover: p.brandHover,

      colorBgLayout: p.bg,
      colorBgContainer: p.surface,
      colorBgElevated: p.surface,
      colorBorder: p.sep,
      colorBorderSecondary: p.sep2,
      colorSplit: p.sep2,

      colorText: p.text,
      colorTextSecondary: p.text2,
      colorTextTertiary: p.text3,
      colorTextQuaternary: p.text3,
      colorTextPlaceholder: p.text3,
      colorTextDescription: p.text2,
      colorTextHeading: p.text,

      /**
       * 表头底色、展开行底色都由它派生。设计要求表头**无底色**,
       * 所以给它卡片面色;行 hover 要 --surface2,在 antd-tune.css 里单独盖。
       */
      colorFillAlter: p.surface,
      colorFillSecondary: p.fill2,
      colorFillTertiary: p.fill,
      colorFillQuaternary: p.fill,

      borderRadius: radius.input,
      borderRadiusLG: radius.card,
      borderRadiusSM: 8,
      borderRadiusXS: 6,

      fontFamily: font.sans,
      fontSize: 13,
      fontSizeSM: 12,
      fontSizeLG: 15,
      fontSizeHeading5: 15,
      fontSizeHeading4: 19,

      controlHeight: 36,
      controlHeightSM: 28,
      controlHeightLG: 46,
      controlOutlineWidth: 3,
      controlOutline: p.brandBg,

      boxShadow: p.shadowLg,
      boxShadowSecondary: p.shadowLg,
      boxShadowTertiary: p.shadow,

      wireframe: false,
      motionEaseInOut: 'cubic-bezier(.2,.8,.2,1)',
    },
    components: {
      Layout: {
        colorBgHeader: p.surface,
        colorBgBody: p.bg,
      },
      Menu: {
        colorItemBg: 'transparent',
        colorSubItemBg: 'transparent',
        colorItemBgSelected: p.brand,
        colorItemTextSelected: '#fff',
        colorItemBgHover: p.fill,
        radiusItem: radius.pill,
        itemMarginInline: 12,
      },
      Button: {
        borderRadius: radius.pill,
        borderRadiusSM: radius.pill,
        borderRadiusLG: radius.pill,
        controlHeight: 34,
        controlHeightSM: 28,
        controlHeightLG: 46,
        fontSize: 13,
        fontSizeSM: 12.5,
        fontSizeLG: 15,
        paddingContentHorizontal: 18,
      },
      Tag: {
        borderRadiusSM: radius.pill,
      },
      Modal: {
        borderRadiusLG: radius.sheet,
      },
      Drawer: {
        borderRadiusLG: radius.sheet,
      },
      Segmented: {
        borderRadius: radius.seg,
        borderRadiusSM: 7,
        colorBgLayout: p.fill,
      },
      Tooltip: {
        borderRadius: 10,
      },
      Card: {
        borderRadiusLG: radius.card,
        boxShadowTertiary: p.shadow,
      },
    },
  }
}
