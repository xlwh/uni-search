<template>
  <div>
    <el-tabs v-model="activeTab">
      <!-- Synonyms -->
      <el-tab-pane label="同义词集合" name="synonyms">
        <div style="margin-bottom: 12px;">
          <el-button type="primary" @click="showCreateSyn = true">创建同义词集</el-button>
          <el-button @click="loadSynonyms">刷新</el-button>
        </div>
        <el-table :data="synonymSets" stripe size="small">
          <el-table-column prop="id" label="ID" min-width="150" />
          <el-table-column label="操作" width="160">
            <template #default="{ row }">
              <el-button size="small" @click="viewSynonyms(row.id)">查看</el-button>
              <el-button size="small" type="danger" @click="deleteSynonyms(row.id)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- Ingest Pipeline -->
      <el-tab-pane label="Ingest Pipeline" name="pipelines">
        <div style="margin-bottom: 12px;">
          <el-button type="primary" @click="showCreatePipe = true">创建管道</el-button>
          <el-button @click="loadPipelines">刷新</el-button>
        </div>
        <el-table :data="pipelines" stripe size="small">
          <el-table-column prop="name" label="管道 ID" min-width="200" />
          <el-table-column label="操作" width="220">
            <template #default="{ row }">
              <el-button size="small" @click="viewPipeline(row.name)">查看</el-button>
              <el-button size="small" @click="deletePipeline(row.name)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- Stored Scripts -->
      <el-tab-pane label="存储脚本" name="scripts">
        <div style="margin-bottom: 12px;">
          <el-input v-model="scriptId" placeholder="脚本 ID" style="width: 200px; margin-right: 8px;" />
          <el-button type="primary" @click="viewScript">查看脚本</el-button>
          <el-button @click="showCreateScript = true">创建脚本</el-button>
        </div>
        <el-drawer v-model="showScriptDetail" title="脚本内容" size="50%">
          <pre style="font-size: 13px;">{{ scriptContent }}</pre>
        </el-drawer>
      </el-tab-pane>
    </el-tabs>

    <!-- Create Synonyms -->
    <el-dialog v-model="showCreateSyn" title="创建同义词集" width="500px">
      <el-form label-position="top">
        <el-form-item label="同义词集 ID">
          <el-input v-model="synForm.id" placeholder="my-synonyms" />
        </el-form-item>
        <el-form-item label="同义词规则 (JSON)">
          <JsonEditor v-model="synForm.body" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreateSyn = false">取消</el-button>
        <el-button type="primary" @click="createSynonyms">创建</el-button>
      </template>
    </el-dialog>

    <!-- Create Pipeline -->
    <el-dialog v-model="showCreatePipe" title="创建 Pipeline" width="600px">
      <el-form label-position="top">
        <el-form-item label="管道 ID">
          <el-input v-model="pipeForm.id" placeholder="my-pipeline" />
        </el-form-item>
        <el-form-item label="管道定义 (JSON)">
          <JsonEditor v-model="pipeForm.body" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreatePipe = false">取消</el-button>
        <el-button type="primary" @click="createPipeline">创建</el-button>
      </template>
    </el-dialog>

    <!-- Create Script -->
    <el-dialog v-model="showCreateScript" title="创建脚本" width="600px">
      <el-form label-position="top">
        <el-form-item label="脚本 ID">
          <el-input v-model="scriptForm.id" placeholder="my-script" />
        </el-form-item>
        <el-form-item label="脚本内容 (JSON)">
          <JsonEditor v-model="scriptForm.body" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreateScript = false">取消</el-button>
        <el-button type="primary" @click="createScript">创建</el-button>
      </template>
    </el-dialog>

    <!-- Detail Drawer -->
    <el-drawer v-model="showDetail" :title="detailTitle" size="50%">
      <pre style="font-size: 13px;">{{ detailContent }}</pre>
    </el-drawer>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import JsonEditor from '@/components/JsonEditor.vue'
import * as synApi from '@/api/synonyms'
import * as pipeApi from '@/api/pipelines'
import * as scriptApi from '@/api/scripts'

const activeTab = ref('synonyms')
const synonymSets = ref<any[]>([])
const pipelines = ref<any[]>([])
const showCreateSyn = ref(false)
const showCreatePipe = ref(false)
const showCreateScript = ref(false)
const showDetail = ref(false)
const showScriptDetail = ref(false)
const detailTitle = ref('')
const detailContent = ref('')
const scriptId = ref('')
const scriptContent = ref('')

const synForm = ref({ id: '', body: '{\n  "synonyms_set": [\n    ["happy", "glad", "joyful"]\n  ]\n}' })
const pipeForm = ref({ id: '', body: '{\n  "processors": []\n}' })
const scriptForm = ref({ id: '', body: '{\n  "script": {\n    "lang": "painless",\n    "source": "doc[\'field\'].value * 2"\n  }\n}' })

async function loadSynonyms() {
  const { data } = await synApi.getSynonyms()
  synonymSets.value = Array.isArray(data) ? data : []
}

async function loadPipelines() {
  const { data } = await pipeApi.getPipelines()
  pipelines.value = Object.entries(data || {}).map(([name, def]) => ({ name, def }))
}

async function createSynonyms() {
  await synApi.putSynonyms(synForm.value.id, JSON.parse(synForm.value.body))
  ElMessage.success('同义词集已创建')
  showCreateSyn.value = false
  loadSynonyms()
}

async function viewSynonyms(id: string) {
  const { data } = await synApi.getSynonymSet(id)
  detailTitle.value = `同义词集: ${id}`
  detailContent.value = JSON.stringify(data, null, 2)
  showDetail.value = true
}

async function deleteSynonyms(id: string) {
  await ElMessageBox.confirm(`确定删除同义词集 "${id}"？`, '删除确认', { type: 'warning' })
  await synApi.deleteSynonyms(id)
  ElMessage.success('同义词集已删除')
  loadSynonyms()
}

async function createPipeline() {
  await pipeApi.putPipeline(pipeForm.value.id, JSON.parse(pipeForm.value.body))
  ElMessage.success('管道已创建')
  showCreatePipe.value = false
  loadPipelines()
}

async function viewPipeline(name: string) {
  const { data } = await pipeApi.getPipeline(name)
  detailTitle.value = `Pipeline: ${name}`
  detailContent.value = JSON.stringify(data, null, 2)
  showDetail.value = true
}

async function deletePipeline(name: string) {
  await ElMessageBox.confirm(`确定删除管道 "${name}"？`, '删除确认', { type: 'warning' })
  await pipeApi.deletePipeline(name)
  ElMessage.success('管道已删除')
  loadPipelines()
}

async function viewScript() {
  if (!scriptId.value) return
  const { data } = await scriptApi.getScript(scriptId.value)
  scriptContent.value = JSON.stringify(data, null, 2)
  showScriptDetail.value = true
}

async function createScript() {
  await scriptApi.putScript(scriptForm.value.id, JSON.parse(scriptForm.value.body))
  ElMessage.success('脚本已创建')
  showCreateScript.value = false
}

onMounted(() => {
  loadSynonyms()
  loadPipelines()
})
</script>
