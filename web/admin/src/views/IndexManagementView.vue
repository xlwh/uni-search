<template>
  <div>
    <div style="margin-bottom: 16px; display: flex; gap: 8px;">
      <el-input v-model="newIndex.name" placeholder="索引名称" style="width: 200px" />
      <el-button type="primary" @click="openCreateDialog">创建索引</el-button>
      <el-button @click="loadIndices">刷新</el-button>
    </div>

    <el-table :data="indices" stripe size="small" style="width: 100%" v-loading="loading">
      <el-table-column prop="index" label="索引名" min-width="150" />
      <el-table-column prop="health" label="健康" width="80">
        <template #default="{ row }">
          <StatusBadge :status="row.health" />
        </template>
      </el-table-column>
      <el-table-column prop="status" label="状态" width="80" />
      <el-table-column prop="docs_count" label="文档数" width="100" />
      <el-table-column prop="store_size" label="存储" width="100" />
      <el-table-column label="操作" width="380" fixed="right">
        <template #default="{ row }">
          <el-button size="small" @click="viewMapping(row.index)">Mapping</el-button>
          <el-button size="small" @click="viewSettings(row.index)">Settings</el-button>
          <el-button size="small" @click="refreshIndex(row.index)">刷新</el-button>
          <el-dropdown @command="(cmd: string) => handleAction(cmd, row.index)" trigger="click">
            <el-button size="small">
              更多<el-icon class="el-icon--right"><ArrowDown /></el-icon>
            </el-button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="flush">Flush</el-dropdown-item>
                <el-dropdown-item command="forcemerge">Force Merge</el-dropdown-item>
                <el-dropdown-item command="open" :disabled="row.status === 'open'">打开索引</el-dropdown-item>
                <el-dropdown-item command="close" :disabled="row.status === 'close'">关闭索引</el-dropdown-item>
                <el-dropdown-item command="segments" divided>查看 Segments</el-dropdown-item>
                <el-dropdown-item command="stats">查看 Stats</el-dropdown-item>
                <el-dropdown-item command="aliases">查看 Aliases</el-dropdown-item>
                <el-dropdown-item command="delete" divided>
                  <span style="color: var(--el-color-danger)">删除索引</span>
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </template>
      </el-table-column>
    </el-table>

    <!-- Create Index Dialog -->
    <el-dialog v-model="showCreateDialog" title="创建索引" width="750px" @closed="resetCreateForm">
      <el-form label-position="top">
        <el-form-item label="索引名称">
          <el-input v-model="newIndex.name" placeholder="my-index" />
        </el-form-item>

        <el-form-item>
          <el-radio-group v-model="createMode">
            <el-radio-button value="form">表单模式</el-radio-button>
            <el-radio-button value="json">JSON 模式</el-radio-button>
          </el-radio-group>
        </el-form-item>

        <!-- Form Mode -->
        <template v-if="createMode === 'form'">
          <!-- Settings -->
          <el-form-item label="Settings">
            <el-row :gutter="12">
              <el-col :span="8">
                <el-input-number v-model="formSettings.shards" :min="1" :max="10" controls-position="right" style="width: 100%">
                  <template #prefix>分片数</template>
                </el-input-number>
              </el-col>
              <el-col :span="8">
                <el-input-number v-model="formSettings.replicas" :min="0" :max="5" controls-position="right" style="width: 100%">
                  <template #prefix>副本数</template>
                </el-input-number>
              </el-col>
              <el-col :span="8">
                <el-input v-model="formSettings.refreshInterval" placeholder="1s">
                  <template #prefix>刷新间隔</template>
                </el-input>
              </el-col>
            </el-row>
          </el-form-item>

          <!-- Mappings Fields -->
          <el-form-item label="字段定义 (Mappings)">
            <div style="width: 100%;">
              <el-table :data="formFields" border size="small" style="width: 100%">
                <el-table-column label="字段名" width="160">
                  <template #default="{ row }">
                    <el-input v-model="row.name" placeholder="field_name" size="small" />
                  </template>
                </el-table-column>
                <el-table-column label="类型" width="140">
                  <template #default="{ row }">
                    <el-select v-model="row.type" size="small" style="width: 100%" @change="onTypeChange(row)">
                      <el-option v-for="t in fieldTypes" :key="t" :label="t" :value="t" />
                    </el-select>
                  </template>
                </el-table-column>
                <el-table-column label="参数" min-width="200">
                  <template #default="{ row }">
                    <template v-if="row.type === 'dense_vector'">
                      <el-row :gutter="8">
                        <el-col :span="12">
                          <el-input-number v-model="row.dimensions" :min="1" size="small" placeholder="维度" style="width: 100%" />
                        </el-col>
                        <el-col :span="12">
                          <el-select v-model="row.similarity" size="small" style="width: 100%">
                            <el-option label="cosine" value="cosine" />
                            <el-option label="dot_product" value="dot_product" />
                            <el-option label="l2_norm" value="l2_norm" />
                          </el-select>
                        </el-col>
                      </el-row>
                    </template>
                    <template v-else-if="row.type === 'text' || row.type === 'keyword'">
                      <el-input v-model="row.analyzer" size="small" placeholder="分析器 (可选, 如 standard/ik)" />
                    </template>
                    <template v-else-if="row.type === 'date'">
                      <el-input v-model="row.format" size="small" placeholder="格式 (如 yyyy-MM-dd)" />
                    </template>
                    <template v-else>
                      <span style="color: #c0c4cc; font-size: 12px;">无额外参数</span>
                    </template>
                  </template>
                </el-table-column>
                <el-table-column label="操作" width="60" align="center">
                  <template #default="{ $index }">
                    <el-button size="small" type="danger" text @click="formFields.splice($index, 1)">
                      <el-icon><Delete /></el-icon>
                    </el-button>
                  </template>
                </el-table-column>
              </el-table>
              <el-button size="small" type="primary" plain style="margin-top: 8px" @click="addField">
                <el-icon style="margin-right: 4px"><Plus /></el-icon>添加字段
              </el-button>
            </div>
          </el-form-item>
        </template>

        <!-- JSON Mode -->
        <template v-else>
          <el-form-item label="Mappings (JSON)">
            <JsonEditor v-model="newIndex.mappings" />
          </el-form-item>
          <el-form-item label="Settings (JSON)">
            <JsonEditor v-model="newIndex.settings" />
          </el-form-item>
        </template>
      </el-form>
      <template #footer>
        <el-button @click="showCreateDialog = false">取消</el-button>
        <el-button type="primary" @click="createIndex" :loading="creating">创建</el-button>
      </template>
    </el-dialog>

    <!-- Detail Drawer (Mapping/Settings/Segments/Stats/Aliases) -->
    <el-drawer v-model="showDetail" :title="detailTitle" size="60%">
      <JsonEditor :model-value="detailContent" read-only />
    </el-drawer>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessageBox, ElMessage } from 'element-plus'
