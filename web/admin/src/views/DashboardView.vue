<template>
  <div>
    <el-row :gutter="16" style="margin-bottom: 16px">
      <el-col :span="6">
        <MetricCard title="集群状态" :value="health?.status || 'loading...'" />
      </el-col>
      <el-col :span="6">
        <MetricCard title="节点数" :value="health?.number_of_nodes || 0" />
      </el-col>
      <el-col :span="6">
        <MetricCard title="索引数" :value="dashboard?.index_count || 0" />
      </el-col>
      <el-col :span="6">
        <MetricCard title="文档总数" :value="dashboard?.doc_count || 0" />
      </el-col>
    </el-row>

    <el-row :gutter="16" style="margin-bottom: 16px">
      <el-col :span="6">
        <MetricCard title="活跃分片" :value="health?.active_shards || 0" />
      </el-col>
      <el-col :span="6">
        <MetricCard title="主分片" :value="health?.active_primary_shards || 0" />
      </el-col>
      <el-col :span="6">
        <MetricCard title="未分配分片" :value="health?.unassigned_shards || 0" />
      </el-col>
      <el-col :span="6">
        <MetricCard title="租户数" :value="dashboard?.tenant_count || 0" />
      </el-col>
    </el-row>

    <el-card shadow="hover">
      <template #header>索引列表</template>
      <el-table :data="dashboard?.indices || []" stripe size="small" style="width: 100%">
        <el-table-column prop="index" label="索引名" min-width="150" />
        <el-table-column prop="health" label="健康" width="80">
          <template #default="{ row }">
            <StatusBadge :status="row.health" />
          </template>
        </el-table-column>
        <el-table-column prop="status" label="状态" width="80" />
        <el-table-column prop="docs_count" label="文档数" width="100" />
        <el-table-column prop="store_size" label="存储大小" width="100" />
      </el-table>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import MetricCard from '@/components/MetricCard.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import { getClusterHealth } from '@/api/cluster'
import { getDashboard } from '@/api/metrics'
import type { ClusterHealth } from '@/types'

const health = ref<ClusterHealth | null>(null)
const dashboard = ref<any>(null)
let timer: number

async function refresh() {
  try {
    const [h, d] = await Promise.all([getClusterHealth(), getDashboard()])
    health.value = h.data
    dashboard.value = d.data
  } catch { /* handled by interceptor */ }
}

onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 5000)
})
onUnmounted(() => clearInterval(timer))
</script>
