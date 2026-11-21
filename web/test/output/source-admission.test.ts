import { describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { RevisionReopenedDuringRecoveryError } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  canShortCircuitCompletedFile,
  evaluatePersistedSourceAdmission,
  persistedSourceAdmissionError,
} from '../../src/output/persistence/source-admission'
import { admitCheckpointMetadataReuse } from '../../src/output/persistence/metadata-reuse'
import {
  CheckpointLineageDecisionError,
  CheckpointSourceEvidenceUnavailableError,
  SourceRevisionChangedError,
} from '../../src/output/persistent-tree/errors'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { ProgressiveZipArchive } from '../../src/output/progressive-zip/archive'
import { createProgressiveZipOutput } from '../../src/output/progressive-zip/output'
import { ObjectCheckpointCoordinator } from '../../src/output/origin-private/native-object/coordinator'
import type { NativeObjectIO } from '../../src/output/origin-private/native-object/contracts'
import type { TaskCheckpoint, TaskDirectoryPin, TaskEntry } from '../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointCommit, TaskCheckpointStore } from '../../src/output/origin-private/task-checkpoint/store'
import { SelectionPolicy } from '../../src/catalog/selection'
import { FaultDomain, FaultScope, SourceFaultCode } from '../../src/transfer/failure/fault'
import { normalizeFileTransferFailure, transferFileOutcomeEvidence } from '../../src/transfer/job/failures'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'
import { deliveryIdentity } from './browser-delivery-fixture'
import { FILE_ID, materializationFixture, revision } from './persistent-tree-session-fixture'
import { identity } from './planning/fixture'
import { identity as bytesIdentity, identityText, receiveIntentFixture } from '../transfer/v2-job-fixture'

const META_A = identity(81, 32)
const META_B = identity(82, 32)
const REV_1 = identity(11)
const REV_2 = identity(12)

