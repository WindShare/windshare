import { snapshotPortableCatalogPath } from '../../../catalog/path-policy'
import { validateZipEntryPlan, type ZipEntryPlanV1 } from '../../zip-layout/policy'

export interface TaskObjectRef {
  readonly operationId: string
  readonly objectId: string
  readonly kind: 'original-file' | 'zip-archive'
  readonly handleId: string
}

export interface ZipEntryLayout {
  readonly entryId: string
  readonly sequence: bigint
  readonly localHeaderOffset: bigint
  readonly payloadOffset: bigint
  readonly exactSize: bigint
  readonly descriptorOffset: bigint
  readonly endOffset: bigint
  readonly encodingVersion: 1
}

export interface TaskRange {
  readonly start: bigint
  readonly end: bigint
  readonly crc32: number
}

export interface TaskEntry {
  readonly entryId: string
  readonly path: readonly string[]
  readonly kind: 'file' | 'directory'
  readonly source: Readonly<{
    shareInstance: string
    directoryId: string
    generation: string
    sourcePath: readonly string[]
  }>
  readonly revision?: Readonly<{ fileId: string; fileRevision: string; exactSize: bigint }>
  readonly zipLayout?: ZipEntryLayout
  readonly zipPlan?: ZipEntryPlanV1
  readonly ranges: readonly TaskRange[]
  readonly revisionFailure?: string
}

export interface TaskDirectoryPin {
  readonly directoryId: string
  readonly generation: string
  readonly sourcePath: readonly string[]
}

export interface TaskCheckpoint {
  readonly object: TaskObjectRef
  readonly generation: bigint
  /** End of the immutable entry layout; unwritten payload gaps need not exist physically yet. */
  readonly allocatedLength: bigint
  readonly physicalLength: bigint
  readonly entryCount: bigint
  readonly discoveryComplete: boolean
  readonly selectedPaths: readonly (readonly string[])[]
  readonly artifactState: 'receiving' | 'finalizing' | 'sealed'
  readonly finalization?: Readonly<{
    nextEntry: bigint
    centralDirectoryOffset: bigint
    committedLength: bigint
  }>
  readonly sealedLength?: bigint
}

export const MAX_TASK_ENTRY_PAGE_SIZE = 256
const MAX_UINT64 = (1n << 64n) - 1n

export function snapshotTaskCheckpoint(value: TaskCheckpoint): TaskCheckpoint {
  for (const identity of [value.object.operationId, value.object.objectId, value.object.handleId]) {
    if (typeof identity !== 'string' || identity.length === 0) throw new TypeError('Task object identity is missing')
  }
  if (value.object.kind !== 'original-file' && value.object.kind !== 'zip-archive') {
    throw new TypeError('Task object kind is invalid')
  }
  uint64(value.generation, 'generation')
  if (value.generation === 0n) throw new TypeError('Task generation starts at one')
  uint64(value.allocatedLength, 'allocated length')
  uint64(value.physicalLength, 'physical length')
  uint64(value.entryCount, 'entry count')
  if (typeof value.discoveryComplete !== 'boolean' ||
      !['receiving', 'finalizing', 'sealed'].includes(value.artifactState)) {
    throw new TypeError('Task checkpoint state is invalid')
  }
  if (value.artifactState !== 'receiving' && !value.discoveryComplete) {
    throw new TypeError('Task finalization requires complete discovery')
  }
  validateFinalization(value)
  if (value.sealedLength !== undefined) uint64(value.sealedLength, 'sealed length')
  if (value.artifactState === 'sealed' &&
      (value.sealedLength === undefined || value.sealedLength !== value.physicalLength)) {
    throw new TypeError('Sealed task has no artifact length')
  }
  return Object.freeze({
    ...value,
    object: Object.freeze({ ...value.object }),
    selectedPaths: Object.freeze(value.selectedPaths.map(path => Object.freeze([...path]))),
    ...(value.finalization === undefined ? {} : { finalization: Object.freeze({ ...value.finalization }) }),
  })
}

