import { proxyPost, proxyGet } from './proxy'

export function search(index: string, body: unknown) {
  return proxyPost(`/${index}/_search`, body)
}

export function searchAll(body: unknown) {
  return proxyPost('/_search', body)
}

export function knnSearch(index: string, body: unknown) {
  return proxyPost(`/${index}/_knn_search`, body)
}

export function countDocuments(index: string, body?: unknown) {
  return proxyPost(`/${index}/_count`, body)
}

export function explainQuery(index: string, id: string, body: unknown) {
  return proxyPost(`/${index}/_explain/${id}`, body)
}

export function validateQuery(index: string, body: unknown) {
  return proxyPost(`/${index}/_validate/query`, body)
}
