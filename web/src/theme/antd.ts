import type { ThemeConfig } from 'ant-design-vue/es/config-provider/context'
import { color, font, radius, shadowOverlay } from './tokens'

/**
 * 通过 ConfigProvider :theme 下发。
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
 */
export const antdTheme: ThemeConfig = {
  token: {
    colorPrimary: color.brand,
    colorInfo: color.brand,
    colorSuccess: color.success,
    colorWarning: color.warning,
    colorError: color.danger,

    colorBgLayout: color.bgPage,
    colorBgContainer: color.bgSurface,
    colorBorder: color.border,
    colorBorderSecondary: color.borderSubtle,

    colorText: color.text1,
    colorTextSecondary: color.text2,
    colorTextTertiary: color.text3,
    colorTextQuaternary: color.text4,
    colorTextPlaceholder: color.placeholder,

    /**
     * 表头底色、行 hover 底色、展开行底色都由它派生。
     * Table 的 component token 在本版本盖不掉,但这个 alias token 能 ——
     * 一处改到位,比给表格写三条 CSS 规则可靠。
     */
    colorFillAlter: color.bgPage,

    borderRadius: radius.control,
    borderRadiusLG: radius.card,
    borderRadiusSM: radius.control,

    fontFamily: font.sans,
    // 默认 14 在这个密度下太松,整体降一档。
    fontSize: 13,
    fontSizeSM: 12,

    controlHeight: 30,
    controlHeightSM: 24,

    boxShadow: shadowOverlay,
    boxShadowSecondary: shadowOverlay,

    wireframe: false,
  },
  components: {
    Layout: {
      // 白侧栏 + 白顶栏。深色 Sider 与「浅灰底 + 白内容区」的方向直接冲突。
      colorBgHeader: color.bgSurface,
      colorBgBody: color.bgPage,
    },
    Menu: {
      colorItemBg: 'transparent',
      colorSubItemBg: 'transparent',
      // 选中用浅蓝底,不用左侧色条 —— colorActiveBarWidth 默认就是 0。
      colorItemBgSelected: color.brandBg,
      colorItemTextSelected: color.brandHover,
      radiusItem: radius.control,
      itemMarginInline: 8,
    },
  },
}
