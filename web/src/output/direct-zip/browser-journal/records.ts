import { decodeBase64Url, encodeBase64Url } from '../../../crypto/bytes'
import {
  chainDirectZipEpochDigestV1, encodeDirectZipLocalHeaderV2, planDirectZipEntryWithDosDateTimeV2,
  type DirectZipDosDateTimeV2, type DirectZipEntryPlanV2,
} from '../format'
import type { ZipEntrySpec } from '../../zip-layout/policy'
import type { DirectZipEpochProofV1, DirectZipMemberAdmissionV1 } from '../writer'
import {
  canonicalFrame, canonicalRecord, canonicalU64, equalCanonicalBytes, type CanonicalBytes,
} from '../../../transfer/encoding/canonical'
import { CanonicalRecordReader } from '../../../transfer/encoding/canonical-reader'

const ZIP_LOCAL_HEADER_SIGNATURE = 0x04034b50
const ZIP_LOCAL_HEADER_FIXED_BYTES = 30
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

/**
 * Replays one committed member against its authenticated entry. Every field is recomputed
 * except the DOS date/time, which is taken from the committed local header: those bytes are
 * already in the target and were encoded in the zone active when the member was admitted
 * (UTC before local-time encoding), which can differ from the zone that resumes.
 */
export function replayLayoutPlan(
  bytes: Uint8Array, ordinal: bigint, entry: ZipEntrySpec,
): DirectZipEntryPlanV2 {
  const layout = decodeLayout(bytes)
  const plan = planDirectZipEntryWithDosDateTimeV2(
    { ordinal, localHeaderOffset: layout.offset, entry },
    committedDosDateTime(layout.localHeader),
  )
  if (layout.ordinal !== ordinal ||
      !equalCanonicalBytes(layout.localHeader, encodeDirectZipLocalHeaderV2(plan))) {
    throw new TypeError('Direct ZIP replay changed its committed entry plan')
  }
  return plan
}

function committedDosDateTime(localHeader: Uint8Array): DirectZipDosDateTimeV2 {
  const view = new DataView(localHeader.buffer, localHeader.byteOffset, localHeader.byteLength)
  if (localHeader.byteLength < ZIP_LOCAL_HEADER_FIXED_BYTES ||
      view.getUint32(0, true) !== ZIP_LOCAL_HEADER_SIGNATURE) {
    throw new TypeError('Direct ZIP replay changed its committed entry plan')
  }
  return { dosTime: view.getUint16(10, true), dosDate: view.getUint16(12, true) }
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
