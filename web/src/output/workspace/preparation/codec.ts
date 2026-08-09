import { snapshotPortableCatalogPath } from '../../../catalog/path-policy'
import {
  snapshotCanonicalModifiedTime,
  type CanonicalModifiedTime,
} from '../../../transfer/directory-admission'
import type { SealedZipLayoutPlanV1 } from '../../zip-layout/layout'
import type { ZipEntryPlanV1, ZipEntrySpec } from '../../zip-layout/policy'
import {
  canonicalBoolean,
  canonicalFrame,
  canonicalI64,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalU32,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  type CanonicalBytes,
} from '../canonical'
import { CanonicalRecordReader } from '../canonical-reader'
import type { AuthenticatedGenerationReference } from '../manifest'
import {
  MAX_ARTIFACT_ENTRIES,
  type PreparationDirectoryRole,
  type PreparationManifestEntry,
} from './model'

const NANOSECONDS_PER_MILLISECOND = 1_000_000n

export function canonicalSealedZipLayoutStorageBytes(
  candidate: SealedZipLayoutPlanV1,
): CanonicalBytes {
  const plan = candidate
  const evidence = plan.evidence.kind === 'prepared'
    ? concatCanonicalBytes([
        canonicalU8(1),
        canonicalFrame(canonicalIdentity(
          plan.evidence.preparationManifestDigest,
          32,
          'preparation manifest digest',
        )),
      ])
    : concatCanonicalBytes([
        canonicalU8(2),
        canonicalFrame(canonicalIdentity(
          plan.evidence.discoveryLedgerDigest,
          32,
          'discovery ledger digest',
        )),
      ])
  return canonicalRecord('windshare/zip-layout/v1', 1, [
    canonicalFrame(canonicalIdentity(plan.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(plan.artifactDigest, 32, 'artifact digest')),
    canonicalFrame(evidence),
    canonicalFrame(canonicalText(plan.encodingPolicy)),
    canonicalFrame(canonicalU8(plan.encodingPolicyVersion)),
    canonicalU64(BigInt(plan.entries.length)),
    ...plan.entries.map((entry) => canonicalFrame(canonicalZipEntryPlan(entry))),
    canonicalFrame(canonicalU64(plan.centralDirectoryOffset)),
    canonicalFrame(canonicalU64(plan.centralDirectoryBytes)),
    canonicalFrame(canonicalBoolean(plan.zip64EndRequired)),
    canonicalFrame(canonicalU64(plan.zip64EndBytes)),
    canonicalFrame(canonicalU64(plan.classicEndBytes)),
    canonicalFrame(canonicalU64(plan.exactArchiveBytes)),
    canonicalFrame(canonicalU64(plan.maximumSpoolBytes)),
    canonicalFrame(canonicalIdentity(plan.digest, 32, 'ZIP layout digest')),
  ])
}

export function canonicalPreparationEntry(entry: PreparationManifestEntry): CanonicalBytes {
  const common = [
    canonicalFrame(canonicalPreparationSourcePath(entry)),
    canonicalFrame(canonicalPath(entry.artifactPath)),
  ]
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      ...common,
      canonicalFrame(canonicalIdentity(entry.directoryId, 16, 'directory ID')),
      canonicalFrame(canonicalIdentity(entry.generation, 16, 'directory generation')),
      canonicalFrame(canonicalModifiedTime(entry.modifiedTime)),
      canonicalFrame(canonicalU8(directoryRoleByte(entry.role))),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    ...common,
    canonicalFrame(canonicalIdentity(entry.fileId, 16, 'file ID')),
    canonicalFrame(canonicalIdentity(entry.containingDirectoryId, 16, 'containing directory ID')),
    canonicalFrame(canonicalIdentity(entry.generation, 16, 'directory generation')),
    canonicalFrame(canonicalU64(entry.exactSize)),
    canonicalFrame(canonicalModifiedTime(entry.modifiedTime)),
  ])
}

