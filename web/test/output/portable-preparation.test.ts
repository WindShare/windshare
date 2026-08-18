import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { browserHandoffOffer } from '../../src/output/capability/acquisition'
import {
  createPortableExecutionRoutes,
  type PortableExecutionLifecycleAuthority,
} from '../../src/output/portable/preparation'
import type {
  BrowserHandoffPublisher,
  DownloadStarted,
  PortableArtifactAdmission,
} from '../../src/output/portable/browser-download'
import type {
  PortableEnvironmentOffer,
} from '../../src/output/planning'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import { MemoryZipCentralDirectorySpool } from './zip-spool-fake'
import type { ZipArchiveWriter } from '../../src/output/streams/zip-archive'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortableHandoffPlan,
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createZipArchiveArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import type {
  ExactPreparationEvidence,
  OutputFileRequest,
} from '../../src/transfer/output-session'
import { createV2PlanExecutionAuthority } from '../../src/transfer/settlement/v2-plan-authority'

const ORIGINAL_BYTES = Uint8Array.of(1, 2, 3)
const MODIFIED_TIME = Object.freeze({
  seconds: 1_700_000_000n,
  nanoseconds: 123_000_000,
  precision: 3 as const,
})
const TAMPERED_MODIFIED_TIME = Object.freeze({
  ...MODIFIED_TIME,
  nanoseconds: MODIFIED_TIME.nanoseconds + 1,
})
const ROOT_GENERATION = identity(20)
const EMPTY_GENERATION = identity(21)

describe('portable exact-preparation execution routes', () => {
  it('issues a bound original admission and settles only after DownloadStarted', async () => {
    const intent = await originalIntent()
    const evidence = originalEvidence(intent, 3n)
    const downloads: Blob[] = []
    const lifecycle = lifecycleAuthority()
    const routes = createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(30),
      publisher: recordingPublisher(downloads),
      assembly: { Blob, WritableStream },
      lifecycle,
    })
    const authority = await createV2PlanExecutionAuthority({
      intent,
      routes: {
        ...routes,
        lifecycle: unopenedLifecycle(intent),
      },
    })
    const prepared = await authority.preparePortable(
      portableIntent(intent),
      evidence,
      new AbortController().signal,
    )
    expect(prepared.kind).toBe('accepted')
    if (prepared.kind !== 'accepted') return

    const request = outputRequest(intent, evidence.entries[0]!, ORIGINAL_BYTES)
    expect(prepared.execution.output.capabilities.modificationTime).toBe(false)
    expect(request.modifiedTime).toBeUndefined()
    const opened = await prepared.execution.output.beginFile(
      request,
      new AbortController().signal,
    )
    await opened.transaction.writeRange(0n, ORIGINAL_BYTES, new AbortController().signal)
    await opened.transaction.commit(new AbortController().signal)
    const state = await prepared.execution.settle({
      transferJobId: identity(31),
      worker: successfulWorker(),
      materialization: materialization(evidence),
    }, new AbortController().signal)

    expect(state).toMatchObject({
      kind: 'download-started',
      attemptKind: 'portable',
      attemptId: identity(30),
    })
    expect(downloads).toHaveLength(1)
    expect(new Uint8Array(await downloads[0]!.arrayBuffer())).toEqual(ORIGINAL_BYTES)
    expect(lifecycle.recordDownloadStarted).toHaveBeenCalledOnce()
    expect(lifecycle.recordDownloadStarted).toHaveBeenCalledWith(
      expect.objectContaining({
        admission: expect.objectContaining({
          sealedArtifact: expect.objectContaining({ artifactKind: 'original-file' }),
          exactArtifactBytes: 3n,
        }),
      }),
      expect.any(AbortSignal),
    )
  })

  it('adapts sealed ZIP order, empty directories, and close proof without recomputing bytes', async () => {
    const intent = await zipIntent()
    const evidence = zipEvidence(intent)
    const downloads: Blob[] = []
    const spool = new MemoryZipCentralDirectorySpool()
    const routes = createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(32),
      publisher: recordingPublisher(downloads),
      assembly: { Blob, WritableStream },
      lifecycle: lifecycleAuthority(),
      createZipSpool: () => spool,
    })
    const authority = await createV2PlanExecutionAuthority({
      intent,
      routes: {
        ...routes,
        lifecycle: unopenedLifecycle(intent),
      },
    })
    const prepared = await authority.preparePortable(
      portableIntent(intent),
      evidence,
      new AbortController().signal,
    )
    expect(prepared.kind).toBe('accepted')
    if (prepared.kind !== 'accepted') return

    const file = evidence.entries.find(entry => entry.kind === 'file')
    if (file?.kind !== 'file') throw new TypeError('test preparation lost its file')
    const request = outputRequest(intent, file, ORIGINAL_BYTES)
    expect(prepared.execution.output.capabilities.modificationTime).toBe(false)
    expect(request.modifiedTime).toBeUndefined()
    const opened = await prepared.execution.output.beginFile(
      request,
      new AbortController().signal,
    )
    await opened.transaction.writeRange(0n, ORIGINAL_BYTES, new AbortController().signal)
    await opened.transaction.commit(new AbortController().signal)
    const state = await prepared.execution.settle({
      transferJobId: identity(33),
      worker: successfulWorker(),
      materialization: materialization(evidence),
    }, new AbortController().signal)

    expect(state.kind).toBe('download-started')
    expect(downloads).toHaveLength(1)
    expect(spool.cleared).toBe(true)
    const archive = new Uint8Array(await downloads[0]!.arrayBuffer())
    expect(BigInt(archive.byteLength)).toBeGreaterThan(BigInt(ORIGINAL_BYTES.byteLength))
    const encodedNames = new TextDecoder().decode(archive)
    expect(encodedNames).toContain(`${intent.artifact.kind === 'zip-archive'
      ? intent.artifact.layout.name
      : ''}/`)
    expect(encodedNames).toContain('empty/')
    expect(encodedNames).toContain('file.bin')
  })

})

