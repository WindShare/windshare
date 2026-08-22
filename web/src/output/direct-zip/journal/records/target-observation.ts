import { requireDirectZipFsaOffset } from '../../format'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalU64,
  snapshotIdentity,
} from '../../../workspace/canonical'
import { DIRECT_ZIP_JOURNAL_SCHEMA_VERSION, type DirectZipTargetObservationV1 } from '../model'
import {
  assertCanonicalProjection,
  digestFrame,
  fixedFrame,
  identityFrame,
  requireJournalVersion,
  requireUnixMilliseconds,
  snapshotDigest,
  snapshotFixedBase64,
} from './canonical-fields'

const DIRECT_ZIP_TARGET_OBSERVATION_DOMAIN = 'windshare/direct-zip-target-observation/v1'

export interface DirectZipTargetObservationInputV1 {
  readonly operationId: string
  readonly parentBindingDigest: string
  readonly fileBindingDigest: string
  readonly ownershipMarkerDigest: string
  readonly exactLength: bigint
  readonly lastModifiedMilliseconds: number
  readonly epochRootDigest: string
}

export async function createDirectZipTargetObservationV1(
  input: DirectZipTargetObservationInputV1,
): Promise<DirectZipTargetObservationV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const parentBindingDigest = snapshotDigest(input.parentBindingDigest, 'parent binding digest')
  const fileBindingDigest = snapshotDigest(input.fileBindingDigest, 'file binding digest')
  const ownershipMarkerDigest = snapshotDigest(
    input.ownershipMarkerDigest,
    'ownership marker digest',
  )
  requireDirectZipFsaOffset(input.exactLength, 'target observation exact length')
  const lastModifiedMilliseconds = requireUnixMilliseconds(
    input.lastModifiedMilliseconds,
    'target observation last-modified time',
  )
  const epochRootDigest = snapshotFixedBase64(
    input.epochRootDigest,
    32,
    'epoch root digest',
    true,
  )
  const canonicalBytes = canonicalRecord(DIRECT_ZIP_TARGET_OBSERVATION_DOMAIN, 1, [
    identityFrame(operationId, 16, 'operation ID'),
    digestFrame(parentBindingDigest, 'parent binding digest'),
    digestFrame(fileBindingDigest, 'file binding digest'),
    digestFrame(ownershipMarkerDigest, 'ownership marker digest'),
    canonicalFrame(canonicalU64(input.exactLength)),
    canonicalFrame(canonicalU64(BigInt(lastModifiedMilliseconds))),
    fixedFrame(epochRootDigest, 32, 'epoch root digest', true),
  ])
  return Object.freeze({
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    operationId,
    parentBindingDigest,
    fileBindingDigest,
    ownershipMarkerDigest,
    exactLength: input.exactLength,
    lastModifiedMilliseconds,
    epochRootDigest,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipTargetObservationV1(
  input: DirectZipTargetObservationV1,
): Promise<DirectZipTargetObservationV1> {
  requireJournalVersion(input.schemaVersion)
  const rebuilt = await createDirectZipTargetObservationV1(input)
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP target observation')
  return rebuilt
}
