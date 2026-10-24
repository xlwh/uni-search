<template>
  <div>
    <div style="margin-bottom: 16px; display: flex; gap: 8px;">
      <el-select v-model="selectedIndex" filterable placeholder="选择索引" style="width: 200px" @change="loadDocuments">
        <el-option v-for="idx in indexOptions" :key="idx" :label="idx" :value="idx" />
      </el-select>
      <el-button type="primary" @click="showCreateDoc = true">新增文档</el-button>
      <el-button @click="showBulkImport = true">批量导入</el-button>
      <el-button @click="loadDocuments">刷新</el-button>
    </div>

    <el-table :data="documents" stripe size="small" v-loading="loading">
      <el-table-column prop="_id" label="_id" width="200" />
      <el-table-column label="_source" min-width="300">
        <template #default="{ row }">
          <pre style="font-size: 12px; max-height: 60px; overflow: auto;">{{ JSON.stringify(row._source, null, 2) }}</pre>
        </template>
      </el-table-column>
      <el-table-column prop="_score" label="score" width="80" />
      <el-table-column label="操作" width="160" fixed="right">
        <template #default="{ row }">
          <el-button size="small" @click="viewDoc(row)">查看</el-button>
          <el-button size="small" type="danger" @click="deleteDoc(row._id)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- View/Edit Document -->
    <el-drawer v-model="showDocDetail" title="文档详情" size="50%">
      <JsonEditor v-model="docContent" />
      <div style="margin-top: 12px;">
        <el-button type="primary" @click="saveDoc">保存</el-button>
      </div>
    </el-drawer>

    <!-- Create Document -->
    <el-dialog v-model="showCreateDoc" title="新增文档" width="600px">
      <el-form label-position="top">
        <el-form-item label="文档 ID (留空自动生成)">
          <el-input v-model="newDoc.id" placeholder="auto-generated if empty" />
        </el-form-item>
        <el-form-item label="文档内容 (JSON)">
          <JsonEditor v-model="newDoc.body" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreateDoc = false">取消</el-button>
        <el-button type="primary" @click="createDoc">创建</el-button>
      </template>
    </el-dialog>

    <!-- Bulk Import -->
    <el-dialog v-model="showBulkImport" title="批量导入 (NDJSON)" width="700px">
      <el-input type="textarea" v-model="bulkText" :rows="15" placeholder='{"index": {"_id": "1"}}\n{"title": "doc1"}\n{"index": {"_id": "2"}}\n{"title": "doc2"}' />
      <template #footer>
        <el-button @click="showBulkImport = false">取消</el-button>
        <el-button type="primary" @click="doBulkImport" :loading="importing">导入</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import JsonEditor from '@/components/JsonEditor.vue'
import { getCatIndices } from '@/api/indices'
import { search } from '@/api/search'
import { getDocument, indexDocument, indexAutoId, deleteDocument, bulk } from '@/api/documents'

const indexOptions = ref<string[]>([])
const selectedIndex = ref('')
const documents = ref<any[]>([])
const loading = ref(false)
const showDocDetail = ref(false)
const docContent = ref('{}')
const currentDocId = ref('')
const showCreateDoc = ref(false)
const showBulkImport = ref(false)
const bulkText = ref('')
const importing = ref(false)
const newDoc = ref({ id: '', body: '{\n  \n}' })

async function loadIndices() {
  const { data } = await getCatIndices()
  indexOptions.value = (Array.isArray(data) ? data : []).map((i: any) => i.index)
}

async function loadDocuments() {
  if (!selectedIndex.value) return
  loading.value = true
  try {
    const { data } = await search(selectedIndex.value, { query: { match_all: {} }, size: 100 })
    documents.value = data.hits?.hits || []
  } finally {
    loading.value = false
  }
}

function viewDoc(row: any) {
  currentDocId.value = row._id
  docContent.value = JSON.stringify(row._source, null, 2)
  showDocDetail.value = true
}

function requireIndex(): boolean {
  if (!selectedIndex.value) {
    ElMessage.warning('请先选择索引')
    return false
  }
  return true
}

async function saveDoc() {
  if (!requireIndex()) return
  try {
    await indexDocument(selectedIndex.value, currentDocId.value, JSON.parse(docContent.value))
    ElMessage.success('文档已保存')
    showDocDetail.value = false
    await loadDocuments()
  } catch (e: any) {
    if (e instanceof SyntaxError) ElMessage.error('JSON 格式错误')
  }
}

async function createDoc() {
  if (!requireIndex()) return
  try {
    const body = JSON.parse(newDoc.value.body)
    if (newDoc.value.id) {
      await indexDocument(selectedIndex.value, newDoc.value.id, body)
    } else {
      await indexAutoId(selectedIndex.value, body)
    }
    ElMessage.success('文档已创建')
    showCreateDoc.value = false
    newDoc.value = { id: '', body: '{\n  \n}' }
    await loadDocuments()
  } catch (e: any) {
    if (e instanceof SyntaxError) ElMessage.error('JSON 格式错误')
  }
}

async function deleteDoc(id: string) {
  if (!requireIndex()) return
  await ElMessageBox.confirm(`确定删除文档 "${id}"？`, '删除确认', { type: 'warning' })
  await deleteDocument(selectedIndex.value, id)
  ElMessage.success('文档已删除')
  await loadDocuments()
}

async function doBulkImport() {
  if (!bulkText.value.trim() || !selectedIndex.value) {
    ElMessage.warning('请输入 NDJSON 数据并选择索引')
    return
  }
  importing.value = true
  try {
    const lines = bulkText.value.trim().split('\n')
    const ndjson = lines.map((line) => {
      try { return JSON.stringify(JSON.parse(line)) } catch { return line }
    }).join('\n') + '\n'
    await bulk(ndjson)
    ElMessage.success('批量导入完成')
    showBulkImport.value = false
    bulkText.value = ''
    loadDocuments()
  } finally {
    importing.value = false
  }
}

onMounted(loadIndices)
</script>
