<template>
  <div class="metric-card">
    <div class="metric-card-title">{{ title }}</div>
    <div class="metric-card-value">{{ value }}</div>
    <div v-if="subtitle" class="metric-card-subtitle">{{ subtitle }}</div>
  </div>
</template>

<script setup lang="ts">
defineProps<{
  title: string
  value: string | number
  subtitle?: string
}>()
</script>

<style scoped>
.metric-card-subtitle {
  font-size: 12px;
  color: #c0c4cc;
  margin-top: 4px;
}
</style>
