import { createApp } from 'vue'
import { createPinia } from 'pinia'
import Antd, { message } from 'ant-design-vue'
import 'ant-design-vue/dist/reset.css'

import App from './App.vue'
import { router } from './router'
import './styles/fonts'
// 令牌先于一切:base.css 与 antd-tune.css 里全是 var(--xxx)。
import './styles/tokens.css'
// base.css 必须在 reset.css 之后:它带 focus-visible 与夹断工具类。
import './styles/base.css'
// 只补 Token 系统够不到的几项,见文件头的说明。
import './styles/antd-tune.css'

// 吐司改成顶部居中的胶囊横幅(样式在 antd-tune.css),3.2s 后消失。
message.config({ top: '14px', duration: 3.2, maxCount: 3 })

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.use(Antd)
app.mount('#app')
