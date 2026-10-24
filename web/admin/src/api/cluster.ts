import { proxyGet } from './proxy'

export function getClusterHealth() {
  return proxyGet('/_cluster/health')
}

export function getClusterState() {
  return proxyGet('/_cluster/state')
}

export function getClusterStats() {
  return proxyGet('/_cluster/stats')
}

export function getNodesInfo() {
  return proxyGet('/_nodes/info')
}

export function getNodesStats() {
  return proxyGet('/_nodes/stats')
}

export function getCatHealth() {
  return proxyGet('/_cat/health')
}

export function getCatNodes() {
  return proxyGet('/_cat/nodes')
}
