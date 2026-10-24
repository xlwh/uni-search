<template>
  <div>
    <div style="margin-bottom: 16px;">
      <el-button type="primary" @click="showCreate = true">创建租户</el-button>
      <el-button @click="loadTenants">刷新</el-button>
    </div>

    <el-table :data="tenants" stripe size="small" v-loading="loading">
      <el-table-column prop="id" label="租户 ID" min-width="150" />
      <el-table-column label="存储配额" width="180">
        <template #default="{ row }">
          <el-progress :percentage="storagePercent(row)" :format="() => formatBytes(row.storage_used) + ' / ' + formatBytes(row.storage_bytes)" />
        </template>
      </el-table-column>
      <el-table-column label="向量配额" width="180">
        <template #default="{ row }">
          <el-progress :percentage="vectorPercent(row)" :format="() => row.vectors_used + ' / ' + row.vector_count" />
        </template>
      </el-table-column>
      <el-table-column prop="query_qps" label="QPS 限制" width="80" />
      <el-table-column prop="write_tps" label="TPS 限制" width="80" />
      <el-table-column label="操作" width="160" fixed="right">
        <template #default="{ row }">
          <el-button size="small" @click="editTenant(row)">编辑</el-button>
          <el-button size="small" type="danger" @click="confirmDelete(row.id)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="showCreate" :title="isEdit ? '编辑租户' : '创建租户'" width="500px">
      <el-form label-position="top">
        <el-form-item label="租户 ID">
          <el-input v-model="form.tenant_id" :disabled="isEdit" placeholder="my-tenant" />
        </el-form-item>
        <el-form-item label="存储配额 (Bytes)">
          <el-input-number v-model="form.quota.StorageBytes" :min="0" style="width: 100%" />
        </el-form-item>
        <el-form-item label="向量数量配额">
          <el-input-number v-model="form.quota.VectorCount" :min="0" style="width: 100%" />
        </el-form-item>
        <el-form-item label="查询 QPS 限制">
          <el-input-number v-model="form.quota.QueryQPS" :min="0" style="width: 100%" />
        </el-form-item>
        <el-form-item label="写入 TPS 限制">
          <el-input-number v-model="form.quota.WriteTPS" :min="0" style="width: 100%" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showCreate = false">取消</el-button>
        <el-button type="primary" @click="saveTenant">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { listTenants, createTenant, updateTenant, deleteTenant, type CreateTenantRequest } from '@/api/tenants'

const tenants = ref<any[]>([])
const loading = ref(false)
const showCreate = ref(false)
const isEdit = ref(false)
const form = ref<CreateTenantRequest>({
  tenant_id: '',
  quota: { StorageBytes: 1073741824, VectorCount: 1000000, QueryQPS: 100, WriteTPS: 50 },
})

async function loadTenants() {
  loading.value = true
  try {
    const { data } = await listTenants()
    tenants.value = Array.isArray(data) ? data : []
  } finally {
    loading.value = false
  }
}

function editTenant(row: any) {
  isEdit.value = true
  form.value = {
    tenant_id: row.id,
    quota: {
      StorageBytes: row.storage_bytes,
      VectorCount: row.vector_count,
      QueryQPS: row.query_qps,
      WriteTPS: row.write_tps,
    },
  }
  showCreate.value = true
}

async function saveTenant() {
  if (!form.value.tenant_id) {
    ElMessage.warning('请输入租户 ID')
    return
  }
  if (isEdit.value) {
    await updateTenant(form.value.tenant_id, form.value)
    ElMessage.success('租户已更新')
  } else {
    await createTenant(form.value)
    ElMessage.success('租户已创建')
  }
  showCreate.value = false
  isEdit.value = false
  form.value = { tenant_id: '', quota: { StorageBytes: 1073741824, VectorCount: 1000000, QueryQPS: 100, WriteTPS: 50 } }
  loadTenants()
}

async function confirmDelete(id: string) {
  await ElMessageBox.confirm(`确定删除租户 "${id}"？`, '删除确认', { type: 'warning' })
  await deleteTenant(id)
  ElMessage.success('租户已删除')
  loadTenants()
}

function storagePercent(row: any): number {
  if (!row.storage_bytes) return 0
  return Math.min(100, Math.round(row.storage_used / row.storage_bytes * 100))
}
function vectorPercent(row: any): number {
  if (!row.vector_count) return 0
  return Math.min(100, Math.round(row.vectors_used / row.vector_count * 100))
}
function formatBytes(bytes: number): string {
  if (bytes >= 1073741824) return (bytes / 1073741824).toFixed(1) + 'GB'
  if (bytes >= 1048576) return (bytes / 1048576).toFixed(1) + 'MB'
  if (bytes >= 1024) return (bytes / 1024).toFixed(0) + 'KB'
  return bytes + 'B'
}

onMounted(loadTenants)
</script>