describe('portable exact-preparation binding and failure boundaries', () => {
  it('keeps zero-byte OriginalFile timestamps in the sealed preparation evidence', async () => {
    const intent = await originalIntent()
    const evidence = originalEvidence(intent, 0n)
    const entry = evidence.entries[0]
    if (entry?.kind !== 'file') throw new TypeError('test preparation lost its file')
    const changedEvidence: ExactPreparationEvidence = Object.freeze({
      ...evidence,
      entries: Object.freeze([
        Object.freeze({ ...entry, modifiedTime: TAMPERED_MODIFIED_TIME }),
      ]),
    })

    const downloadManifestDigest = async (
      currentEvidence: ExactPreparationEvidence,
      attemptId: string,
    ): Promise<string> => {
      const admissions: PortableArtifactAdmission[] = []
      const downloads: Blob[] = []
      const prepared = await createPortableExecutionRoutes({
        environment: supportedEnvironment(),
        attemptId,
        publisher: recordingPublisher(downloads),
        assembly: { Blob, WritableStream },
        lifecycle: lifecycleAuthority(admissions),
      }).portableOriginal!.prepare(
        originalPortableIntent(intent),
        currentEvidence,
        new AbortController().signal,
      )
      expect(prepared.kind).toBe('accepted')
      if (prepared.kind !== 'accepted') throw new Error('portable original was not accepted')
      const currentEntry = currentEvidence.entries[0]
      if (currentEntry?.kind !== 'file') throw new TypeError('test preparation lost its file')
      const request = outputRequest(intent, currentEntry, new Uint8Array())
      expect(request.modifiedTime).toBeUndefined()
      expect(prepared.execution.output.capabilities.modificationTime).toBe(false)
      const opened = await prepared.execution.output.beginFile(
        request,
        new AbortController().signal,
      )
      await opened.transaction.commit(new AbortController().signal)
      const state = await prepared.execution.settle({
        transferJobId: identity(48),
        worker: successfulWorker(),
        materialization: materialization(currentEvidence),
      }, new AbortController().signal)

      expect(state.kind).toBe('download-started')
      expect(downloads).toHaveLength(1)
      expect(downloads[0]!.size).toBe(0)
      const admission = admissions[0]
      if (admission === undefined) throw new Error('DownloadStarted lost portable admission')
      return admission.sealedArtifact.preparationManifestDigest
    }

    const originalDigest = await downloadManifestDigest(evidence, identity(46))
    const changedDigest = await downloadManifestDigest(changedEvidence, identity(47))
    expect(changedDigest).not.toBe(originalDigest)
  })

  it('surfaces ZIP cleanup uncertainty as NeedsAttention without resumability', async () => {
    const intent = await zipIntent()
    const evidence = zipEvidence(intent)
    const lifecycle = lifecycleAuthority()
    const routes = createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(39),
      publisher: recordingPublisher([]),
      assembly: { Blob, WritableStream },
      lifecycle,
      createZipSpool: () => new MemoryZipCentralDirectorySpool(),
      createZipWriter: () => failingAbortWriter(),
    })
    const prepared = await routes.portableZip!.prepare(
      zipPortableIntent(intent),
      evidence,
      new AbortController().signal,
    )
    expect(prepared.kind).toBe('accepted')
    if (prepared.kind !== 'accepted') return
    const file = evidence.entries.find(entry => entry.kind === 'file')
    if (file?.kind !== 'file') throw new TypeError('test preparation lost its file')
    await prepared.execution.output.beginFile(
      outputRequest(intent, file, ORIGINAL_BYTES),
      new AbortController().signal,
    )

    const state = await prepared.execution.pause({
      worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
      materialization: {
        entryCount: 0n,
        fileCount: 0n,
        directoryCount: evidence.directoryCount,
        rawBytes: 0n,
      },
      reason: new DOMException('cancelled', 'AbortError'),
    }, new AbortController().signal)

    expect(state).toMatchObject({
      kind: 'needs-attention',
      reason: 'cleanup-unknown',
    })
    expect(lifecycle.recordAbort).toHaveBeenCalledWith(
      expect.objectContaining({ cleanup: 'unknown' }),
      expect.any(AbortSignal),
    )
  })

  it('rejects over-limit preparation before constructing output or opening content', async () => {
    const exactSize = DEFAULT_PORTABLE_ARTIFACT_LIMIT + 1n
    const intent = await originalIntent()
    const evidence = originalEvidence(intent, exactSize)
    const downloads: Blob[] = []
    const lifecycle = lifecycleAuthority()
    const routes = createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(34),
      publisher: recordingPublisher(downloads),
      assembly: { Blob, WritableStream },
      lifecycle,
    })
    const result = await routes.portableOriginal!.prepare(
      originalPortableIntent(intent),
      evidence,
      new AbortController().signal,
    )

    expect(result.kind).toBe('rejected')
    expect(downloads).toEqual([])
    expect(lifecycle.rejectAdmission).toHaveBeenCalledWith(
      expect.objectContaining({ reason: 'artifact-limit' }),
      expect.any(AbortSignal),
    )
  })

  it('rejects supplied modified times that conflict with sealed OriginalFile or ZIP evidence', async () => {
    const original = await originalIntent()
    const originalEvidenceValue = originalEvidence(original, BigInt(ORIGINAL_BYTES.byteLength))
    const originalDownloads: Blob[] = []
    const originalPrepared = await createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(44),
      publisher: recordingPublisher(originalDownloads),
      assembly: { Blob, WritableStream },
      lifecycle: lifecycleAuthority(),
    }).portableOriginal!.prepare(
      originalPortableIntent(original),
      originalEvidenceValue,
      new AbortController().signal,
    )
    expect(originalPrepared.kind).toBe('accepted')
    if (originalPrepared.kind !== 'accepted') return
    const originalEntry = originalEvidenceValue.entries[0]
    if (originalEntry?.kind !== 'file') throw new TypeError('test preparation lost its file')
    const originalOpenRevision = vi.fn(
      async () => revision(original, originalEntry.fileId, originalEntry.exactSize),
    )

    await expect(originalPrepared.execution.output.beginFile({
      ...outputRequest(original, originalEntry, ORIGINAL_BYTES),
      modifiedTime: TAMPERED_MODIFIED_TIME,
      openRevision: originalOpenRevision,
    }, new AbortController().signal)).rejects.toThrow(
      /does not match exact portable preparation/u,
    )
    expect(originalOpenRevision).not.toHaveBeenCalled()
    expect(originalDownloads).toEqual([])

    const zip = await zipIntent()
    const zipEvidenceValue = zipEvidence(zip)
    const zipDownloads: Blob[] = []
    const zipPrepared = await createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(45),
      publisher: recordingPublisher(zipDownloads),
      assembly: { Blob, WritableStream },
      lifecycle: lifecycleAuthority(),
      createZipSpool: () => new MemoryZipCentralDirectorySpool(),
    }).portableZip!.prepare(
      zipPortableIntent(zip),
      zipEvidenceValue,
      new AbortController().signal,
    )
    expect(zipPrepared.kind).toBe('accepted')
    if (zipPrepared.kind !== 'accepted') return
    const zipEntry = zipEvidenceValue.entries.find(entry => entry.kind === 'file')
    if (zipEntry?.kind !== 'file') throw new TypeError('test preparation lost its file')
    const zipOpenRevision = vi.fn(
      async () => revision(zip, zipEntry.fileId, zipEntry.exactSize),
    )

    await expect(zipPrepared.execution.output.beginFile({
      ...outputRequest(zip, zipEntry, ORIGINAL_BYTES),
      modifiedTime: TAMPERED_MODIFIED_TIME,
      openRevision: zipOpenRevision,
    }, new AbortController().signal)).rejects.toThrow(
      /does not match exact portable preparation/u,
    )
    expect(zipOpenRevision).not.toHaveBeenCalled()
    expect(zipDownloads).toEqual([])
  })

  it('fails reordered or missing ZIP members before DownloadStarted', async () => {
    const intent = await zipIntent()
    const evidence = zipEvidence(intent)
    const downloads: Blob[] = []
    const lifecycle = lifecycleAuthority()
    const routes = createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(35),
      publisher: recordingPublisher(downloads),
      assembly: { Blob, WritableStream },
      lifecycle,
      createZipSpool: () => new MemoryZipCentralDirectorySpool(),
    })
    const prepared = await routes.portableZip!.prepare(
      zipPortableIntent(intent),
      evidence,
      new AbortController().signal,
    )
    expect(prepared.kind).toBe('accepted')
    if (prepared.kind !== 'accepted') return
    const file = evidence.entries.find(entry => entry.kind === 'file')
    if (file?.kind !== 'file') throw new TypeError('test preparation lost its file')
    const openRevision = vi.fn(async () => revision(intent, file.fileId, file.exactSize))
    const wrong = {
      ...outputRequest(intent, file, ORIGINAL_BYTES),
      artifactPath: Object.freeze([
        ...(file.artifactPath.slice(0, -1)),
        'wrong.bin',
      ]),
      openRevision,
    }

    await expect(prepared.execution.output.beginFile(
      wrong,
      new AbortController().signal,
    )).rejects.toThrow(/does not match exact portable preparation/u)
    expect(openRevision).not.toHaveBeenCalled()
    expect(downloads).toEqual([])

    const second = await createPortableExecutionRoutes({
      environment: supportedEnvironment(),
      attemptId: identity(36),
      publisher: recordingPublisher(downloads),
      assembly: { Blob, WritableStream },
      lifecycle,
      createZipSpool: () => new MemoryZipCentralDirectorySpool(),
    }).portableZip!.prepare(
      zipPortableIntent(intent),
      evidence,
      new AbortController().signal,
    )
    expect(second.kind).toBe('accepted')
    if (second.kind !== 'accepted') return
    await expect(second.execution.settle({
      transferJobId: identity(37),
      worker: successfulWorker(),
      materialization: materialization(evidence),
    }, new AbortController().signal)).rejects.toMatchObject({
      restartReason: 'preparation-invalidated',
    })
    expect(downloads).toEqual([])
  })

  it('suppresses unsupported routes and never installs a fallback', () => {
    const environment = supportedEnvironment(false)
    const routes = createPortableExecutionRoutes({
      environment,
      attemptId: identity(38),
      publisher: { handoff: vi.fn() } as unknown as BrowserHandoffPublisher,
      assembly: { Blob, WritableStream },
      lifecycle: lifecycleAuthority(),
      createZipSpool: () => new MemoryZipCentralDirectorySpool(),
    })
    expect(routes).toEqual({})
  })
})

