export interface ClusterHealth {
  cluster_name: string
  status: string
  timed_out: boolean
  number_of_nodes: number
  number_of_data_nodes: number
  active_primary_shards: number
  active_shards: number
  relocating_shards: number
  initializing_shards: number
  unassigned_shards: number
}

export interface IndexInfo {
  health: string
  status: string
  index: string
  uuid: string
  primaries: number
  replicas: number
  docs_count: number
  docs_deleted: number
  store_size: string
}
