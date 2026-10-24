<template>
  <div>
    <el-row :gutter="16">
      <el-col :span="10">
        <el-card shadow="hover">
          <template #header>查询编辑器</template>
          <el-form label-position="top">
            <el-form-item label="索引">
              <el-select v-model="selectedIndex" filterable placeholder="选择索引" style="width: 100%">
                <el-option v-for="idx in indexOptions" :key="idx" :label="idx" :value="idx" />
              </el-select>
            </el-form-item>
            <el-form-item label="搜索模式">
              <el-radio-group v-model="searchMode">
                <el-radio-button value="search">全文搜索</el-radio-button>
                <el-radio-button value="knn">KNN 向量搜索</el-radio-button>
              </el-radio-group>
            </el-form-item>
            <el-form-item label="DSL (JSON)">
              <JsonEditor v-model="queryBody" />
            </el-form-item>
            <el-form-item>
              <el-button type="primary" @click="executeSearch" :loading="searching">执行查询</el-button>
              <el-button @click="loadHistory">历史记录</el-button>
            </el-form-item>
          </el-form>
        </el-card>
      </el-col>
      <el-col :span="14">
        <el-card shadow="hover">
          <template #header>
            <span>查询结果</span>
            <span v-if="result" style="margin-left: 12px; color: #909399; font-size: 13px;">
              命中 {{ result.hits?.total?.value ?? result.hits?.total ?? 0 }} 条，耗时 {{ tookMs }}ms
            </span>
          </template>
          <div v-if="!result" style="text-align: center; color: #909399; padding: 40px;">
            执行查询后在此查看结果
          </div>
          <div v-else>
            <el-collapse v-model="activeHits">
              <el-collapse-item
                v-for="(hit, i) in result.hits?.hits || []"
                :key="hit._id || i"
                :name="hit._id || i"
              >
                <template #title>
                  <span style="font-weight: 600;">{{ hit._id }}</span>
                  <span style="margin-left: 8px; color: #909399;">score: {{ hit._score?.toFixed(4) }}</span>
                </template>
                <pre style="font-size: 13px; overflow-x: auto;">{{ JSON.stringify(hit._source, null, 2) }}</pre>
              </el-collapse-item>
            </el-collapse>
          </div>
        </el-card>
      </el-col>
    </el-row>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import JsonEditor from '@/components/JsonEditor.vue'
import { getCatIndices } from '@/api/indices'
import { search, knnSearch } from '@/api/search'

const indexOptions = ref<string[]>([])
const selectedIndex = ref('')
const searchMode = ref('search')
const queryBody = ref('{\n  "query": {\n    "match_all": {}\n  },\n  "size": 10\n}')
const result = ref<any>(null)
const tookMs = ref(0)
const searching = ref(false)
const activeHits = ref<string[]>([])

async function loadIndices() {
  try {
    const { data } = await getCatIndices()
    indexOptions.value = (Array.isArray(data) ? data : []).map((i: any) => i.index)
  } catch {}
}

async function executeSearch() {
  if (!selectedIndex.value) {
    ElMessage.warning('请选择索引')
    return
  }
  searching.value = true
  try {
    const body = JSON.parse(queryBody.value)
    const { data } = searchMode.value === 'knn'
      ? await knnSearch(selectedIndex.value, body)
      : await search(selectedIndex.value, body)
    result.value = data
    tookMs.value = data.took || 0
    // Save to history
    const history = JSON.parse(localStorage.getItem('searchHistory') || '[]')
    history.unshift({ index: selectedIndex.value, body: queryBody.value, mode: searchMode.value })
    localStorage.setItem('searchHistory', JSON.stringify(history.slice(0, 20)))
  } catch (e: any) {
    if (e instanceof SyntaxError) {
      ElMessage.error('JSON 格式错误: ' + e.message)
    }
  } finally {
    searching.value = false
  }
}

function loadHistory() {
  const history = JSON.parse(localStorage.getItem('searchHistory') || '[]')
  if (history.length === 0) {
    ElMessage.info('暂无历史记录')
    return
  }
  const latest = history[0]
  selectedIndex.value = latest.index
  queryBody.value = latest.body
  searchMode.value = latest.mode
}

onMounted(loadIndices)
</script>
