import client from './client'

export interface TenantSummary {
  id: string
  storage_bytes: number
  vector_count: number
  query_qps: number
  write_tps: number
  storage_used: number
  vectors_used: number
}

export interface CreateTenantRequest {
  tenant_id: string
  quota: {
    StorageBytes: number
    VectorCount: number
    QueryQPS: number
    WriteTPS: number
  }
}

export function listTenants() {
  return client.get('/tenants')
}

export function createTenant(data: CreateTenantRequest) {
  return client.post('/tenants', data)
}

export function getTenant(id: string) {
  return client.get(`/tenants/${id}`)
}

export function updateTenant(id: string, data: CreateTenantRequest) {
  return client.put(`/tenants/${id}`, data)
}

export function deleteTenant(id: string) {
  return client.delete(`/tenants/${id}`)
}