describe('persisted source admission contract', () => {
  it('enforces one completed-file short-circuit rule across all preconditions', () => {
    const base = {
      requestFileId: FILE_ID,
      requestExpectedSize: 6n,
      retainedFileId: FILE_ID,
      retainedExactSize: 6n,
      locallyComplete: true,
      coordinateMatches: true,
    }
    expect(canShortCircuitCompletedFile(base)).toBe(true)
    expect(canShortCircuitCompletedFile({ ...base, requestExpectedSize: undefined })).toBe(true)
    expect(canShortCircuitCompletedFile({ ...base, requireRevisionOpen: true })).toBe(false)
    expect(canShortCircuitCompletedFile({ ...base, requestFileId: undefined })).toBe(false)
    expect(canShortCircuitCompletedFile({ ...base, requestFileId: identity(99) })).toBe(false)
    expect(canShortCircuitCompletedFile({ ...base, requestExpectedSize: 7n })).toBe(false)
    expect(canShortCircuitCompletedFile({ ...base, locallyComplete: false })).toBe(false)
    expect(canShortCircuitCompletedFile({ ...base, hasBlockingState: true })).toBe(false)
    expect(canShortCircuitCompletedFile({ ...base, coordinateMatches: false })).toBe(false)
  })

  it('applies one canonical precedence order for persisted source comparison', () => {
    const previous = { fileRevision: REV_1, exactSize: 6n, sourceMetadata: META_A }
    expect(evaluatePersistedSourceAdmission({ previous, current: previous, allowsMetadataReuse: false }))
      .toEqual({ kind: 'same-revision' })
    expect(evaluatePersistedSourceAdmission({
      previous, current: { ...previous, fileRevision: REV_2 }, allowsMetadataReuse: true,
    })).toMatchObject({ kind: 'metadata-reuse', comparison: { kind: 'metadata-agreement' } })

    // Observed changes outrank state blocks; state blocks outrank missing metadata.
    expect(evaluatePersistedSourceAdmission({
      previous, current: { fileRevision: REV_2, exactSize: 6n, sourceMetadata: META_B }, allowsMetadataReuse: false,
    })).toMatchObject({ kind: 'source-changed', reason: 'metadata-changed' })
    expect(evaluatePersistedSourceAdmission({
      previous, current: { fileRevision: REV_2, exactSize: 6n }, allowsMetadataReuse: false,
    })).toEqual({ kind: 'reuse-state-blocked' })
    expect(evaluatePersistedSourceAdmission({
      previous, current: { fileRevision: REV_2, exactSize: 6n }, allowsMetadataReuse: true,
    })).toMatchObject({ kind: 'evidence-unavailable' })
    expect(evaluatePersistedSourceAdmission({
      previous, current: { fileRevision: REV_1, exactSize: 7n, sourceMetadata: META_A }, allowsMetadataReuse: true,
    })).toMatchObject({ kind: 'invalid-same-revision' })
    expect(evaluatePersistedSourceAdmission({
      previous, current: previous, allowsMetadataReuse: true, contextMatches: false,
    })).toEqual({ kind: 'source-changed', reason: 'context-changed' })
  })

  it('aligns persisted rejection FaultDomain with live RevisionReopenedDuringRecoveryError', () => {
    const liveDescriptor = (rev: number, sourceMetadata?: string) => Object.freeze({
      shareInstance: bytesIdentity(1),
      shareInstanceId: identityText(1),
      fileId: bytesIdentity(2),
      fileIdText: identityText(2),
      fileRevision: bytesIdentity(rev),
      fileRevisionText: identityText(rev),
      exactSize: 6n,
      geometry: new FileGeometry(6n, 65_536n),
      ...(sourceMetadata === undefined ? {} : { sourceMetadata }),
    })
    const liveChanged = normalizeFileTransferFailure(
      new RevisionReopenedDuringRecoveryError(liveDescriptor(1, META_A), liveDescriptor(2, META_B)),
    )
    const persistedChanged = normalizeFileTransferFailure(
      persistedSourceAdmissionError({ kind: 'source-changed', reason: 'metadata-changed' }),
    )
    expect(liveChanged.kind).toBe('fault')
    expect(persistedChanged.kind).toBe('fault')
    if (liveChanged.kind === 'fault' && persistedChanged.kind === 'fault') {
      expect(persistedChanged.fault).toEqual(liveChanged.fault)
      expect(persistedChanged.fault).toMatchObject({
        domain: FaultDomain.Source, scope: FaultScope.FileLocal, code: SourceFaultCode.RevisionChanged,
      })
      expect(transferFileOutcomeEvidence(persistedChanged.diagnostic)).toEqual({ kind: 'authenticated-source-drift' })
    }

    const unavailableError = persistedSourceAdmissionError({
      kind: 'evidence-unavailable', comparison: { kind: 'unavailable', reason: 'metadata-unavailable' },
    })
    expect(unavailableError).toBeInstanceOf(CheckpointSourceEvidenceUnavailableError)
    const unavailable = normalizeFileTransferFailure(unavailableError)
    expect(unavailable).toMatchObject({
      kind: 'fault',
      fault: { domain: FaultDomain.Source, scope: FaultScope.FileLocal, code: SourceFaultCode.Unavailable },
    })

    const blockedError = persistedSourceAdmissionError({ kind: 'reuse-state-blocked' })
    expect(blockedError).toBeInstanceOf(CheckpointLineageDecisionError)
    const blocked = normalizeFileTransferFailure(blockedError)
    expect(blocked).toMatchObject({
      kind: 'fault',
      fault: { domain: FaultDomain.Checkpoint, scope: FaultScope.FileLocal, code: 'unsafe-install' },
    })
  })

  it('keeps observed source changes in FaultDomain.Source across PersistentTree, BrowserDelivery, and ProgressiveZip', async () => {
    // 1. PersistentTree
    const treeFixture = await materializationFixture()
    const path = ['same.bin'] as const
    const initial = { ...revision(4n), sourceMetadata: META_A }
    const tx = await treeFixture.session.beginFile({ materializationRelativePath: path, openRevision: async () => initial })
    await tx.writeRange(0n, Uint8Array.of(1, 2))
    await tx.pause()
    let treeError: unknown
    try {
      await treeFixture.session.beginFile({
        materializationRelativePath: path,
        openRevision: async () => ({ ...initial, fileRevision: REV_2, sourceMetadata: META_B }),
      })
    } catch (error) { treeError = error }
    expect(treeError).toBeInstanceOf(SourceRevisionChangedError)
    expect(normalizeFileTransferFailure(treeError)).toMatchObject({
      fault: { domain: FaultDomain.Source, scope: FaultScope.FileLocal, code: SourceFaultCode.RevisionChanged },
    })
    await treeFixture.session.close()

    // 2. BrowserDelivery
    const deliveryFixture = await deliveryEngineFixture()
    const req = deliveryFixture.request(11, 4n)
    const src = { ...await req.openRevision(), sourceMetadata: deliveryIdentity(81) }
    const dtx = await deliveryFixture.session.beginFile({ ...req, openRevision: async () => src })
    await dtx.writeRange(0n, Uint8Array.of(1, 2))
    await dtx.pause()
    let deliveryError: unknown
    try {
      await deliveryFixture.session.beginFile({
        ...req,
        openRevision: async () => ({ ...src, fileRevision: deliveryIdentity(82, 16), sourceMetadata: deliveryIdentity(82) }),
      })
    } catch (error) { deliveryError = error }
    expect(deliveryError).toBeInstanceOf(SourceRevisionChangedError)
    await deliveryFixture.session.close()

    // 3. ProgressiveZip
    const zip = await openZipFixture()
    const zipSource = { shareInstance: zip.intent.shareInstance, directoryId: 'd', generation: 'g', sourcePath: ['same.bin'] }
    await zip.archive.admitFile({
      entryId: 'file:a', path: ['windshare', 'same.bin'], source: zipSource,
      revision: { fileId: 'a', fileRevision: REV_1, exactSize: 4n, sourceMetadata: META_A },
    })
    let zipError: unknown
    try {
      await zip.archive.admitFile({
        entryId: 'file:a', path: ['windshare', 'same.bin'], source: zipSource,
        revision: { fileId: 'a', fileRevision: REV_2, exactSize: 4n, sourceMetadata: META_B },
      })
    } catch (error) { zipError = error }
    expect(zipError).toBeInstanceOf(SourceRevisionChangedError)
    await zip.archive.close()
  })

  it('maps forced reopen of completed files to CheckpointLineageDecisionError(reuse-state-blocked) across all three backends', async () => {
    const path = ['done.bin'] as const
    const initial = { ...revision(4n), sourceMetadata: META_A }
    const reopened = { ...initial, fileRevision: REV_2 }

    // 1a. PersistentTree with semantic final receipt
    const semanticFixture = await materializationFixture()
    const stx = await semanticFixture.session.beginFile({
      fileId: FILE_ID, expectedSize: 4n, materializationRelativePath: path, openRevision: async () => initial,
    })
    await stx.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    await stx.commit()
    await expect(semanticFixture.session.beginFile({
      fileId: FILE_ID, expectedSize: 4n, materializationRelativePath: path,
      requireRevisionOpen: true, openRevision: async () => reopened,
    })).rejects.toMatchObject({ name: 'CheckpointLineageDecisionError', decision: 'reuse-state-blocked' })
    await semanticFixture.session.close()

    // 1b. PersistentTree checkpoint-only (no semantic journal)
    const cpOnly = await materializationFixture()
    await cpOnly.session.close()
    const workspaceSession = await PersistentTreeOutputSession.open({ tree: cpOnly.tree, checkpoints: cpOnly.checkpoints })
    const wtx = await workspaceSession.beginFile({
      fileId: FILE_ID, expectedSize: 4n, materializationRelativePath: path, openRevision: async () => initial,
    })
    await wtx.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    await wtx.pause()
    const committed = cpOnly.checkpoints.committed(FILE_ID)
    await expect(admitCheckpointMetadataReuse(committed, reopened, cpOnly.checkpoints, undefined))
      .rejects.toMatchObject({ name: 'CheckpointLineageDecisionError', decision: 'reuse-state-blocked' })
    await workspaceSession.close()

    // 2. ProgressiveZip completed entry forced reopen
    const zip = await openZipFixture()
    const zipSource = { shareInstance: zip.intent.shareInstance, directoryId: 'd', generation: 'g', sourcePath: ['done.bin'] }
    await zip.archive.admitFile({
      entryId: 'file:done', path: ['windshare', 'done.bin'], source: zipSource,
      revision: { fileId: 'done', fileRevision: REV_1, exactSize: 4n, sourceMetadata: META_A },
    })
    await zip.archive.writeRange('file:done', 0n, Uint8Array.of(1, 2, 3, 4))
    await zip.archive.checkpoint('complete')
    await expect(zip.archive.admitFile({
      entryId: 'file:done', path: ['windshare', 'done.bin'], source: zipSource,
      revision: { fileId: 'done', fileRevision: REV_2, exactSize: 4n, sourceMetadata: META_A },
    })).rejects.toMatchObject({ name: 'CheckpointLineageDecisionError', decision: 'reuse-state-blocked' })
    expect(zip.archive.failed).toBe(false)
    await zip.archive.close()
  })

  it('refuses completed ProgressiveZip short-circuit when catalog expectedSize changed or entry has revisionFailure', async () => {
    const zip = await openZipFixture()
    const { output, directories } = await createProgressiveZipOutput({
      archive: zip.archive, intent: zip.intent, identity: { backend: 'native-zip', outputSessionId: 'zip-sc' },
    })
    const signal = new AbortController().signal
    const root = snapshotMaterializationRootRelativePath(['windshare'])
    const parent = await directories.admitDirectory({
      directory: { directoryId: identityText(2), generation: identityText(3), path: root },
      sourceAuthenticationPath: snapshotSourceAuthenticationPath([]),
      logicalArtifactPath: snapshotLogicalArtifactPath(root),
    }, signal)
    const file = { shareInstance: zip.intent.shareInstance, fileId: identityText(16) }
    const path = [...root, 'done.bin']
    const firstOpen = vi.fn(async () => ({ ...file, fileRevision: REV_1, exactSize: 4n, sourceMetadata: META_A }))
    const baseRequest = {
      source: file, expectedSize: 4n, parentAdmission: parent, openRevision: firstOpen,
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(['done.bin']),
      logicalArtifactPath: snapshotLogicalArtifactPath(path),
      materializationRelativePath: snapshotMaterializationRootRelativePath(path),
    }
    const begun = await output.beginFile(baseRequest, signal)
    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4), signal)
    await begun.transaction.commit(signal)

    // Matching expectedSize short-circuits without calling openRevision.
    const untouchedOpen = vi.fn(async () => { throw new Error('must short-circuit') })
    const skipped = await output.beginFile({ ...baseRequest, openRevision: untouchedOpen }, signal)
    expect(untouchedOpen).not.toHaveBeenCalled()
    await skipped.transaction.commit(signal)

    // Changed expectedSize must NOT short-circuit; it opens the source and reports SourceRevisionChangedError.
    const resizedOpen = vi.fn(async () => ({ ...file, fileRevision: REV_2, exactSize: 8n, sourceMetadata: META_B }))
    await expect(output.beginFile({
      ...baseRequest, expectedSize: 8n, openRevision: resizedOpen,
    }, signal)).rejects.toBeInstanceOf(SourceRevisionChangedError)
    expect(resizedOpen).toHaveBeenCalledOnce()

    // Now entry carries revisionFailure; even with original expectedSize it must not silently short-circuit.
    const retryOpen = vi.fn(async () => ({ ...file, fileRevision: REV_2, exactSize: 4n, sourceMetadata: META_B }))
    await expect(output.beginFile({
      ...baseRequest, expectedSize: 4n, openRevision: retryOpen,
    }, signal)).rejects.toBeInstanceOf(SourceRevisionChangedError)
    expect(retryOpen).toHaveBeenCalledOnce()
    await zip.archive.close()
  })
})

