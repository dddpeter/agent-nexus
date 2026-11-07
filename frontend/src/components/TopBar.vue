<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { authStatus, logout as doLogout } from '../api/auth'
import { useAgents } from '../stores/agents'
import { useEventStream } from '../stores/events'

const router = useRouter()
const agentsStore = useAgents()
const events = useEventStream()

const showLogout = ref(false)

const topMeta = computed(() => {
  if (!agentsStore.loaded) return ''
  return `${agentsStore.stats.total} agent · ${agentsStore.stats.online} 在线`
})

onMounted(async () => {
  showLogout.value = (await authStatus()).auth_required
  if (!agentsStore.loaded) void agentsStore.load()
})

async function logout(): Promise<void> {
  await doLogout()
  void router.push({ name: 'login' })
}
</script>

<template>
  <header class="topbar">
    <div class="topbar-in">
      <router-link class="brand" to="/"><span class="brand-mark"></span>Nexus</router-link>
      <nav class="nav">
        <router-link to="/" :class="{ on: $route.name === 'agents' || $route.name === 'agent-detail' }">Agent</router-link>
        <router-link to="/agents/new" :class="{ on: $route.name === 'wizard' }">注册</router-link>
        <router-link to="/chat" :class="{ on: $route.name === 'chat' }">私聊</router-link>
        <router-link to="/groups" :class="{ on: $route.name === 'groups' }">群聊</router-link>
        <router-link to="/tasks" :class="{ on: $route.name === 'tasks' }">任务</router-link>
        <router-link to="/events" :class="{ on: $route.name === 'events' }">事件</router-link>
      </nav>
      <div class="top-meta">
        <span
          class="small"
          :title="`SSE ${events.connected ? '已连接' : '重连中'}`"
          :style="{ color: events.connected ? 'var(--ok)' : 'var(--warn)' }"
        >●</span>
        <span style="margin: 0 12px">{{ topMeta }}</span>
        <button v-if="showLogout" class="btn btn-quiet btn-sm" type="button" @click="logout">退出</button>
      </div>
    </div>
  </header>
</template>