function lifecycleAuthority(
  admissions?: PortableArtifactAdmission[],
): PortableExecutionLifecycleAuthority & {
  rejectAdmission: ReturnType<typeof vi.fn>
  recordDownloadStarted: ReturnType<typeof vi.fn>
  recordAbort: ReturnType<typeof vi.fn>
} {
  return {
    rejectAdmission: vi.fn(async ({ intent }) => discarded(intent)),
    recordDownloadStarted: vi.fn(async ({ intent, attemptId, admission }) => {
      admissions?.push(admission)
      return Object.freeze({
        kind: 'download-started' as const,
        operationId: intent.operationId,
        receiveIntentDigest: intent.digest,
        generation: 4n,
        attemptKind: 'portable' as const,
        attemptId,
      })
    }),
    recordAbort: vi.fn(async ({ intent, cleanup, reason }) => cleanup === 'unknown'
      ? Object.freeze({
          kind: 'needs-attention' as const,
          operationId: intent.operationId,
          receiveIntentDigest: intent.digest,
          generation: 4n,
          reason: 'cleanup-unknown' as const,
          lastVerifiedRecordDigest: identity(40, 32),
        })
      : Object.freeze({
          kind: 'restart-required' as const,
          operationId: intent.operationId,
          receiveIntentDigest: intent.digest,
          generation: 4n,
          reason,
          receiptDigest: identity(41, 32),
        })),
  }
}

