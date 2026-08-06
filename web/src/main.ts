import { createApp } from 'vue'
import { createPinia } from 'pinia'
import Antd from 'ant-design-vue'
import 'ant-design-vue/dist/reset.css'

import App from './App.vue'
import { router } from './router'
import './styles/fonts'
// base.css 必须在 reset.css 之后:它带 focus-visible 与夹断工具类。
import './styles/base.css'
// 只补 Token 系统够不到的几项密度,见文件头的说明。
import './styles/antd-tune.css'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.use(Antd)
app.mount('#app')