export function decodePreparationGeneration(bytes: Uint8Array): AuthenticatedGenerationReference {
  const reader = CanonicalRecordReader.value(bytes)
  const generation = Object.freeze({
    directoryId: reader.framedIdentity(16, 'directory ID'),
    generation: reader.framedIdentity(16, 'directory generation'),
  })
  reader.finish('preparation generation')
  return generation
}

export function decodePreparationEntry(bytes: Uint8Array): PreparationManifestEntry {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('preparation entry discriminant')
  const sourcePath = decodeCanonicalPath(reader.frame('preparation source path'), true)
  const artifactPath = decodeCanonicalPath(reader.frame('preparation artifact path'), false)
  if (discriminant === 1) {
    const directoryId = reader.framedIdentity(16, 'directory ID')
    const generation = reader.framedIdentity(16, 'directory generation')
    const modifiedTime = decodeCanonicalModifiedTime(reader.frame('directory modified time'))
    const role = decodePreparationDirectoryRole(reader.frame('preparation directory role'))
    reader.finish('preparation directory entry')
    return Object.freeze({
      kind: 'directory',
      sourcePath,
      artifactPath,
      directoryId,
      generation,
      ...(modifiedTime === undefined ? {} : { modifiedTime }),
      role,
    })
  }
  if (discriminant !== 2) throw new TypeError('preparation entry discriminant is invalid')
  const fileId = reader.framedIdentity(16, 'file ID')
  const containingDirectoryId = reader.framedIdentity(16, 'containing directory ID')
  const generation = reader.framedIdentity(16, 'directory generation')
  const exactSize = reader.framedU64('file size')
  const modifiedTime = decodeCanonicalModifiedTime(reader.frame('file modified time'))
  reader.finish('preparation file entry')
  return Object.freeze({
    kind: 'file',
    sourcePath,
    artifactPath,
    fileId,
    containingDirectoryId,
    generation,
    exactSize,
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

export function boundedEntryCount(value: bigint, allowZero = true): number {
  if ((!allowZero && value === 0n) || value > BigInt(MAX_ARTIFACT_ENTRIES)) {
    throw new TypeError('persisted preparation aggregate exceeds its entry bound')
  }
  return Number(value)
}

export function canonicalGeneration(reference: AuthenticatedGenerationReference): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(reference.directoryId, 16, 'directory ID')),
    canonicalFrame(canonicalIdentity(reference.generation, 16, 'directory generation')),
  ])
}

