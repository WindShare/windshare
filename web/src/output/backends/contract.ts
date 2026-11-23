import type { ArtifactChoice, ArtifactOffers, SaveBackendKind } from '../model/offers'
import type { ReceiveOperationDisplay } from '../model/operation-display'
import type { NativeSupportRuntime } from '../native-object/support'
import { probeNativeObjectSupport } from '../native-object/support'
import type { SelectionShape } from '../../transfer/discovery'
import type { TransferOutputSession } from '../../transfer/output-port'
import type { ResolvedArtifactSpec, SelectionSpec } from '../../transfer/selection'
import type { ReceiveLifecycleState, SafeRenamedSummary } from '../../transfer/state'
import type { TransferJobResult } from '../../transfer/job'
import type { ReceiveTaskRecord } from '../store/types'
import { supportsBrowserDownload, type BrowserDownloadWindow } from './browser-handoff'

const MEBIBYTE = 1024n * 1024n

/** Small complete single files prefer an instant browser download without picker/OPFS ceremony. */
export const SMALL_FILE_PORTABLE_THRESHOLD_BYTES = 16n * MEBIBYTE
/** Portable in-memory Blob assembly is capped to protect browser tab memory. */
export const PORTABLE_MAX_BYTES = 64n * MEBIBYTE

export interface BrowserOutputWindow extends BrowserDownloadWindow, NativeSupportRuntime {
  readonly indexedDB?: IDBFactory
  readonly navigator?: {
    readonly locks?: LockManager
    readonly storage?: StorageManager
  }
  readonly showDirectoryPicker?: (options?: { mode?: 'read' | 'readwrite' }) => Promise<FileSystemDirectoryHandle>
}

export interface BrowserOutputCapabilities {
  readonly indexedDb: boolean
  readonly fsa: boolean
  readonly opfs: boolean
  readonly portable: boolean
  readonly availableStorageBytes: bigint | null
}

export interface OutputSessionHandle {
  readonly taskId: string
  readonly backend: SaveBackendKind
  readonly transferOutput: TransferOutputSession
  /** Settles the backend after `TransferJob.run()` finishes and persists the final task record. */
  settle(result: TransferJobResult): Promise<ReceiveTaskRecord>
  /** Closes live handles and releases the cross-tab Web Lock without deleting retained data. */
  close(): Promise<void>
}

export interface PreparedFSAPicker {
  readonly picked: Promise<FileSystemDirectoryHandle>
}

export async function probeBrowserOutputCapabilities(
  windowPort: BrowserOutputWindow,
  signal?: AbortSignal,
): Promise<BrowserOutputCapabilities> {
  signal?.throwIfAborted()
  const indexedDb = typeof windowPort.indexedDB?.open === 'function'
  const portable = supportsBrowserDownload(windowPort)
  const fsa = indexedDb && typeof windowPort.showDirectoryPicker === 'function'
  const storage = windowPort.navigator?.storage
  const hasOpfsDir = typeof storage?.getDirectory === 'function'
  const nativeWorker = hasOpfsDir && indexedDb && portable
    ? await probeNativeObjectSupport(windowPort, signal)
    : false
  signal?.throwIfAborted()
  const opfs = nativeWorker && hasOpfsDir && indexedDb && portable
  const availableStorageBytes = opfs ? await estimateAvailableBytes(storage) : null
  signal?.throwIfAborted()
  return Object.freeze({ indexedDb, fsa, opfs, portable, availableStorageBytes })
}

export function planArtifactOffers(input: Readonly<{
  epoch: bigint
  selectionKey: string
  shape: SelectionShape
  capabilities: BrowserOutputCapabilities
}>): ArtifactOffers {
  const { epoch, selectionKey, shape, capabilities } = input
  if (shape.kind === 'empty') {
    return Object.freeze({ kind: 'selection-empty', epoch, selectionKey })
  }
  if (shape.kind === 'single-file') {
    return planSingleFileOffers(epoch, selectionKey, shape, capabilities)
  }
  return planTreeOffers(epoch, selectionKey, shape, capabilities)
}

function planSingleFileOffers(
  epoch: bigint,
  selectionKey: string,
  shape: Extract<SelectionShape, { kind: 'single-file' }>,
  caps: BrowserOutputCapabilities,
): ArtifactOffers {
  const artifact: ResolvedArtifactSpec = Object.freeze({
    kind: 'original-file',
    name: shape.file.name,
    singleFileId: shape.file.idText,
    singleFilePath: shape.sourcePath,
    singleFileSize: shape.exactBytes,
  })
  const fsaArtifact: ResolvedArtifactSpec = Object.freeze({
    kind: 'directory-tree',
    name: shape.file.name,
    singleFileId: shape.file.idText,
    singleFilePath: shape.sourcePath,
    singleFileSize: shape.exactBytes,
  })

  const choices: ArtifactChoice[] = []
  const fitsPortable = caps.portable && shape.exactBytes <= PORTABLE_MAX_BYTES
  const fitsOpfs = caps.opfs && (caps.availableStorageBytes === null || shape.exactBytes <= caps.availableStorageBytes)

  if (fitsOpfs) {
    choices.push(makeChoice('opfs:file', 'opfs', artifact, true, true, shape.exactBytes))
  }
  if (fitsPortable) {
    choices.push(makeChoice('portable:file', 'portable', artifact, false, choices.length === 0, shape.exactBytes))
  }
  if (caps.fsa) {
    choices.push(makeChoice('fsa:file', 'fsa', fsaArtifact, true, choices.length === 0, shape.exactBytes))
  }

  if (choices.length === 0) {
    return Object.freeze({
      kind: 'unavailable',
      epoch,
      selectionKey,
      reason: unavailableOfferReason(caps, fitsOpfs, shape.exactBytes),
    })
  }

  return Object.freeze({
    kind: 'ready',
    epoch,
    selectionKey,
    primary: choices[0]!,
    alternatives: Object.freeze(choices.slice(1)),
    zip: null,
  })
}

