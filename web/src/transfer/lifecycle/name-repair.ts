import { snapshotCatalogPath, isCatalogName } from '../../catalog/path-policy'
import { encodeBase64Url } from '../../crypto/bytes'
import { canonicalPath } from '../encoding/canonical'

/** User-visible restoration evidence. Storage handles, ledger records and repair algorithms remain output-owned. */
export type CompatibleNamePairPlacement = 'inside-logical-root' | 'beside-mapped-root'

export type CompatibleNameFooterState = 'active' | 'completed' | 'stopped' | 'failed'

export interface CompatibleNameFooterObservationV1 {
  readonly committedCount: number
  readonly state: CompatibleNameFooterState
}

export interface CompatibleNameRepairSummary {
  readonly committedCount: number
  readonly logicalPathSample: readonly (readonly string[])[]
  readonly pairDisplayNames: Readonly<{
    script?: string
    sidecar: string
  }>
  readonly placement: CompatibleNamePairPlacement
  readonly latestObservedFooter?: CompatibleNameFooterObservationV1
  readonly sidecarSync: 'pending' | 'current'
  readonly terminalSettlement: 'none' | 'pending' | 'complete'
}

export const MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS = 1_048_576

export const MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS = 8

export function compatibleNameRepairSummary(
  input: CompatibleNameRepairSummary,
): CompatibleNameRepairSummary {
  const committedCount = compatibleNameBoundedCount(
    input.committedCount,
    'compatible-name committed count',
  )
  if (input.logicalPathSample.length > MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS ||
      input.logicalPathSample.length > committedCount) {
    throw new TypeError('compatible-name repair sample exceeds its bound')
  }
  const logicalPathSample = Object.freeze(input.logicalPathSample.map(snapshotCatalogPath))
  const sampleKeys = logicalPathSample.map(path => encodeBase64Url(canonicalPath(path)))
  if (new Set(sampleKeys).size !== sampleKeys.length) {
    throw new TypeError('compatible-name repair sample repeats a logical path')
  }
  const latestObservedFooter = input.latestObservedFooter === undefined
    ? undefined
    : compatibleNameFooterObservationV1(input.latestObservedFooter)
  if (latestObservedFooter !== undefined && latestObservedFooter.committedCount > committedCount) {
    throw new TypeError('compatible-name footer exceeds the committed ledger prefix')
  }
  if (input.sidecarSync !== 'pending' && input.sidecarSync !== 'current') {
    throw new TypeError('invalid sidecar synchronization state')
  }
  if (!['none', 'pending', 'complete'].includes(input.terminalSettlement)) {
    throw new TypeError('invalid terminal settlement state')
  }
  if (input.sidecarSync === 'current' &&
      latestObservedFooter?.committedCount !== committedCount) {
    throw new TypeError('current sidecar requires a verified complete ledger prefix')
  }
  if (input.terminalSettlement === 'complete' &&
      (input.sidecarSync !== 'current' || latestObservedFooter?.state === 'active')) {
    throw new TypeError('complete terminal settlement requires a current terminal footer')
  }
  return Object.freeze({
    committedCount,
    logicalPathSample,
    pairDisplayNames: Object.freeze({
      ...(input.pairDisplayNames.script === undefined ? {} : { script: compatibleNameComponent(input.pairDisplayNames.script, 'script display name') }),
      sidecar: compatibleNameComponent(input.pairDisplayNames.sidecar, 'sidecar display name'),
    }),
    placement: compatiblePairPlacement(input.placement),
    ...(latestObservedFooter === undefined ? {} : { latestObservedFooter }),
    sidecarSync: input.sidecarSync,
    terminalSettlement: input.terminalSettlement,
  })
}

export function compatibleNameFooterObservationV1(
  input: CompatibleNameFooterObservationV1,
): CompatibleNameFooterObservationV1 {
  return Object.freeze({
    committedCount: compatibleNameBoundedCount(input.committedCount, 'footer committed count'),
    state: compatibleFooterState(input.state),
  })
}

export function compatiblePairPlacement(value: CompatibleNamePairPlacement): CompatibleNamePairPlacement {
  if (value !== 'inside-logical-root' && value !== 'beside-mapped-root') {
    throw new TypeError('compatible-name pair placement is invalid')
  }
  return value
}

function compatibleFooterState(value: CompatibleNameFooterState): CompatibleNameFooterState {
  if (value !== 'active' && value !== 'completed' && value !== 'stopped' && value !== 'failed') {
    throw new TypeError('compatible-name footer state is invalid')
  }
  return value
}

export function compatibleNameComponent(value: string, label: string): string {
  if (typeof value !== 'string' || !isCatalogName(value)) {
    throw new TypeError(`${label} violates the canonical path-component policy`)
  }
  return value
}

export function compatibleNameBoundedCount(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value < 0 || value > MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS) {
    throw new TypeError(`${label} is invalid`)
  }
  return value
}