function unopenedLifecycle(intent: ReceiveIntent) {
  return {
    settleExecutionAdmissionFailure: async () => discarded(intent),
    recordSettlementUnknown: async () => Object.freeze({
      kind: 'needs-attention' as const,
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      generation: 5n,
      reason: 'publication-unknown' as const,
      lastVerifiedRecordDigest: identity(42, 32),
    }),
  }
}

function discarded(intent: ReceiveIntent): Extract<
  ReceiveLifecycleState,
  { readonly kind: 'discarded' }
> {
  return Object.freeze({
    kind: 'discarded',
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 3n,
    cleanupReceiptDigest: identity(43, 32),
  })
}

function recordingPublisher(downloads: Blob[]): BrowserHandoffPublisher {
  return {
    handoff(request): DownloadStarted {
      downloads.push(request.source)
      return Object.freeze({
        kind: 'download-started',
        suggestedName: request.suggestedName,
      })
    },
  }
}

function failingAbortWriter(): ZipArchiveWriter {
  return {
    cleanupPending: false,
    cleanupFailure: undefined,
    addDirectory: async () => undefined,
    beginFile: async () => ({
      write: async () => undefined,
      close: async () => undefined,
      abort: async () => undefined,
    }),
    close: async () => undefined,
    abort: async () => { throw new Error('ZIP output cleanup failed') },
    retryCleanup: async () => undefined,
  }
}