function planTreeOffers(
  epoch: bigint,
  selectionKey: string,
  shape: Extract<SelectionShape, { kind: 'tree' }>,
  caps: BrowserOutputCapabilities,
): ArtifactOffers {
  const folderArtifact: ResolvedArtifactSpec = Object.freeze({
    kind: 'directory-tree',
    name: shape.suggestedName,
  })
  const zipName = shape.suggestedName.toLowerCase().endsWith('.zip')
    ? shape.suggestedName
    : `${shape.suggestedName}.zip`
  const zipArtifact: ResolvedArtifactSpec = Object.freeze({
    kind: 'zip-archive',
    name: zipName,
  })

  const choices: ArtifactChoice[] = []
  if (caps.fsa) {
    choices.push(makeChoice('fsa:tree', 'fsa', folderArtifact, true, true, shape.knownBytes))
  }

  const fitsOpfs = caps.opfs && (caps.availableStorageBytes === null || shape.knownBytes <= caps.availableStorageBytes)
  const zip = selectTreeZipChoice(shape, caps, fitsOpfs, zipArtifact, choices.length === 0)
  if (zip !== null) choices.push(zip)
  if (shouldOfferQuickPortableZip(shape, caps, zip)) {
    choices.push(makeChoice('portable:zip', 'portable', zipArtifact, false, false, shape.knownBytes))
  }

  if (choices.length === 0) {
    return Object.freeze({
      kind: 'unavailable',
      epoch,
      selectionKey,
      reason: unavailableOfferReason(caps, fitsOpfs, shape.knownBytes),
    })
  }

  return Object.freeze({
    kind: 'ready',
    epoch,
    selectionKey,
    primary: choices[0]!,
    alternatives: Object.freeze(choices.slice(1)),
    zip,
  })
}

function selectTreeZipChoice(
  shape: Extract<SelectionShape, { kind: 'tree' }>,
  caps: BrowserOutputCapabilities,
  fitsOpfs: boolean,
  zipArtifact: ResolvedArtifactSpec,
  recommended: boolean,
): ArtifactChoice | null {
  if (fitsOpfs) {
    return makeChoice('opfs:zip', 'opfs', zipArtifact, true, recommended, shape.knownBytes)
  }
  if (caps.portable && (!shape.complete || shape.knownBytes <= PORTABLE_MAX_BYTES)) {
    return makeChoice('portable:zip', 'portable', zipArtifact, false, recommended, shape.knownBytes)
  }
  return null
}

function shouldOfferQuickPortableZip(
  shape: Extract<SelectionShape, { kind: 'tree' }>,
  caps: BrowserOutputCapabilities,
  zip: ArtifactChoice | null,
): boolean {
  return caps.opfs && caps.portable && zip?.backend === 'opfs' &&
    shape.complete && shape.knownBytes <= SMALL_FILE_PORTABLE_THRESHOLD_BYTES
}

function unavailableOfferReason(
  caps: BrowserOutputCapabilities,
  fitsOpfs: boolean,
  bytes: bigint,
): 'storage-full' | 'portable-limit-exceeded' | 'no-safe-destination' {
  if (caps.opfs && !fitsOpfs) return 'storage-full'
  if (caps.portable && bytes > PORTABLE_MAX_BYTES) return 'portable-limit-exceeded'
  return 'no-safe-destination'
}

function makeChoice(
  choiceId: string,
  backend: SaveBackendKind,
  artifact: ResolvedArtifactSpec,
  resumable: boolean,
  recommended: boolean,
  estimatedBytes: bigint,
): ArtifactChoice {
  return Object.freeze({
    choiceId,
    backend,
    artifactKind: artifact.kind,
    artifact,
    resumable,
    recommended,
    suggestedName: artifact.name,
    estimatedBytes,
  })
}

async function estimateAvailableBytes(storage: StorageManager | undefined): Promise<bigint | null> {
  if (typeof storage?.estimate !== 'function') return null
  try {
    const estimate = await storage.estimate()
    if (typeof estimate.quota !== 'number' || !Number.isFinite(estimate.quota)) return null
    const usage = typeof estimate.usage === 'number' && Number.isFinite(estimate.usage) ? estimate.usage : 0
    return BigInt(Math.max(0, Math.floor(estimate.quota - usage)))
  } catch {
    return null
  }
}

export interface StartBackendTaskInput {
  readonly taskId: string
  readonly selection: SelectionSpec
  readonly selectionKey: string
  readonly artifact: ResolvedArtifactSpec
  readonly lifecycle: ReceiveLifecycleState
  readonly display?: ReceiveOperationDisplay
  readonly safeRenamed?: SafeRenamedSummary
}
