import type { CatalogEntry } from '../../catalog/records'
import { snapshotCatalogPath } from '../../catalog/path-policy'
import { SelectionPolicy, type FrozenSelectionPolicy } from '../../catalog/selection'
import type { ArtifactOffers } from '../../output/model'
import {
  planArtifactOffers,
  PORTABLE_MAX_BYTES,
  type BrowserOutputCapabilities,
} from '../../output/backends/contract'
import type { ReceiveTaskRecord } from '../../output/store/types'
import type { SelectionShape } from '../../transfer/discovery'
import {
  selectionKey,
  selectionSpecFromPolicy,
  type SelectionSpec,
} from '../../transfer/selection'
import type { BrowseDirectory, JoinedBrowserShare } from '../browser-share/gateway'
import type {
  ReceiveAction,
  ReceiveAdmission,
  ReceiveBlockReason,
  RetainedTaskSnapshot,
  SaveUnavailableReason,
} from './contracts'

export class SaveUnavailableError extends Error {
  readonly reason: SaveUnavailableReason

  constructor(reason: SaveUnavailableReason, message = `Single-file save unavailable: ${reason}`) {
    super(message)
    this.name = 'SaveUnavailableError'
    this.reason = reason
  }
}

export class ReceiveBlockedError extends Error {
  readonly reason: ReceiveBlockReason

  constructor(reason: ReceiveBlockReason, message = `Receive action blocked: ${reason}`) {
    super(message)
    this.name = 'ReceiveBlockedError'
    this.reason = reason
  }
}

export interface SingleFileDownloadPlan {
  readonly selection: FrozenSelectionPolicy
  readonly spec: SelectionSpec
  readonly selectionKey: string
  readonly offers: ArtifactOffers
}

export type SingleFileReuseDecision =
  | Readonly<{ kind: 'export'; task: RetainedTaskSnapshot }>
  | Readonly<{ kind: 'resume'; task: RetainedTaskSnapshot }>
  | Readonly<{ kind: 'blocked'; reason: ReceiveBlockReason; task: RetainedTaskSnapshot }>
  | Readonly<{ kind: 'none' }>

/**
 * Synchronously validates the target file path and plans single-file offers
 * using a throwaway SelectionPolicy so `joined.selection` draft state is untouched.
 */
export function planSingleFileDownload(
  joined: JoinedBrowserShare,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
  capabilities: BrowserOutputCapabilities,
): SingleFileDownloadPlan {
  const sourcePath = snapshotCatalogPath([...directory.path, entry.name])
  const policy = new SelectionPolicy(false)
  policy.set(entry, directory.ancestry, true)
  const selection = policy.snapshot()
  const spec = selectionSpecFromPolicy(
    joined.descriptor.shareInstanceId,
    joined.descriptor.syntheticRootId,
    selection,
  )
  const key = selectionKey(spec)
  const shape: Extract<SelectionShape, { kind: 'single-file' }> = Object.freeze({
    kind: 'single-file',
    file: entry,
    sourcePath,
    exactBytes: entry.expectedSize,
  })
  const offers = planArtifactOffers({ epoch: 0n, selectionKey: key, shape, capabilities })
  return Object.freeze({ selection, spec, selectionKey: key, offers })
}

/**
 * Returns the minimum single-file byte size at which `planArtifactOffers` falls back to
 * FSA (requiring a synchronous directory picker inside the user click), or `null` when
 * FSA is never selected as the primary backend.
 */
export function singleFilePickerFloorBytes(
  capabilities: BrowserOutputCapabilities | undefined,
): bigint | null {
  if (capabilities === undefined || !capabilities.fsa) return null
  if (capabilities.opfs && capabilities.availableStorageBytes === null) return null
  let nonPickerMax = -1n
  if (capabilities.portable) nonPickerMax = PORTABLE_MAX_BYTES
  if (
    capabilities.opfs &&
    capabilities.availableStorageBytes !== null &&
    capabilities.availableStorageBytes > nonPickerMax
  ) {
    nonPickerMax = capabilities.availableStorageBytes
  }
  return nonPickerMax + 1n
}

