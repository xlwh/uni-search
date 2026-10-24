import { proxyGet, proxyPut, proxyDelete } from './proxy'

export function getScript(id: string) {
  return proxyGet(`/_scripts/${id}`)
}

export function putScript(id: string, body: unknown) {
  return proxyPut(`/_scripts/${id}`, body)
}

export function deleteScript(id: string) {
  return proxyDelete(`/_scripts/${id}`)
}
