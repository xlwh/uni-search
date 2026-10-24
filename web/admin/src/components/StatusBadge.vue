<template>
  <span :class="['status-badge', statusClass]">{{ statusText }}</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ status: string }>()

const statusClass = computed(() => {
  const s = props.status?.toLowerCase()
  if (s === 'green' || s === 'healthy') return 'status-green'
  if (s === 'yellow') return 'status-yellow'
  if (s === 'red') return 'status-red'
  return ''
})

const statusText = computed(() => props.status || 'unknown')
</script>

<style scoped>
.status-badge {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 4px;
  font-size: 12px;
  font-weight: 600;
  background: #f0f0f0;
}
.status-green { color: #67c23a; background: #f0f9eb; }
.status-yellow { color: #e6a23c; background: #fdf6ec; }
.status-red { color: #f56c6c; background: #fef0f0; }
</style>
