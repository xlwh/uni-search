import { proxyGet, proxyPut, proxyDelete } from './proxy'

export function getSynonyms() {
  return proxyGet('/_synonyms')
}

export function putSynonyms(id: string, body: unknown) {
  return proxyPut(`/_synonyms/${id}`, body)
}

export function deleteSynonyms(id: string) {
  return proxyDelete(`/_synonyms/${id}`)
}

export function getSynonymSet(id: string) {
  return proxyGet(`/_synonyms/${id}`)
}
