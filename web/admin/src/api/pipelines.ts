import { proxyGet, proxyPut, proxyDelete, proxyPost } from './proxy'

export function getPipelines() {
  return proxyGet('/_ingest/pipeline')
}

export function getPipeline(id: string) {
  return proxyGet(`/_ingest/pipeline/${id}`)
}

export function putPipeline(id: string, body: unknown) {
  return proxyPut(`/_ingest/pipeline/${id}`, body)
}

export function deletePipeline(id: string) {
  return proxyDelete(`/_ingest/pipeline/${id}`)
}

export function simulatePipeline(id: string, body: unknown) {
  return proxyPost(`/_ingest/pipeline/${id}/_simulate`, body)
}
