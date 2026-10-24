import { proxyGet, proxyPut, proxyDelete, proxyPost } from './proxy'

export function listRepos() {
  return proxyGet('/_snapshot')
}

export function createRepo(name: string, body: unknown) {
  return proxyPut(`/_snapshot/${name}`, body)
}

export function deleteRepo(name: string) {
  return proxyDelete(`/_snapshot/${name}`)
}

export function listSnapshots(repo: string) {
  return proxyGet(`/_snapshot/${repo}`)
}

export function createSnapshot(repo: string, snapshot: string, body?: unknown) {
  return proxyPut(`/_snapshot/${repo}/${snapshot}`, body)
}

export function getSnapshot(repo: string, snapshot: string) {
  return proxyGet(`/_snapshot/${repo}/${snapshot}`)
}

export function deleteSnapshot(repo: string, snapshot: string) {
  return proxyDelete(`/_snapshot/${repo}/${snapshot}`)
}

export function restoreSnapshot(repo: string, snapshot: string, body?: unknown) {
  return proxyPost(`/_snapshot/${repo}/${snapshot}/_restore`, body)
}

export function verifyRepo(repo: string) {
  return proxyPost(`/_snapshot/${repo}/_verify`)
}
