<template>
  <div>
    <div style="margin-bottom: 16px; display: flex; gap: 8px; align-items: center;">
      <el-select v-model="selectedMetric" filterable placeholder="选择指标" style="width: 300px">
        <el-option v-for="m in metricNames" :key="m" :label="m" :value="m" />
      </el-select>
      <el-button type="primary" @click="fetchMetrics">查询</el-button>
      <el-button @click="togglePolling">{{ polling ? '停止轮询' : '开始轮询' }}</el-button>
    </div>

    <el-card v-if="chartData.length > 0" shadow="hover" style="margin-bottom: 16px;">
      <template #header>{{ selectedMetric }} - 实时图表</template>
      <v-chart :option="chartOption" style="height: 300px;" autoresize />
    </el-card>

    <el-card shadow="hover">
      <template #header>指标列表</template>
      <el-table :data="metrics" stripe size="small" max-height="500">
        <el-table-column prop="name" label="指标名" min-width="250" />
        <el-table-column prop="type" label="类型" width="80" />
        <el-table-column label="标签" min-width="200">
          <template #default="{ row }">
            <span v-for="(v, k) in row.labels" :key="k" style="margin-right: 8px; font-size: 12px;">{{ k }}={{ v }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="value" label="当前值" width="120">
          <template #default="{ row }">
            {{ row.value.toFixed(4) }}
          </template>
        </el-table-column>
      </el-table>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { use } from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent } from 'echarts/components'
import VChart from 'vue-echarts'
import { getMetrics } from '@/api/metrics'

use([CanvasRenderer, LineChart, GridComponent, TooltipComponent, LegendComponent])

const metrics = ref<any[]>([])
const metricNames = ref<string[]>([])
const selectedMetric = ref('')
const polling = ref(false)
const chartData = ref<{ time: string; value: number }[]>([])
let timer: number | null = null

const chartOption = computed(() => ({
  tooltip: { trigger: 'axis' },
  xAxis: { type: 'category', data: chartData.value.map((d) => d.time) },
  yAxis: { type: 'value' },
  series: [{ data: chartData.value.map((d) => d.value), type: 'line', smooth: true, areaStyle: {} }],
}))

async function fetchMetrics() {
  const { data } = await getMetrics()
  metrics.value = data.metrics || []
  // Extract unique metric names
  const names = new Set<string>()
  metrics.value.forEach((m) => names.add(m.name))
  metricNames.value = Array.from(names).sort()

  if (selectedMetric.value && chartData.value.length < 60) {
    const entry = metrics.value.find((m) => m.name === selectedMetric.value)
    if (entry) {
      chartData.value.push({
        time: new Date().toLocaleTimeString(),
        value: entry.value,
      })
    }
  } else if (chartData.value.length >= 60) {
    chartData.value.shift()
  }
}

function togglePolling() {
  if (polling.value) {
    if (timer) clearInterval(timer)
    polling.value = false
  } else {
    if (!selectedMetric.value) return
    polling.value = true
    chartData.value = []
    timer = window.setInterval(fetchMetrics, 2000)
  }
}

onMounted(fetchMetrics)
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>
