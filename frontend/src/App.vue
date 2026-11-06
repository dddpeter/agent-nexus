<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import TopBar from './components/TopBar.vue'
import { useAgents } from './stores/agents'

const route = useRoute()
const agentsStore = useAgents()

const bare = computed(() => route.meta.bare === true)

onMounted(() => {
  if (!bare.value) {
    agentsStore.load()
    agentsStore.bindEvents()
  }
})
</script>

<template>
  <TopBar v-if="!bare" />
  <main :class="bare ? '' : 'wrap'">
    <router-view />
  </main>
  <footer v-if="!bare" class="wrap" style="padding-top: 0">
    <div
      class="small muted"
      style="border-top: 1px solid var(--line); padding-top: 16px; display: flex; justify-content: space-between"
    >
      <span>Nexus Agent Hub</span>
      <span>Vue 3 SPA · SQLite WAL</span>
    </div>
  </footer>
</template>
