import client from './client'

// proxyRequest sends a request through the admin reverse proxy to the
// ES API server (localhost:9200). The path should start with a slash,
// e.g. "/_cluster/health".
export function proxyRequest(method: string, path: string, data?: unknown) {
  return client.request({
    method,
    url: `/proxy${path}`,
    data,
  })
}

// Convenience helpers
export const proxyGet = (path: string) => proxyRequest('GET', path)
export const proxyPost = (path: string, data?: unknown) => proxyRequest('POST', path, data)
export const proxyPut = (path: string, data?: unknown) => proxyRequest('PUT', path, data)
export const proxyDelete = (path: string) => proxyRequest('DELETE', path)
