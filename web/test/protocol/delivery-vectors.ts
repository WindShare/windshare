import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface DescriptorObjectVector extends VectorCase {
  readonly domain: string
  readonly contextB64: string
  readonly contextHashB64: string
  readonly keyB64: string
  readonly nonceB64: string
  readonly canonicalCborB64: string
  readonly aadB64: string
  readonly signaturePreimageB64: string
  readonly objectB64: string
}

export const descriptorObjectVector = loadVectorFile(
  new URL('../../../core/testvectors/v2-descriptor-object.json', import.meta.url),
).cases[0] as DescriptorObjectVector

const contentPayloadCases = loadVectorFile(
  new URL('../../../core/testvectors/v2-content-payloads.json', import.meta.url),
).cases

export const CONTENT_PAYLOAD_NAMES = ['catalog-page', 'directory-failure', 'file-revision', 'block-record'] as const

export function contentPayload(name: (typeof CONTENT_PAYLOAD_NAMES)[number]): Uint8Array<ArrayBuffer> {
  const encoded = contentPayloadCases.find(candidate => candidate.name === name)?.canonicalCborB64
  if (typeof encoded !== 'string') throw new Error(`missing canonical content payload ${name}`)
  return b64ToBytes(encoded)
}