import { ArrowDown, Delete, Plus } from '@element-plus/icons-vue'
import StatusBadge from '@/components/StatusBadge.vue'
import JsonEditor from '@/components/JsonEditor.vue'
import {
  getCatIndices, createIndex as apiCreateIndex, deleteIndex as apiDeleteIndex,
  getMapping, getSettings, refreshIndex as apiRefreshIndex,
  flushIndex as apiFlushIndex, forceMergeIndex as apiForceMergeIndex,
  openIndex as apiOpenIndex, closeIndex as apiCloseIndex,
  getSegments, getIndexStats, getAliases,
} from '@/api/indices'

const fieldTypes = ['text', 'keyword', 'long', 'integer', 'double', 'float', 'boolean', 'date', 'ip', 'dense_vector', 'object']

interface FormField {
  name: string
  type: string
  dimensions: number | undefined
  similarity: string
  analyzer: string
  format: string
}

const indices = ref<any[]>([])
const loading = ref(false)
const showCreateDialog = ref(false)
const creating = ref(false)
const showDetail = ref(false)
const detailTitle = ref('')
const detailContent = ref('')
const createMode = ref<'form' | 'json'>('form')

const newIndex = ref({
  name: '',
  mappings: '{\n  "properties": {}\n}',
  settings: '{}',
})

const formSettings = ref({
  shards: 1,
  replicas: 0,
  refreshInterval: '1s',
})

const formFields = ref<FormField[]>([])

function addField() {
  formFields.value.push({
    name: '',
    type: 'text',
    dimensions: undefined,
    similarity: 'cosine',
    analyzer: '',
    format: '',
  })
}

function onTypeChange(row: FormField) {
  if (row.type === 'dense_vector') {
    if (!row.dimensions) row.dimensions = 128
    if (!row.similarity) row.similarity = 'cosine'
  }
}

function resetCreateForm() {
  newIndex.value = { name: '', mappings: '{\n  "properties": {}\n}', settings: '{}' }
  formSettings.value = { shards: 1, replicas: 0, refreshInterval: '1s' }
  formFields.value = []
  createMode.value = 'form'
}

