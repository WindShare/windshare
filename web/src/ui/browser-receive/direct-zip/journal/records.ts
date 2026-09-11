import { decodeBase64Url, encodeBase64Url } from '../../../../crypto/bytes'
import {
  chainDirectZipEpochDigestV1, encodeDirectZipLocalHeaderV2,
  type DirectZipEntryPlanV2,
} from '../../../../output/direct-zip/format'
import type { DirectZipEpochProofV1, DirectZipMemberAdmissionV1 } from '../../../../output/direct-zip/writer'
import {
  canonicalFrame, canonicalRecord, canonicalU64, equalCanonicalBytes, type CanonicalBytes,
} from '../../../../output/workspace/canonical'
import { CanonicalRecordReader } from '../../../../output/workspace/canonical-reader'

const LAYOUT_DOMAIN = 'windshare/browser-direct-zip-layout/v1'
const CENTRAL_DOMAIN = 'windshare/browser-direct-zip-central/v1'
const EPOCH_DOMAIN = 'windshare/browser-direct-zip-epoch/v1'
export const UNBOUND_DISCOVERY = canonicalRecord('windshare/browser-direct-zip-unbound-discovery/v1', 1, [])

export function encodeLayout(admission: DirectZipMemberAdmissionV1): CanonicalBytes {
  return canonicalRecord(LAYOUT_DOMAIN, 1, [
    canonicalFrame(canonicalU64(admission.plan.ordinal)),
    canonicalFrame(canonicalU64(admission.plan.zipEntry.localHeaderOffset)),
    canonicalFrame(encodeDirectZipLocalHeaderV2(admission.plan)),
    canonicalFrame(admission.layoutEvidence), canonicalFrame(admission.discoveryEvidence),
  ])
}

export function decodeLayout(bytes: Uint8Array) {
  const reader = CanonicalRecordReader.open(bytes, LAYOUT_DOMAIN)
  const value = {
    ordinal: reader.framedU64('layout ordinal'),
    offset: reader.framedU64('layout offset'),
    localHeader: reader.frame('layout local header'),
    layoutEvidence: reader.frame('layout evidence'),
    discoveryEvidence: reader.frame('layout discovery'),
  }
  reader.finish()
  return Object.freeze(value)
}

export function requireLayoutPlan(bytes: Uint8Array, plan: DirectZipEntryPlanV2): void {
  const layout = decodeLayout(bytes)
  if (layout.ordinal !== plan.ordinal || layout.offset !== plan.zipEntry.localHeaderOffset ||
      !equalCanonicalBytes(layout.localHeader, encodeDirectZipLocalHeaderV2(plan))) {
    throw new TypeError('Direct ZIP replay changed its committed entry plan')
  }
}

export function encodeCentral(ordinal: bigint, bytes: Uint8Array): CanonicalBytes {
  return canonicalRecord(CENTRAL_DOMAIN, 1, [
    canonicalFrame(canonicalU64(ordinal)), canonicalFrame(bytes),
  ])
}

export function decodeCentral(bytes: Uint8Array) {
  const reader = CanonicalRecordReader.open(bytes, CENTRAL_DOMAIN)
  const value = { ordinal: reader.framedU64('central ordinal'), bytes: reader.frame('central record') }
  reader.finish()
  return Object.freeze(value)
}

export function encodeEpoch(proof: DirectZipEpochProofV1): CanonicalBytes {
  return canonicalRecord(EPOCH_DOMAIN, 1, [
    canonicalFrame(canonicalU64(proof.start)), canonicalFrame(canonicalU64(proof.end)),
    canonicalFrame(proof.contentDigest), canonicalFrame(proof.predecessorRoot),
    canonicalFrame(proof.epochRoot),
  ])
}

export function decodeEpoch(bytes: Uint8Array): DirectZipEpochProofV1 {
  const reader = CanonicalRecordReader.open(bytes, EPOCH_DOMAIN)
  const proof = {
    start: reader.framedU64('epoch start'), end: reader.framedU64('epoch end'),
    contentDigest: reader.frame('epoch content digest'),
    predecessorRoot: reader.frame('epoch predecessor root'), epochRoot: reader.frame('epoch root'),
  }
  reader.finish()
  if (proof.start >= proof.end || proof.contentDigest.byteLength !== 32 ||
      proof.predecessorRoot.byteLength !== 32 || proof.epochRoot.byteLength !== 32 ||
      !equalCanonicalBytes(chainDirectZipEpochDigestV1(proof), proof.epochRoot)) {
    throw new TypeError('Direct ZIP persisted epoch proof is inconsistent')
  }
  return Object.freeze(proof)
}

export function digestBytes(value: string): Uint8Array {
  const bytes = decodeBase64Url(value)
  if (bytes?.byteLength !== 32 || encodeBase64Url(bytes) !== value) {
    throw new TypeError('Direct ZIP journal digest is not canonical')
  }
  return bytes
}
