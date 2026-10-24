<template>
  <div>
    <el-tabs v-model="activeTab">
      <el-tab-pane label="快照仓库" name="repos">
        <div style="margin-bottom: 12px; display: flex; gap: 8px;">
          <el-button type="primary" @click="showCreateRepo = true">创建仓库</el-button>
          <el-button @click="loadRepos">刷新</el-button>
        </div>
        <el-table :data="repos" stripe size="small" v-loading="loadingRepos">
          <el-table-column prop="name" label="仓库名" min-width="150" />
          <el-table-column prop="type" label="类型" width="100" />
          <el-table-column prop="status" label="状态" width="100" />
          <el-table-column label="操作" width="200">
            <template #default="{ row }">
              <el-button size="small" @click="loadSnapshots(row.name)">查看快照</el-button>
              <el-button size="small" type="danger" @click="deleteRepo(row.name)">删除仓库</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <el-tab-pane label="快照列表" name="snapshots">
        <div style="margin-bottom: 12px;">
          <el-select v-model="selectedRepo" filterable placeholder="选择仓库" style="width: 200px" @change="loadSnapshots(selectedRepo)">
            <el-option v-for="r in repos" :key="r.name" :label="r.name" :value="r.name" />
          </el-select>
          <el-button type="primary" @click="showCreateSnapshot = true" style="margin-left: 8px;">创建快照</el-button>
          <el-button @click="loadSnapshots(selectedRepo)">刷新</el-button>
        </div>
        <el-table :data="snapshots" stripe size="small" v-loading="loadingSnapshots">
          <el-table-column prop="snapshot" label="快照名" min-width="150" />
          <el-table-column prop="state" label="状态" width="100" />
          <el-table-column prop="start_time" label="开始时间" width="180" />
          <el-table-column prop="end_time" label="结束时间" width="180" />
          <el-table-column label="操作" width="200">
            <template #default="{ row }">
              <el-button size="small" @click="restoreSnapshot(row.snapshot)">恢复</el-button>
              <el-button size="small" type="danger" @click="deleteSnapshot(row.snapshot)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>
    </el-tabs>

    <el-dialog v-model="showCreateRepo" title="创建快照仓库" width="500px">
      <el-form label-position="top">
        <el-form-item label="仓库名称">
          <el-input v-model="repoForm.name" placeholder="my-repo" />
        </el-form-item>
        <el-form-item label="仓库类型">
          <el-select v-model="repoForm.type" style="width: 100%">
            <el-option label="fs (本地文件系统)" value="fs" />
            <el-option label="s3" value="s3" />
          </el-select>
        </el-form-item>
        <el-form-item label="仓库配置 (JSON)">
          <JsonEditor v-model="repoForm.settings" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreateRepo = false">取消</el-button>
        <el-button type="primary" @click="createRepo">创建</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="showCreateSnapshot" title="创建快照" width="400px">
      <el-form label-position="top">
        <el-form-item label="快照名称">
          <el-input v-model="snapshotForm.name" placeholder="snap_1" />
        </el-form-item>
        <el-form-item label="仓库">
          <el-select v-model="snapshotForm.repo" style="width: 100%">
            <el-option v-for="r in repos" :key="r.name" :label="r.name" :value="r.name" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreateSnapshot = false">取消</el-button>
        <el-button type="primary" @click="createSnapshotAction">创建</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import JsonEditor from '@/components/JsonEditor.vue'
import * as snapApi from '@/api/snapshots'

const activeTab = ref('repos')
const repos = ref<any[]>([])
const snapshots = ref<any[]>([])
const selectedRepo = ref('')
const loadingRepos = ref(false)
const loadingSnapshots = ref(false)
const showCreateRepo = ref(false)
const showCreateSnapshot = ref(false)
const repoForm = ref({ name: '', type: 'fs', settings: '{\n  "location": "/tmp/snapshots"\n}' })
const snapshotForm = ref({ name: '', repo: '' })

async function loadRepos() {
  loadingRepos.value = true
  try {
    const { data } = await snapApi.listRepos()
    if (Array.isArray(data)) {
      repos.value = data
    } else if (data?.results) {
      repos.value = Object.entries(data.results).map(([name, info]: [string, any]) => ({
        name,
        type: info?.type || '',
        status: info?.status || 'available',
      }))
    } else {
      repos.value = []
    }
  } catch (e) {
    console.error('加载仓库列表失败', e)
    repos.value = []
  } finally {
    loadingRepos.value = false
  }
}

async function loadSnapshots(repo: string) {
  if (!repo) return
  activeTab.value = 'snapshots'
  selectedRepo.value = repo
  loadingSnapshots.value = true
  try {
    const { data } = await snapApi.listSnapshots(repo)
    snapshots.value = data?.snapshots || []
  } catch (e) {
    console.error('加载快照列表失败', e)
    snapshots.value = []
  } finally {
    loadingSnapshots.value = false
  }
}

async function createRepo() {
  const settings = JSON.parse(repoForm.value.settings)
  await snapApi.createRepo(repoForm.value.name, { type: repoForm.value.type, settings })
  ElMessage.success('仓库已创建')
  showCreateRepo.value = false
  await loadRepos()
}

async function deleteRepo(name: string) {
  await ElMessageBox.confirm(`确定删除仓库 "${name}"？`, '删除确认', { type: 'warning' })
  await snapApi.deleteRepo(name)
  ElMessage.success('仓库已删除')
  await loadRepos()
}

async function createSnapshotAction() {
  await snapApi.createSnapshot(snapshotForm.value.repo, snapshotForm.value.name)
  ElMessage.success('快照创建已提交')
  showCreateSnapshot.value = false
  loadSnapshots(snapshotForm.value.repo)
}

async function restoreSnapshot(name: string) {
  await ElMessageBox.confirm(`确定从快照 "${name}" 恢复？`, '恢复确认', { type: 'warning' })
  await snapApi.restoreSnapshot(selectedRepo.value, name)
  ElMessage.success('快照恢复已提交')
}

async function deleteSnapshot(name: string) {
  await ElMessageBox.confirm(`确定删除快照 "${name}"？`, '删除确认', { type: 'warning' })
  await snapApi.deleteSnapshot(selectedRepo.value, name)
  ElMessage.success('快照已删除')
  loadSnapshots(selectedRepo.value)
}

onMounted(loadRepos)
</script>
