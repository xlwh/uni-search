<template>
  <div class="app-layout">
    <aside class="app-sidebar">
      <div class="app-sidebar-header">UniSearch Admin</div>
      <nav class="app-sidebar-menu">
        <router-link
          v-for="route in menuRoutes"
          :key="route.path"
          :to="route.path"
          class="menu-item"
          :class="{ active: isActive(route.path) }"
        >
          <el-icon><component :is="route.meta.icon" /></el-icon>
          <span>{{ route.meta.title }}</span>
        </router-link>
      </nav>
    </aside>
    <main class="app-main">
      <header class="app-header">
        {{ currentTitle }}
      </header>
      <div class="app-content">
        <router-view />
      </div>
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import router from '@/router'

const route = useRoute()
const menuRoutes = router.options.routes

const currentTitle = computed(() => route.meta.title || 'UniSearch Admin')

function isActive(path: string) {
  return route.path === path
}
</script>
