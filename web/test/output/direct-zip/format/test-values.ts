import {
  concatDirectZipBytes,
  encodeDirectZipBootstrapPrefixV1,
  encodeDirectZipCentralDirectoryRecordV2,
  planDirectZipEntryV2,
  type DirectZipOwnershipMarkerInputV1,
} from '../../../../src/output/direct-zip/format'
import { encodeZipEndRecords } from '../../../../src/output/zip-layout/policy'

export const DIRECT_ZIP_FIXTURE_ROOT = 'windshare-fixture'

export const TEST_DIRECT_ZIP_MARKER: DirectZipOwnershipMarkerInputV1 = Object.freeze({
  operationId: sequence(0x01, 16),
  candidateId: sequence(0x11, 16),
  ownershipNonce: sequence(0x21, 32),
  bindingDigest: sequence(0x41, 32),
})

export function encodeDirectZipMarkerFixture(): Uint8Array<ArrayBuffer> {
  const root = planDirectZipEntryV2({
    ordinal: 0n,
    localHeaderOffset: 0n,
    entry: { kind: 'directory', path: [DIRECT_ZIP_FIXTURE_ROOT] },
    ownershipMarker: TEST_DIRECT_ZIP_MARKER,
  })
  const prefix = encodeDirectZipBootstrapPrefixV1(DIRECT_ZIP_FIXTURE_ROOT, TEST_DIRECT_ZIP_MARKER)
  const central = encodeDirectZipCentralDirectoryRecordV2(root, 0)
  const end = encodeZipEndRecords({
    entryCount: 1n,
    centralDirectoryOffset: BigInt(prefix.byteLength),
    centralDirectoryBytes: BigInt(central.byteLength),
    zip64EndRequired: false,
  })
  return concatDirectZipBytes([prefix, central, end.classicEnd])
}

export function sequence(first: number, length: number): Uint8Array<ArrayBuffer> {
  return Uint8Array.from({ length }, (_, index) => (first + index) & 0xff)
}