async function openZipFixture() {
  const selection = new SelectionPolicy(true)
  const intent = await receiveIntentFixture({
    planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection,
  })
  let checkpoint: TaskCheckpoint | undefined
  const entries = new Map<string, TaskEntry>()
  const pins = new Map<string, TaskDirectoryPin>()
  const store: TaskCheckpointStore = {
    readCheckpoint: async () => checkpoint,
    readEntry: async id => entries.get(id),
    readEntries: async q => [...entries.values()]
      .filter(e => q.afterSequence === undefined || e.zipLayout!.sequence > q.afterSequence)
      .sort((a, b) => Number(a.zipLayout!.sequence - b.zipLayout!.sequence))
      .slice(0, q.limit),
    readDirectoryPin: async id => pins.get(id),
    readPath: async path => [...entries.values()].find(entry => entry.path.join('/') === path.join('/')),
    commit: async (input: TaskCheckpointCommit) => {
      checkpoint = input.checkpoint
      for (const entry of input.entries) entries.set(entry.entryId, entry)
      for (const pin of input.directoryPins ?? []) pins.set(pin.directoryId, pin)
    },
    close: () => undefined,
  }
  let data = new Uint8Array(0)
  const io: NativeObjectIO = {
    writeAt: async (offset, bytes) => {
      const end = Number(offset) + bytes.byteLength
      if (end > data.byteLength) {
        const next = new Uint8Array(end)
        next.set(data)
        data = next
      }
      data.set(bytes, Number(offset))
    },
    flush: async () => undefined,
    truncate: async length => { data = data.slice(0, Number(length)) },
    size: async () => BigInt(data.byteLength),
    close: async () => undefined,
  }
  const objectId = encodeBase64Url(new Uint8Array(16).fill(4))
  const archive = await ProgressiveZipArchive.open({
    store,
    coordinator: new ObjectCheckpointCoordinator({ io, operationId: intent.operationId, objectId }),
    capacity: { reserveGrowth: async () => ({ settle: async () => undefined, release: async () => undefined }) },
    object: {
      operationId: intent.operationId, objectId,
      handleId: encodeBase64Url(new Uint8Array(16).fill(5)), kind: 'zip-archive',
    },
    selectedPaths: [[]],
  })
  return { intent, archive }
}
