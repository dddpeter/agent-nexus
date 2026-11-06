<script setup lang="ts">
// 登录页：token 换 HttpOnly cookie 会话（TODO#2 闭环）。本地免登模式自动跳转。
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { login } from '../api/auth'
import { ApiError } from '../api/client'

const router = useRouter()
const route = useRoute()

const token = ref('')
const error = ref('')
const busy = ref(false)

onMounted(async () => {
  // 免登模式直接进（守卫已保证 authenticated=false 才会到这）
  document.title = 'Nexus · 登录'
})

async function submit(): Promise<void> {
  if (!token.value.trim() || busy.value) return
  busy.value = true
  error.value = ''
  try {
    await login(token.value.trim())
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
    await router.replace(redirect)
  } catch (e) {
    error.value = e instanceof ApiError && e.status === 401 ? 'token 不正确' : e instanceof Error ? e.message : '登录失败'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <div class="card login-card">
      <div class="login-brand"><span class="brand-mark"></span>Nexus</div>
      <div class="login-sub">Agent Hub 控制台 · 输入访问令牌登录</div>

      <form @submit.prevent="submit">
        <label class="f-label">访问令牌</label>
        <input
          v-model="token"
          class="f-input"
          type="password"
          placeholder="auth_token"
          autocomplete="current-password"
          autofocus
        />
        <div class="callout bad" v-if="error" style="margin-top: 12px">{{ error }}</div>
        <button class="btn btn-primary" type="submit" style="width: 100%; margin-top: 18px" :disabled="busy">
          登录
        </button>
      </form>
    </div>
  </div>
</template>