function openCreateDialog() {
  resetCreateForm()
  showCreateDialog.value = true
}

function buildBodyFromForm(): any {
  const body: any = {}

  // Settings
  const settings: any = {
    number_of_shards: formSettings.value.shards,
    number_of_replicas: formSettings.value.replicas,
  }
  if (formSettings.value.refreshInterval) {
    settings.refresh_interval = formSettings.value.refreshInterval
  }
  body.settings = settings

  // Mappings
  const properties: any = {}
  for (const f of formFields.value) {
    if (!f.name) continue
    const field: any = { type: f.type }
    if (f.type === 'dense_vector') {
      field.dimensions = f.dimensions || 128
      field.similarity = f.similarity || 'cosine'
    } else if ((f.type === 'text' || f.type === 'keyword') && f.analyzer) {
      field.analyzer = f.analyzer
    } else if (f.type === 'date' && f.format) {
      field.format = f.format
    }
    properties[f.name] = field
  }
  body.mappings = { properties }
  return body
}

async function createIndex() {
  if (!newIndex.value.name) {
    ElMessage.warning('请输入索引名称')
    return
  }
  creating.value = true
  try {
    let body: any
    if (createMode.value === 'form') {
      if (formFields.value.length === 0) {
        ElMessage.warning('请至少添加一个字段')
        creating.value = false
        return
      }
      if (formFields.value.some(f => !f.name)) {
        ElMessage.warning('字段名不能为空')
        creating.value = false
        return
      }
      body = buildBodyFromForm()
    } else {
      body = {}
      try { body.mappings = JSON.parse(newIndex.value.mappings) } catch {}
      try { body.settings = JSON.parse(newIndex.value.settings) } catch {}
    }
    await apiCreateIndex(newIndex.value.name, body)
    ElMessage.success('索引创建成功')
    showCreateDialog.value = false
    await loadIndices()
  } finally {
    creating.value = false
  }
}

async function loadIndices() {
  loading.value = true
  try {
    const { data } = await getCatIndices()
    indices.value = Array.isArray(data) ? data : []
  } catch (e) {
    console.error('加载索引列表失败', e)
  } finally {
    loading.value = false
  }
}

async function handleAction(cmd: string, index: string) {
  switch (cmd) {
    case 'flush':
      await apiFlushIndex(index)
      ElMessage.success('Flush 完成')
      await loadIndices()
      break
    case 'forcemerge':
      await ElMessageBox.confirm(`确定对索引 "${index}" 执行 Force Merge？`, 'Force Merge', { type: 'warning' })
      await apiForceMergeIndex(index)
      ElMessage.success('Force Merge 已提交')
      await loadIndices()
      break
    case 'open':
      await apiOpenIndex(index)
      ElMessage.success('索引已打开')
      await loadIndices()
      break
    case 'close':
      await ElMessageBox.confirm(`确定关闭索引 "${index}"？关闭后无法搜索和写入。`, '关闭索引', { type: 'warning' })
      await apiCloseIndex(index)
      ElMessage.success('索引已关闭')
      await loadIndices()
      break
    case 'segments':
      await showDetailData(index, 'Segments', getSegments)
      break
    case 'stats':
      await showDetailData(index, 'Stats', getIndexStats)
      break
    case 'aliases':
      await showDetailData(index, 'Aliases', getAliases)
      break
    case 'delete':
      await confirmDelete(index)
      break
  }
}

async function showDetailData(index: string, label: string, fn: (idx: string) => Promise<any>) {
  const { data } = await fn(index)
  detailTitle.value = `${index} - ${label}`
  detailContent.value = JSON.stringify(data, null, 2)
  showDetail.value = true
}

async function confirmDelete(name: string) {
  await ElMessageBox.confirm(`确定删除索引 "${name}"？此操作不可逆。`, '删除确认', { type: 'warning' })
  await apiDeleteIndex(name)
  ElMessage.success('索引已删除')
  await loadIndices()
}

async function viewMapping(index: string) {
  const { data } = await getMapping(index)
  detailTitle.value = `${index} - Mapping`
  detailContent.value = JSON.stringify(data, null, 2)
  showDetail.value = true
}

async function viewSettings(index: string) {
  const { data } = await getSettings(index)
  detailTitle.value = `${index} - Settings`
  detailContent.value = JSON.stringify(data, null, 2)
  showDetail.value = true
}

async function refreshIndex(index: string) {
  await apiRefreshIndex(index)
  ElMessage.success('索引已刷新')
}

onMounted(loadIndices)
</script>