export function snapshotTaskEntry(value: TaskEntry): TaskEntry {
  if (!value.entryId || (value.kind !== 'file' && value.kind !== 'directory')) {
    throw new TypeError('Task entry identity is invalid')
  }
  const path = snapshotPortableCatalogPath(value.path)
  if (!value.source.shareInstance || !value.source.directoryId || !value.source.generation) {
    throw new TypeError('Task entry lacks authenticated source evidence')
  }
  if (value.kind === 'file' && value.revision === undefined) {
    throw new TypeError('File allocation requires an authenticated opened revision')
  }
  if (value.revision !== undefined) {
    if (!value.revision.fileId || !value.revision.fileRevision) throw new TypeError('Task revision is invalid')
    uint64(value.revision.exactSize, 'revision size')
  }
  const exactSize = value.revision?.exactSize ?? 0n
  validateRanges(value.ranges, exactSize)
  const layout = value.zipLayout
  const zipPlan = validateLayout(value, path, exactSize)
  return Object.freeze({
    ...value, path,
    source: Object.freeze({ ...value.source, sourcePath: Object.freeze([...value.source.sourcePath]) }),
    ...(value.revision === undefined ? {} : { revision: Object.freeze({ ...value.revision }) }),
    ...(layout === undefined ? {} : { zipLayout: Object.freeze({ ...layout }) }),
    ...(zipPlan === undefined ? {} : { zipPlan }),
    ranges: Object.freeze(value.ranges.map(range => Object.freeze({ ...range }))),
  })
}

export function taskEntryComplete(entry: TaskEntry): boolean {
  if (entry.revisionFailure !== undefined) return false
  const size = entry.revision?.exactSize ?? 0n
  let end = 0n
  for (const range of entry.ranges) {
    if (range.start !== end) return false
    end = range.end
  }
  return end === size
}

function uint64(value: bigint, label: string): void {
  if (typeof value !== 'bigint' || value < 0n || value > MAX_UINT64) {
    throw new TypeError(`Task ${label} is not an unsigned 64-bit integer`)
  }
}
function validateFinalization(value: TaskCheckpoint): void {
  if (value.finalization !== undefined) {
    uint64(value.finalization.nextEntry, 'finalization cursor')
    uint64(value.finalization.centralDirectoryOffset, 'central-directory offset')
    uint64(value.finalization.committedLength, 'finalization length')
    if (value.finalization.nextEntry > value.entryCount ||
        value.finalization.committedLength < value.finalization.centralDirectoryOffset) {
      throw new TypeError('Task finalization coordinates are inconsistent')
    }
  }
}

function validateRanges(ranges: readonly TaskRange[], exactSize: bigint): void {
  let end = 0n
  for (const range of ranges) {
    uint64(range.start, 'range start')
    uint64(range.end, 'range end')
    if (range.start < end || range.end <= range.start || range.end > exactSize ||
        !Number.isInteger(range.crc32) || range.crc32 < 0 || range.crc32 > 0xffff_ffff) {
      throw new TypeError('Task ranges must be disjoint, bounded CRC summaries')
    }
    end = range.end
  }
}

function validateLayout(value: TaskEntry, path: readonly string[], exactSize: bigint): ZipEntryPlanV1 | undefined {
  const layout = value.zipLayout
  let zipPlan: ZipEntryPlanV1 | undefined
  if (layout !== undefined) {
    for (const n of [layout.sequence, layout.localHeaderOffset, layout.payloadOffset,
      layout.exactSize, layout.descriptorOffset, layout.endOffset]) uint64(n, 'ZIP coordinate')
    if (layout.entryId !== value.entryId || layout.encodingVersion !== 1 ||
        layout.exactSize !== exactSize || layout.payloadOffset < layout.localHeaderOffset ||
        layout.descriptorOffset !== layout.payloadOffset + exactSize || layout.endOffset < layout.descriptorOffset) {
      throw new TypeError('Task ZIP layout is inconsistent')
    }
    if (value.zipPlan === undefined) throw new TypeError('Task ZIP allocation requires its encoding plan')
    zipPlan = validateZipEntryPlan(value.zipPlan, layout.localHeaderOffset)
    if (zipPlan.kind !== value.kind || JSON.stringify(zipPlan.path) !== JSON.stringify(path) ||
        zipPlan.exactSize !== exactSize ||
        layout.payloadOffset !== layout.localHeaderOffset + zipPlan.localHeaderBytes ||
        layout.endOffset !== layout.localHeaderOffset + zipPlan.entryStreamBytes) {
      throw new TypeError('Task ZIP encoding plan disagrees with fixed coordinates')
    }
  }
  return zipPlan
}
