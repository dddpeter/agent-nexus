import { createRouter, createWebHistory } from 'vue-router'
import { authStatus } from '../api/auth'
import AgentListPage from '../pages/AgentListPage.vue'
import WizardPage from '../pages/WizardPage.vue'
import AgentDetailPage from '../pages/AgentDetailPage.vue'
import ChatPage from '../pages/ChatPage.vue'
import LoginPage from '../pages/LoginPage.vue'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'agents', component: AgentListPage },
    { path: '/agents/new', name: 'wizard', component: WizardPage },
    { path: '/agents/:id', name: 'agent-detail', component: AgentDetailPage },
    { path: '/chat', name: 'chat', component: ChatPage },
    { path: '/login', name: 'login', component: LoginPage, meta: { bare: true } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// 路由守卫：cookie 会话校验（本地免登模式 authenticated=true 直接放行）。
// authStatus() 带缓存（登录/登出后由 auth.ts invalidate），导航不会重复请求。
router.beforeEach(async (to) => {
  const s = await authStatus()
  if (to.name === 'login') return s.authenticated ? { path: '/' } : true
  return s.authenticated ? true : { name: 'login', query: { redirect: to.fullPath } }
})

export default router