function supportedEnvironment(
  supportsPortableArtifact = true,
): {
  portable: PortableEnvironmentOffer
  handoffTarget: ReturnType<typeof browserHandoffOffer>
} {
  return {
    portable: Object.freeze({
      id: 'portable-memory',
      kind: 'portable-memory',
      persistence: 'none',
      maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
      assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
      maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    }),
    handoffTarget: browserHandoffOffer({
      supportsWorkspacePackage: true,
      supportsPortableArtifact,
    }),
  }
}

async function originalIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createOriginalFileArtifact({
    fileId: identity(3),
    sourcePath: 'root/file.bin',
    suggestedName: 'file.bin',
  })
  const portable = await createPortableBinding({
    operationId: identity(4),
    portablePlanId: identity(5),
    artifact,
  })
  const plan = await createPortableHandoffPlan(artifact, portable)
  return createReceiveIntent({ selection, artifact, plan })
}

function originalEvidence(
  intent: ReceiveIntent,
  exactSize: bigint,
): ExactPreparationEvidence {
  if (intent.artifact.kind !== 'original-file') throw new TypeError('test intent is not original')
  const entry = Object.freeze({
    kind: 'file' as const,
    fileId: intent.artifact.fileId,
    containingDirectoryId: intent.syntheticRoot,
    generation: ROOT_GENERATION,
    sourcePath: Object.freeze(intent.artifact.sourcePath.split('/')),
    artifactPath: Object.freeze([intent.artifact.suggestedName]),
    exactSize,
    modifiedTime: MODIFIED_TIME,
  })
  return Object.freeze({
    generations: Object.freeze([{
      directoryId: intent.syntheticRoot,
      generation: ROOT_GENERATION,
    }]),
    entries: Object.freeze([entry]),
    entryCount: 1n,
    fileCount: 1n,
    directoryCount: 0n,
    selectedRawBytes: exactSize,
  })
}

