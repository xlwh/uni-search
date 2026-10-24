import { proxyGet, proxyPost, proxyDelete, proxyPut } from './proxy'

export function getDocument(index: string, id: string) {
  return proxyGet(`/${index}/_doc/${id}`)
}

export function indexDocument(index: string, id: string, body: unknown) {
  return proxyPut(`/${index}/_doc/${id}`, body)
}

export function indexAutoId(index: string, body: unknown) {
  return proxyPost(`/${index}/_doc`, body)
}

export function deleteDocument(index: string, id: string) {
  return proxyDelete(`/${index}/_doc/${id}`)
}

export function bulk(body: string) {
  return proxyPost('/_bulk', body)
}

export function analyze(index: string, body: unknown) {
  return proxyPost(`/${index}/_analyze`, body)
}
