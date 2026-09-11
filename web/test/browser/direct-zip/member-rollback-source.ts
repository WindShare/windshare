import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../../src/catalog/v2-records'
import { FileGeometry } from '../../../src/content/geometry'
import type { V2BlockRangeReader } from '../../../src/content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../../../src/content/v2-session-services'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  DirectZipOrderedCoordinatorV1, transferDirectZipFileV1, type DirectZipOrderedFileV1,
} from '../../../src/transfer/direct-zip'
import {
  snapshotLogicalArtifactPath, snapshotMaterializationRootRelativePath, snapshotSourceAuthenticationPath,
} from '../../../src/transfer/job/coordinate/direct-tree'
import type { DirectResumableZipExecution } from '../../../src/transfer/output-session'

export const MEMBER_ROLLBACK_BLOCK_BYTES = 3
export const MEMBER_ROLLBACK_PAUSE = new Error('Pause after the active member first block')
export type MemberRollbackRevisionMode = 'unchanged-revision' | 'identical-content' | 'changed-content'
const bytesId = (fill: number) => new Uint8Array(16).fill(fill)
const textId = (fill: number) => encodeBase64Url(bytesId(fill))
const FIRST_PAYLOAD = Uint8Array.of(11, 12, 13)
const ACTIVE_PAYLOAD = Uint8Array.of(21, 22, 23, 24, 25, 26)
const CHANGED_PAYLOAD = Uint8Array.of(31, 32, 33, 34, 35, 36)
const LAST_PAYLOAD = Uint8Array.of(41, 42, 43)

export function memberRollbackSource(rootId: string, mode: MemberRollbackRevisionMode) {
  const payloads = [FIRST_PAYLOAD, ACTIVE_PAYLOAD, LAST_PAYLOAD]
  const members = ['first.txt', 'active.txt', 'last.txt'].map((name, index) =>
    member(name, index + 10, BigInt(payloads[index]!.byteLength), rootId))
  const descriptor: V2ShareDescriptor = {
    wireVersion: 2, suite: 2, shareInstance: bytesId(3), shareInstanceId: textId(3),
    syntheticRoot: bytesId(2), syntheticRootId: rootId, chunkSize: MEMBER_ROLLBACK_BLOCK_BYTES,
    capabilities: 7n, senderPublicKey: new Uint8Array(32).fill(1), createdAtSeconds: 1n,
    pathPolicy: V2_PATH_POLICY,
  }
  const root = { directoryId: rootId, generation: textId(7),
    discoveryEvidence: new TextEncoder().encode('authenticated-root-generation') }
  const opens: { phase: string; name: string; revision: string }[] = []
  const ranges: { phase: string; name: string; start: string; end: string }[] = []
  const initialDurable: { phase: string; name: string; offset: string }[] = []
  const expected = [FIRST_PAYLOAD, mode === 'changed-content' ? CHANGED_PAYLOAD : ACTIVE_PAYLOAD, LAST_PAYLOAD]
  return {
    members, root, opens, ranges, initialDurable,
    expected: expected.map((payload, index) => ({
      name: members[index]!.artifactPath.join('/'), bytes: Array.from(payload),
    })),
    run: (execution: DirectResumableZipExecution, phase: 'initial' | 'resumed', signal: AbortSignal) => {
      const revisions: V2RevisionReader = {
        open: async fileId => {
          const index = members.findIndex(candidate => candidate.fileId === encodeBase64Url(fileId))
          if (index < 0) throw new Error('Requested a file outside the authenticated catalog')
          const revision = index === 1 && phase === 'resumed' && mode !== 'unchanged-revision' ? 30 : index + 20
          const opened = openedRevision(members[index]!, descriptor, revision)
          opens.push({ phase, name: members[index]!.sourcePath[0]!, revision: opened.descriptor.fileRevisionText })
          return opened
        },
      }
      const broker: V2BlockRangeReader = {
        readRange: async function* (opened, _lease, range) {
          const index = members.findIndex(candidate => candidate.fileId === opened.fileIdText)
          if (index < 0) throw new Error('Requested content outside the authenticated catalog')
          ranges.push({ phase, name: members[index]!.sourcePath[0]!,
            start: range.start.toString(), end: range.end.toString() })
          if (phase === 'initial' && index === 1 && range.start === BigInt(MEMBER_ROLLBACK_BLOCK_BYTES)) {
            throw MEMBER_ROLLBACK_PAUSE
          }
          const payload = phase === 'initial' ? payloads[index]! : expected[index]!
          yield { offset: range.start, data: payload.slice(Number(range.start), Number(range.end)) }
        },
      }
      return new DirectZipOrderedCoordinatorV1({
        source: { root: async () => root, members: async function* () { yield* members } },
        output: execution.ordered, signal,
        observeSelectedFile: () => undefined, observeReplayedFile: () => undefined,
        finishMeasure: () => ({ discoveredFiles: members.length, discoveredBytes: 12n,
          discovery: 'complete', sizeClass: 'small' }),
        transferFile: (file, transferSignal) => transferDirectZipFileV1({
          descriptor, output: execution.output, signal: transferSignal, revisions, broker,
          onInitialDurable: offset => { initialDurable.push({
            phase, name: file.sourcePath[0]!, offset: offset.toString(),
          }) },
          onWriteAcknowledged: () => undefined, onComplete: () => undefined,
        }, file),
      }).run()
    },
  }
}

function member(name: string, fill: number, expectedSize: bigint, rootId: string): DirectZipOrderedFileV1 {
  const sourcePath = [name]
  const artifactPath = ['shared', name]
  const entry = { kind: 'file' as const, id: bytesId(fill), idText: textId(fill), name, expectedSize }
  return {
    kind: 'file', fileId: entry.idText, expectedSize, sourcePath, artifactPath,
    layoutEvidence: new TextEncoder().encode('layout:' + name),
    discoveryEvidence: new TextEncoder().encode('member:' + name),
    pending: {
      entry, sourceAuthenticationPath: snapshotSourceAuthenticationPath(sourcePath),
      logicalArtifactPath: snapshotLogicalArtifactPath(artifactPath),
      materializationRelativePath: snapshotMaterializationRootRelativePath(sourcePath),
      parent: { kind: 'reference', directoryId: rootId, generation: textId(7),
        sourceAuthenticationPath: snapshotSourceAuthenticationPath([]),
        logicalArtifactPath: snapshotLogicalArtifactPath(['shared']) },
      ready: Promise.resolve(),
    },
  }
}

function openedRevision(file: DirectZipOrderedFileV1, share: V2ShareDescriptor, revision: number): V2OpenedRevision {
  return {
    descriptor: {
      shareInstance: share.shareInstance, shareInstanceId: share.shareInstanceId,
      fileId: file.pending.entry.id, fileIdText: file.fileId,
      fileRevision: bytesId(revision), fileRevisionText: textId(revision),
      exactSize: file.expectedSize, geometry: new FileGeometry(file.expectedSize, BigInt(MEMBER_ROLLBACK_BLOCK_BYTES)),
    },
    leaseId: bytesId(revision + 10), release: async () => undefined,
  }
}
