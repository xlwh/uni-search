import { proxyGet, proxyPut, proxyDelete, proxyPost } from './proxy'

export function getCatIndices(index?: string) {
  const path = index ? `/_cat/indices/${index}` : '/_cat/indices'
  return proxyGet(`${path}?format=json`)
}

export function createIndex(name: string, body: unknown) {
  return proxyPut(`/${name}`, body)
}

export function deleteIndex(name: string) {
  return proxyDelete(`/${name}`)
}

export function getMapping(index: string) {
  return proxyGet(`/${index}/_mapping`)
}

export function putMapping(index: string, body: unknown) {
  return proxyPut(`/${index}/_mapping`, body)
}

export function getSettings(index: string) {
  return proxyGet(`/${index}/_settings`)
}

export function putSettings(index: string, body: unknown) {
  return proxyPut(`/${index}/_settings`, body)
}

export function openIndex(index: string) {
  return proxyPost(`/${index}/_open`)
}

export function closeIndex(index: string) {
  return proxyPost(`/${index}/_close`)
}

export function refreshIndex(index: string) {
  return proxyPost(`/${index}/_refresh`)
}

export function flushIndex(index: string) {
  return proxyPost(`/${index}/_flush`)
}

export function forceMergeIndex(index: string) {
  return proxyPost(`/${index}/_forcemerge`)
}

export function getAliases(index?: string) {
  const path = index ? `/${index}/_alias` : '/_alias'
  return proxyGet(path)
}

export function getSegments(index?: string) {
  const path = index ? `/${index}/_segments` : '/_segments'
  return proxyGet(path)
}

export function getIndexStats(index?: string) {
  const path = index ? `/${index}/_stats` : '/_stats'
  return proxyGet(path)
}
