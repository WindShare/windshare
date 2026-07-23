import type {
  OutputFile,
  OutputFileOwnership,
  OutputSessionIdentity,
} from '../../transfer/output-session'
import { VerifiedDurableRanges } from '../../transfer/output-session'
import type {
  PersistedFileRecord,
  PersistedOutputRecord,
} from '../persistence/journal'
import { outputPathKey } from '../persistence/journal'

export function fileOwnership(
  identity: OutputSessionIdentity,
  path: readonly string[],
  ownedFileIdentity: string,
): OutputFileOwnership {
  return Object.freeze({
    ...identity,
    canonicalPath: Object.freeze([...path]),
    ownedFileIdentity,
  })
}

export function verifiedFileRanges(record: PersistedFileRecord): VerifiedDurableRanges {
  return new VerifiedDurableRanges(
    record,
    record.source,
    record.exactSize,
    record.durableRanges,
  )
}

export function sameOutputSource(
  left: PersistedFileRecord['source'],
  right: OutputFile['source'],
): boolean {
  return left.shareInstance === right.shareInstance &&
    left.fileId === right.fileId &&
    left.fileRevision === right.fileRevision
}

export function fileKey(path: readonly string[]): string {
  return `file:${outputPathKey(path)}`
}

export function directoryKey(path: readonly string[]): string {
  return `directory:${outputPathKey(path)}`
}

export function nextGeneration(record: PersistedOutputRecord | undefined): bigint {
  return record === undefined ? 1n : record.generation + 1n
}
