import { ByteRangeSet, type ByteRange } from '../../content/geometry'

export interface OutputSourceIdentity {
  readonly shareInstance: string
  readonly fileId: string
  readonly fileRevision: string
}

/** Storage adapters issue this only while the owned local object is accessible and verified. */
export interface ContentReplica {
  readonly backend: string
  readonly operationId: string
  readonly objectId: string
  readonly checkpointId: string
  readonly generation: bigint
  /** Source ranges are relative to the file, even when its replica is inside an archive. */
  readonly payloadOffset: bigint
}

export interface ContentAvailability {
  readonly source: OutputSourceIdentity
  readonly exactSize: bigint
  /** No replica means no reusable bytes, including after deletion or failed verification. */
  readonly replica: ContentReplica | null
  readonly ranges: readonly ByteRange[]
}

export function sameContentRevision(left: OutputSourceIdentity, right: OutputSourceIdentity): boolean {
  return left.shareInstance === right.shareInstance && left.fileId === right.fileId &&
    left.fileRevision === right.fileRevision
}

export function snapshotContentAvailability(input: ContentAvailability): ContentAvailability {
  for (const value of [input.source.shareInstance, input.source.fileId, input.source.fileRevision]) {
    if (typeof value !== 'string' || value.length === 0) throw new TypeError('Content source identity is missing')
  }
  const replica = input.replica
  if (replica === null && input.ranges.length !== 0) {
    throw new TypeError('Reusable ranges require a verified local replica')
  }
  if (replica !== null) {
    for (const value of [replica.backend, replica.operationId, replica.objectId, replica.checkpointId]) {
      if (typeof value !== 'string' || value.length === 0) throw new TypeError('Content replica identity is missing')
    }
    if (typeof replica.generation !== 'bigint' || replica.generation <= 0n ||
        typeof replica.payloadOffset !== 'bigint' || replica.payloadOffset < 0n) {
      throw new TypeError('Content replica commit coordinates are invalid')
    }
  }
  return Object.freeze({
    source: Object.freeze({ ...input.source }), exactSize: input.exactSize,
    replica: replica === null ? null : Object.freeze({ ...replica }),
    ranges: contentRangeSet(input.exactSize, input.ranges).ranges,
  })
}

/** An owned transaction cannot silently replace the replica or reuse an older commit. */
export function assertContentAvailabilityAdvance(previous: ContentAvailability, next: ContentAvailability): void {
  if (!sameContentRevision(previous.source, next.source) || previous.exactSize !== next.exactSize) {
    throw new TypeError('Content checkpoint changed its authenticated revision')
  }
  const before = previous.replica
  const after = next.replica
  if (before === null) return
  if (after === null || before.backend !== after.backend || before.operationId !== after.operationId ||
      before.objectId !== after.objectId || before.checkpointId !== after.checkpointId ||
      before.payloadOffset !== after.payloadOffset || after.generation < before.generation) {
    throw new TypeError('Content checkpoint changed its replica or regressed its generation')
  }
  if (after.generation === before.generation && (previous.ranges.length !== next.ranges.length ||
      previous.ranges.some((range, index) => range.start !== next.ranges[index]?.start || range.end !== next.ranges[index]?.end))) {
    throw new TypeError('Content ranges changed without a new checkpoint generation')
  }
}

/** CRC segments may be adjacent; ranges are canonicalized without changing their payload meaning. */
export function contentRangeSet(exactSize: bigint, ranges: readonly ByteRange[]): ByteRangeSet {
  if (typeof exactSize !== 'bigint' || exactSize < 0n) throw new TypeError('Content size is invalid')
  let end = 0n
  for (const range of ranges) {
    if (typeof range.start !== 'bigint' || typeof range.end !== 'bigint' ||
        range.start < end || range.end <= range.start || range.end > exactSize) {
      throw new TypeError('Content ranges must be sorted, disjoint, and bounded by their revision')
    }
    end = range.end
  }
  return new ByteRangeSet(exactSize, ranges)
}

export function reusableContentBytes(content: ContentAvailability): bigint {
  return content.replica === null ? 0n : content.ranges.reduce((sum, range) => sum + range.end - range.start, 0n)
}

export function contentIsComplete(exactSize: bigint, ranges: readonly ByteRange[]): boolean {
  return contentRangeSet(exactSize, ranges).covers({ start: 0n, end: exactSize })
}

export function reusableContentPrefix(content: ContentAvailability, source: OutputSourceIdentity): bigint {
  if (content.replica === null || !sameContentRevision(content.source, source)) return 0n
  return content.ranges[0]?.start === 0n ? content.ranges[0].end : 0n
}