async function zipIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const portable = await createPortableBinding({
    operationId: identity(6),
    portablePlanId: identity(7),
    artifact,
  })
  const plan = await createPortableHandoffPlan(artifact, portable)
  return createReceiveIntent({ selection, artifact, plan })
}

function zipEvidence(intent: ReceiveIntent): ExactPreparationEvidence {
  if (intent.artifact.kind !== 'zip-archive') throw new TypeError('test intent is not ZIP')
  const rootName = intent.artifact.layout.name
  const entries = Object.freeze([
    Object.freeze({
      kind: 'directory' as const,
      directoryId: intent.syntheticRoot,
      generation: ROOT_GENERATION,
      sourcePath: Object.freeze([]),
      artifactPath: Object.freeze([rootName]),
      role: 'result-root' as const,
    }),
    Object.freeze({
      kind: 'directory' as const,
      directoryId: identity(8),
      generation: EMPTY_GENERATION,
      sourcePath: Object.freeze(['empty']),
      artifactPath: Object.freeze([rootName, 'empty']),
      role: 'explicitly-selected-empty' as const,
    }),
    Object.freeze({
      kind: 'file' as const,
      fileId: identity(3),
      containingDirectoryId: intent.syntheticRoot,
      generation: ROOT_GENERATION,
      sourcePath: Object.freeze(['file.bin']),
      artifactPath: Object.freeze([rootName, 'file.bin']),
      exactSize: BigInt(ORIGINAL_BYTES.byteLength),
      modifiedTime: MODIFIED_TIME,
    }),
  ])
  return Object.freeze({
    generations: Object.freeze([
      Object.freeze({ directoryId: intent.syntheticRoot, generation: ROOT_GENERATION }),
      Object.freeze({ directoryId: identity(8), generation: EMPTY_GENERATION }),
    ]),
    entries,
    entryCount: 3n,
    fileCount: 1n,
    directoryCount: 2n,
    selectedRawBytes: BigInt(ORIGINAL_BYTES.byteLength),
  })
}

function outputRequest(
  intent: ReceiveIntent,
  entry: ExactPreparationEvidence['entries'][number],
  bytes: Uint8Array,
): OutputFileRequest {
  if (entry.kind !== 'file') throw new TypeError('test entry is not a file')
  return {
    source: {
      shareInstance: intent.shareInstance,
      fileId: entry.fileId,
    },
    sourcePath: entry.sourcePath,
    artifactPath: entry.artifactPath,
    expectedSize: entry.exactSize,
    openRevision: async () => revision(intent, entry.fileId, BigInt(bytes.byteLength)),
  }
}

function revision(intent: ReceiveIntent, fileId: string, exactSize: bigint) {
  return Object.freeze({
    shareInstance: intent.shareInstance,
    fileId,
    fileRevision: identity(50),
    exactSize,
  })
}

function successfulWorker() {
  return transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
}

function materialization(evidence: ExactPreparationEvidence) {
  return Object.freeze({
    entryCount: evidence.entryCount,
    fileCount: evidence.fileCount,
    directoryCount: evidence.directoryCount,
    rawBytes: evidence.selectedRawBytes,
  })
}

function portableIntent(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'portable-handoff') throw new TypeError('test intent is not portable')
  return intent as ReceiveIntent & Readonly<{ plan: typeof intent.plan }>
}

function originalPortableIntent(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'portable-handoff' || intent.artifact.kind !== 'original-file') {
    throw new TypeError('test intent is not portable original')
  }
  return intent as ReceiveIntent & Readonly<{
    plan: typeof intent.plan
    artifact: typeof intent.artifact
  }>
}

function zipPortableIntent(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'portable-handoff' || intent.artifact.kind !== 'zip-archive') {
    throw new TypeError('test intent is not portable ZIP')
  }
  return intent as ReceiveIntent & Readonly<{
    plan: typeof intent.plan
    artifact: typeof intent.artifact
  }>
}

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}
