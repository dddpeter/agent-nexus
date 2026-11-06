import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import { setUnauthorizedHandler } from './api/client'
import './styles/system.css'

const app = createApp(App)
app.use(createPinia())
app.use(router)

// 401 统一出口：会话失效 → 登录页（带回跳）。client 层注入，避免循环依赖。
setUnauthorizedHandler(() => {
  const current = router.currentRoute.value
  if (current.name !== 'login') {
    void router.push({ name: 'login', query: { redirect: current.fullPath } })
  }
})

app.mount('#app')