export function zipEntrySpecs(
  entries: readonly PreparationManifestEntry[],
): readonly ZipEntrySpec[] {
  return Object.freeze(entries.map((entry) => {
    const modifiedTimeMilliseconds = entry.modifiedTime === undefined
      ? undefined
      // Precision 3 permits sub-millisecond timestamps. BigInt division floors the
      // non-negative fractional second without crossing a Number-to-BigInt boundary.
      : entry.modifiedTime.seconds * 1_000n +
        BigInt(entry.modifiedTime.nanoseconds) / NANOSECONDS_PER_MILLISECOND
    if (entry.kind === 'directory') {
      return Object.freeze({
        kind: 'directory' as const,
        path: entry.artifactPath,
        ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
      })
    }
    return Object.freeze({
      kind: 'file' as const,
      path: entry.artifactPath,
      exactSize: entry.exactSize,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
  }))
}

function decodeCanonicalModifiedTime(bytes: Uint8Array): CanonicalModifiedTime | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('modified time discriminant')
  if (discriminant === 1) {
    reader.finish('absent modified time')
    return undefined
  }
  if (discriminant !== 2) throw new TypeError('modified time discriminant is invalid')
  const secondsBytes = reader.frame('modified seconds')
  const nanosecondsBytes = reader.frame('modified nanoseconds')
  const precisionBytes = reader.frame('modified precision')
  if (secondsBytes.byteLength !== 8 || nanosecondsBytes.byteLength !== 4 ||
      precisionBytes.byteLength !== 1) {
    throw new TypeError('modified time field width is invalid')
  }
  const modifiedTime = snapshotCanonicalModifiedTime({
    seconds: new DataView(
      secondsBytes.buffer,
      secondsBytes.byteOffset,
      secondsBytes.byteLength,
    ).getBigInt64(0, false),
    nanoseconds: new DataView(
      nanosecondsBytes.buffer,
      nanosecondsBytes.byteOffset,
      nanosecondsBytes.byteLength,
    ).getUint32(0, false),
    precision: precisionBytes[0] as CanonicalModifiedTime['precision'],
  })
  reader.finish('modified time')
  return modifiedTime
}

function decodePreparationDirectoryRole(bytes: Uint8Array): PreparationDirectoryRole {
  const reader = CanonicalRecordReader.value(bytes)
  const value = reader.byte('preparation directory role')
  reader.finish('preparation directory role')
  switch (value) {
    case 1: return 'result-root'
    case 2: return 'necessary-ancestor'
    case 3: return 'explicitly-selected-empty'
    default: throw new TypeError('preparation directory role is invalid')
  }
}

function decodeCanonicalPath(bytes: Uint8Array, allowEmpty: boolean): readonly string[] {
  const reader = CanonicalRecordReader.value(bytes)
  const count = reader.u64('canonical path segment count')
  if (count > 256n || (!allowEmpty && count === 0n)) {
    throw new TypeError('canonical preparation path segment count is invalid')
  }
  const segments: string[] = []
  for (let index = 0n; index < count; index += 1n) {
    const canonical = reader.frame('canonical path segment')
    const value = new TextDecoder(undefined, { fatal: true }).decode(canonical)
    if (!equalCanonicalBytes(canonicalText(value), canonical)) {
      throw new TypeError('canonical preparation path text changed during decoding')
    }
    segments.push(value)
  }
  reader.finish('canonical preparation path')
  return segments.length === 0 ? Object.freeze([]) : snapshotPortableCatalogPath(segments)
}

function canonicalPreparationSourcePath(entry: PreparationManifestEntry): CanonicalBytes {
  if (entry.sourcePath.length !== 0) return canonicalPath(entry.sourcePath)
  if (entry.kind !== 'directory' || entry.role !== 'result-root') {
    throw new TypeError('only a result-root directory may encode an empty preparation source path')
  }
  // canonicalPath intentionally keeps the repository-wide non-empty path policy;
  // this local zero-segment encoding is the synthetic catalog-root sentinel.
  return canonicalU64(0n)
}

function canonicalModifiedTime(value: CanonicalModifiedTime | undefined): CanonicalBytes {
  if (value === undefined) return canonicalU8(1)
  const modified = snapshotCanonicalModifiedTime(value)
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalI64(modified.seconds)),
    canonicalFrame(canonicalU32(modified.nanoseconds)),
    canonicalFrame(canonicalU8(modified.precision)),
  ])
}

function canonicalZipEntryPlan(entry: ZipEntryPlanV1): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalU8(entry.kind === 'directory' ? 1 : 2),
    canonicalFrame(canonicalPath(entry.path)),
    canonicalFrame(Uint8Array.from(entry.nameBytes)),
    canonicalFrame(canonicalU64(entry.exactSize)),
    canonicalFrame(canonicalU32(entry.dosTime)),
    canonicalFrame(canonicalU32(entry.dosDate)),
    canonicalFrame(canonicalBoolean(entry.zip64Size)),
    canonicalFrame(canonicalBoolean(entry.zip64Offset)),
    canonicalFrame(canonicalU8(entry.versionNeeded)),
    canonicalFrame(canonicalU64(entry.localHeaderOffset)),
    canonicalFrame(canonicalU64(entry.localExtraBytes)),
    canonicalFrame(canonicalU64(entry.localHeaderBytes)),
    canonicalFrame(canonicalU64(entry.descriptorBytes)),
    canonicalFrame(canonicalU64(entry.entryStreamBytes)),
    canonicalFrame(canonicalU8(entry.centralZip64ValueCount)),
    canonicalFrame(canonicalU64(entry.centralExtraBytes)),
    canonicalFrame(canonicalU64(entry.centralRecordBytes)),
  ])
}

function directoryRoleByte(role: PreparationDirectoryRole): number {
  switch (role) {
    case 'result-root': return 1
    case 'necessary-ancestor': return 2
    case 'explicitly-selected-empty': return 3
  }
}
