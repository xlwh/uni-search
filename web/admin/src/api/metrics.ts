import client from './client'

export interface MetricEntry {
  name: string
  type: string
  labels?: Record<string, string>
  value: number
}

export interface MetricsResponse {
  metrics: MetricEntry[]
}

export function getMetrics() {
  return client.get<MetricsResponse>('/metrics')
}

export function getDashboard() {
  return client.get('/dashboard')
}