export function singleFileNeedsPicker(
  expectedSize: bigint | undefined,
  pickerFloorBytes: bigint | null,
): boolean {
  return pickerFloorBytes !== null && expectedSize !== undefined && expectedSize >= pickerFloorBytes
}

export function isSingleFileReuseCandidateRecord(record: ReceiveTaskRecord): boolean {
  switch (record.lifecycle.kind) {
    case 'ready-to-save':
      return record.backend === 'opfs'
    case 'download-started':
      return record.backend === 'opfs' && record.lifecycle.retained
    case 'paused':
    case 'downloading':
    case 'preparing':
      return true
    default:
      return false
  }
}

function isExportableOpfsTask(task: RetainedTaskSnapshot): boolean {
  if (task.backend !== 'opfs') return false
  return task.lifecycle.kind === 'ready-to-save' ||
    (task.lifecycle.kind === 'download-started' && task.lifecycle.retained)
}

function byNewestCreatedDesc(left: RetainedTaskSnapshot, right: RetainedTaskSnapshot): number {
  return (right.display?.createdAtMilliseconds ?? 0) - (left.display?.createdAtMilliseconds ?? 0)
}

/**
 * Pure reuse decision for single-file quick download.
 * Ignores dead terminal records (`saved`, `failed`, `partial`, non-retained handoffs)
 * and prefers completed exportable OPFS copies over `paused` tasks, newest first.
 */
export function decideSingleFileReuse(
  tasks: readonly RetainedTaskSnapshot[],
  shareInstance: string,
  key: string,
  admit: (task: RetainedTaskSnapshot, action: ReceiveAction) => ReceiveAdmission,
): SingleFileReuseDecision {
  const matching = tasks.filter(
    task => task.shareInstance === shareInstance && task.selectionKey === key,
  )
  if (matching.length === 0) return Object.freeze({ kind: 'none' })

  const exportable = matching.filter(isExportableOpfsTask).sort(byNewestCreatedDesc)
  if (exportable.length > 0) {
    let firstBlocked: Readonly<{ reason: ReceiveBlockReason; task: RetainedTaskSnapshot }> | undefined
    for (const candidate of exportable) {
      const admission = admit(candidate, 'save')
      if (admission.allowed) return Object.freeze({ kind: 'export', task: candidate })
      firstBlocked ??= { reason: admission.reason ?? 'actionUnavailable', task: candidate }
    }
    if (firstBlocked !== undefined) {
      return Object.freeze({ kind: 'blocked', reason: firstBlocked.reason, task: firstBlocked.task })
    }
  }

  const paused = matching.filter(task => task.lifecycle.kind === 'paused').sort(byNewestCreatedDesc)
  const newestPaused = paused[0]
  if (newestPaused !== undefined) {
    const admission = admit(newestPaused, 'continue')
    if (admission.allowed) return Object.freeze({ kind: 'resume', task: newestPaused })
    return Object.freeze({
      kind: 'blocked',
      reason: admission.reason ?? 'actionUnavailable',
      task: newestPaused,
    })
  }

  const activeElsewhere = matching
    .filter(task => (task.lifecycle.kind === 'downloading' || task.lifecycle.kind === 'preparing') && task.activeElsewhere)
    .sort(byNewestCreatedDesc)[0]
  if (activeElsewhere !== undefined) {
    return Object.freeze({ kind: 'blocked', reason: 'activeInAnotherTab', task: activeElsewhere })
  }

  return Object.freeze({ kind: 'none' })
}

export function mergeRetainedCandidates(
  groups: readonly (readonly RetainedTaskSnapshot[])[],
): readonly RetainedTaskSnapshot[] {
  const byId = new Map<string, RetainedTaskSnapshot>()
  for (const group of groups) {
    for (const task of group) {
      const existing = byId.get(task.taskId)
      if (existing === undefined || task.lifecycle.generation >= existing.lifecycle.generation) {
        byId.set(task.taskId, task)
      }
    }
  }
  return Object.freeze([...byId.values()])
}
