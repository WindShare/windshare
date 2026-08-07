import {
  acquireOutputCapability,
  type AcquiredOutputCapability,
  type OutputSelectionShape,
} from '../output/capability/acquisition'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  resolveIndexedDbRootIdentity,
} from '../output/browser/indexeddb-repository'
import {
  sameOutputCapabilityIdentity,
  snapshotOutputCapabilityIdentity,
  type OutputCapabilityIdentity,
} from '../output/capability/contract'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import {
  acquireFileSystemAccessOutputSession,
  FILE_SYSTEM_ACCESS_BACKEND,
} from '../output/file-system-access/session'
import {
  openOriginPrivateOutputSession,
  ORIGIN_PRIVATE_BACKEND,
} from '../output/origin-private/session'
import { OriginPrivateZipExporter } from '../output/origin-private/zip-exporter'
import {
  browserSupportsPortableDownload,
  createPortableBrowserDownload,
  type PortableDownloadWindow,
} from '../output/portable/browser-download'
import {
  SINGLE_FILE_STREAM_BACKEND,
  SingleFileStreamOutputSession,
} from '../output/streams/single-file'
import { StreamingZipArchiveWriter } from '../output/streams/streaming-zip'
import { ZIP_STREAM_BACKEND, ZipStreamOutputSession } from '../output/streams/zip'
import { IndexedDbZipCentralDirectorySpool } from '../output/streams/zip-spool'
import {
  validateOutputSessionBinding,
  type OutputSession,
  type V2OutputAuthority,
} from '../transfer/output-session'
import {
  freezeTransferIntent,
  validateFinalTransferIntent,
  type TransferIntent,
  type TransferIntentDraft,
  type TransferOutputLocator,
} from '../transfer/intent'

export type V2OutputIntent = 'directory' | 'download'

export interface V2BrowserOutputWindow extends PortableDownloadWindow {
  readonly navigator: Navigator
  showDirectoryPicker?: () => Promise<FileSystemDirectoryHandle>
  showSaveFilePicker?: (
    options?: { readonly suggestedName?: string },
  ) => Promise<FileSystemFileHandle>
}

interface OriginPrivateStorageManager {
  getDirectory?: () => Promise<FileSystemDirectoryHandle>
}

export interface V2OutputCapabilities {
  readonly nativeDirectory: boolean
  readonly nativeSave: boolean
  readonly portableDownload: boolean
  readonly originPrivateStaging: boolean
}

export function browserV2OutputCapabilities(
  windowPort: V2BrowserOutputWindow = window as unknown as V2BrowserOutputWindow,
): V2OutputCapabilities {
  const storage = windowPort.navigator.storage as unknown as
    | OriginPrivateStorageManager
    | undefined
  return Object.freeze({
    nativeDirectory: windowPort.showDirectoryPicker !== undefined,
    nativeSave: windowPort.showSaveFilePicker !== undefined,
    portableDownload: browserSupportsPortableDownload(windowPort),
    originPrivateStaging: storage?.getDirectory !== undefined,
  })
}

export function outputIntentAvailable(
  capabilities: V2OutputCapabilities,
  intent: V2OutputIntent,
): boolean {
  return intent === 'directory'
    ? capabilities.nativeDirectory
    : capabilities.nativeSave || capabilities.portableDownload
}

/** Picker invocation deliberately stays in this non-async function for activation ownership. */
export function acquireBrowserV2Output(
  intent: V2OutputIntent,
  selection: OutputSelectionShape,
  windowPort: V2BrowserOutputWindow = window as unknown as V2BrowserOutputWindow,
  identityProvider: BrowserV2OutputIdentityProvider = DEFAULT_BROWSER_V2_OUTPUT_IDENTITY_PROVIDER,
): Promise<AcquiredOutputCapability> {
  const storage = windowPort.navigator.storage as unknown as
    | OriginPrivateStorageManager
    | undefined
  const getOriginPrivateDirectory = storage?.getDirectory?.bind(storage)
  const portable = windowPort.showSaveFilePicker === undefined &&
    browserSupportsPortableDownload(windowPort)
  return acquireOutputCapability(
    intent === 'directory' ? 'DirectoryTree' : 'BrowserDownload',
    selection,
    {
      ...(windowPort.showDirectoryPicker === undefined
        ? {}
        : { showDirectoryPicker: () => windowPort.showDirectoryPicker!() }),
      ...(windowPort.showSaveFilePicker === undefined
        ? {}
        : { showSaveFilePicker: (options) => windowPort.showSaveFilePicker!(options) }),
      ...(getOriginPrivateDirectory === undefined
        ? {}
        : { getOriginPrivateDirectory }),
      ...(!portable
        ? {}
        : {
            createDirectStream: (name: string, minimumBytes: bigint) =>
              createPortableBrowserDownload(name, minimumBytes, windowPort),
          }),
      resolveRootIdentity: identityProvider.resolveRootIdentity,
      ...(identityProvider.createTargetIdentity === undefined
        ? {}
        : { createTargetIdentity: identityProvider.createTargetIdentity }),
    },
  )
}

/** Root identity services are infrastructure-owned; tests inject a deterministic fake. */
export interface BrowserV2OutputIdentityProvider {
  readonly resolveRootIdentity: (
    root: FileSystemDirectoryHandle,
    backend: string,
  ) => Promise<OutputCapabilityIdentity>
  readonly createTargetIdentity?: (backend: string) => OutputCapabilityIdentity
}

const DEFAULT_BROWSER_V2_OUTPUT_IDENTITY_PROVIDER: BrowserV2OutputIdentityProvider = Object.freeze({
  resolveRootIdentity: (
    root: FileSystemDirectoryHandle,
    backend: string,
  ) => resolveIndexedDbRootIdentity({
    databaseName: DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
    backend,
    root,
  }),
})

export function browserV2OutputAuthority(
  acquired: Promise<AcquiredOutputCapability>,
  sessions: V2BrowserOutputSessionFactory = DEFAULT_V2_BROWSER_OUTPUT_SESSION_FACTORY,
): V2OutputAuthority {
  return new BrowserV2OutputAuthority(acquired, sessions)
}

export interface V2BrowserOutputSessionFactory {
  /**
   * Durable browser targets must receive the frozen intent binding at the
   * production boundary. Stream targets do not persist checkpoints and may
   * leave the binding undefined.
   */
  open(
    capability: AcquiredOutputCapability,
    outputSessionId: string,
    binding?: V2OutputCheckpointBinding,
  ): Promise<OutputSession>
}

export interface V2OutputCheckpointBinding {
  readonly transferIntentDigest: string
  readonly rootIdentity: string
}

const DEFAULT_V2_BROWSER_OUTPUT_SESSION_FACTORY: V2BrowserOutputSessionFactory = Object.freeze({
  open: openBrowserV2OutputSession,
})

class BrowserV2OutputAuthority implements V2OutputAuthority {
  readonly #acquired: Promise<AcquiredOutputCapability>
  readonly #sessions: V2BrowserOutputSessionFactory
  #state: 'pending' | 'opening' | 'opened' | 'aborted' = 'pending'
  #abortReason: unknown
  #capabilityCleanup: Promise<void> | undefined

  constructor(
    acquired: Promise<AcquiredOutputCapability>,
    sessions: V2BrowserOutputSessionFactory,
  ) {
    this.#acquired = acquired
    this.#sessions = sessions
  }

  async confirmOutput(
    draft: TransferIntentDraft,
    signal: AbortSignal,
  ): Promise<{ readonly intent: TransferIntent; readonly session: OutputSession }> {
    this.#start()
    let capability: AcquiredOutputCapability | undefined
    let output: OutputSession | undefined
    try {
      capability = await awaitOutputCapability(this.#acquired, signal)
      signal.throwIfAborted()
      if (this.#wasAborted()) throw this.#abortReason
      // The picker result is the authority for backend and format. The draft is
      // intentionally not digestible until this branch has resolved.
      const intent = await freezeTransferIntent(draft, outputLocatorForCapability(capability))
      assertIntentMatchesCapability(intent, capability)
      output = await this.#openConfirmed(intent, capability, signal)
      this.#state = 'opened'
      return Object.freeze({ intent, session: output })
    } catch (error) {
      await this.#cleanup(error, capability, output)
      throw error
    }
  }

  async openOutput(
    intent: TransferIntent,
    signal: AbortSignal,
  ): Promise<OutputSession> {
    this.#start()
    let capability: AcquiredOutputCapability | undefined
    let output: OutputSession | undefined
    try {
      intent = await validateFinalTransferIntent(intent)
      capability = await awaitOutputCapability(this.#acquired, signal)
      signal.throwIfAborted()
      if (this.#wasAborted()) throw this.#abortReason
      assertIntentMatchesCapability(intent, capability)
      output = await this.#openConfirmed(intent, capability, signal)
      signal.throwIfAborted()
      this.#state = 'opened'
      return output
    } catch (error) {
      await this.#cleanup(error, capability, output)
      throw error
    }
  }

  #start(): void {
    if (this.#state !== 'pending') throw new Error('Browser output authority can only be opened once')
    this.#state = 'opening'
  }

  async #openConfirmed(
    intent: TransferIntent,
    capability: AcquiredOutputCapability,
    signal: AbortSignal,
  ): Promise<OutputSession> {
    // OutputSessionID is per run. The final intent digest, rather than a
    // terminal selection text, owns durable recovery identity.
    const binding = checkpointBindingFor(intent, capability)
    const output = await this.#sessions.open(
      capability,
      outputSessionId(intent.transferJobId),
      binding,
    )
    try {
      signal.throwIfAborted()
      // Root admission belongs to the transfer job because only authenticated
      // catalog discovery knows the committed root generation. Opening a picker
      // must never manufacture that generation from the directory identity.
      return validateOutputSessionBinding(intent, output)
    } catch (error) {
      await output.abortJob(error).catch(() => undefined)
      throw error
    }
  }

  async #cleanup(
    reason: unknown,
    capability: AcquiredOutputCapability | undefined,
    output: OutputSession | undefined,
  ): Promise<void> {
    this.#state = 'aborted'
    this.#abortReason = reason
    if (output !== undefined) {
      await output.abortJob(reason).catch(() => undefined)
    } else if (capability !== undefined) {
      await abortOutputCapability(capability, reason)
    } else {
      this.#scheduleCapabilityAbort(reason)
    }
  }

  abort(reason: unknown): Promise<void> {
    if (this.#state === 'opened' || this.#state === 'aborted') return Promise.resolve()
    this.#state = 'aborted'
    this.#abortReason = reason
    // A browser picker cannot be cancelled programmatically. Arrange cleanup
    // without making receiver cancellation wait for the user to dismiss it.
    this.#scheduleCapabilityAbort(reason)
    return Promise.resolve()
  }

  #wasAborted(): boolean {
    return this.#state === 'aborted'
  }

  #scheduleCapabilityAbort(reason: unknown): void {
    if (this.#capabilityCleanup !== undefined) return
    this.#capabilityCleanup = this.#acquired.then(
      (capability) => abortOutputCapability(capability, reason),
      () => undefined,
    ).then(() => undefined, () => undefined)
  }
}

/**
 * The picker returns a capability rather than a sender path.  Its persisted
 * root identity is the target locator; backend/format/kind are copied from the
 * same capability so a restored intent cannot select a different sink class.
 */
export function outputLocatorForCapability(
  capability: AcquiredOutputCapability,
): TransferOutputLocator {
  validateCapabilityDescriptor(capability)
  const identity = snapshotOutputCapabilityIdentity(
    capability.rootIdentity,
    `${capability.kind} root identity`,
  )
  return Object.freeze({
    target: encodeBase64Url(identity),
    targetKind: 2,
    backend: capability.backend,
    format: capability.format,
  })
}

function validateCapabilityDescriptor(capability: AcquiredOutputCapability): void {
  snapshotOutputCapabilityIdentity(capability.rootIdentity, `${capability.kind} root identity`)
  let expected: {
    readonly backend: string
    readonly format: 'directory' | 'single-file' | 'zip'
    readonly targetKind: 2
  }
  if (capability.kind === 'PersistentDirectory') {
    expected = { backend: FILE_SYSTEM_ACCESS_BACKEND, format: 'directory', targetKind: 2 }
  } else if (capability.kind === 'OriginPrivateStaging') {
    expected = { backend: ORIGIN_PRIVATE_BACKEND, format: 'zip', targetKind: 2 }
  } else if (capability.kind === 'SingleFileStream') {
    expected = { backend: SINGLE_FILE_STREAM_BACKEND, format: 'single-file', targetKind: 2 }
  } else {
    expected = { backend: ZIP_STREAM_BACKEND, format: 'zip', targetKind: 2 }
  }
  if (capability.backend !== expected.backend ||
      capability.format !== expected.format ||
      capability.targetKind !== expected.targetKind) {
    throw new TypeError('acquired output capability metadata does not match its kind')
  }
}

/**
 * OpenOutput and confirmOutput share this gate.  It is deliberately based on
 * the acquired capability's exact identity rather than a backend label: a
 * restored intent for another root, format, or target kind fails before the
 * session factory can touch durable state.
 */
function assertIntentMatchesCapability(
  intent: TransferIntent,
  capability: AcquiredOutputCapability,
): void {
  const expected = outputLocatorForCapability(capability)
  const actual = intent.output
  const actualTargetKind = actual.targetKind
  const target = decodeBase64Url(actual.target)
  const expectedTarget = decodeBase64Url(expected.target)
  if (target === undefined || expectedTarget === undefined ||
      !sameOutputCapabilityIdentity(target, expectedTarget) ||
      actual.backend !== expected.backend ||
      actual.format !== expected.format ||
      actualTargetKind !== expected.targetKind) {
    throw new TypeError('transfer intent output locator does not match the acquired output capability')
  }
}

function outputSessionId(transferJobId: string): string {
  const random = globalThis.crypto?.randomUUID?.()
  return random === undefined ? `${transferJobId}:run` : random
}

export async function openBrowserV2OutputSession(
  capability: AcquiredOutputCapability,
  outputSessionId: string,
  binding?: V2OutputCheckpointBinding,
): Promise<OutputSession> {
  validateCapabilityDescriptor(capability)
  switch (capability.kind) {
    case 'PersistentDirectory': {
      const durableBinding = requireCheckpointBinding(binding, 'directory')
      assertBindingMatchesCapability(durableBinding, capability)
      return acquireFileSystemAccessOutputSession(capability.root, {
        outputSessionId,
        transferIntentDigest: durableBinding.transferIntentDigest,
        rootIdentity: durableBinding.rootIdentity,
      })
    }
    case 'SingleFileStream':
      return new SingleFileStreamOutputSession(outputSessionId, capability.output)
    case 'ZipStream':
      return new ZipStreamOutputSession({
        outputSessionId,
        archive: new StreamingZipArchiveWriter(
          capability.output,
          new IndexedDbZipCentralDirectorySpool(),
        ),
      })
    case 'OriginPrivateStaging':
      {
      const durableBinding = requireCheckpointBinding(binding, 'origin-private staging')
      assertBindingMatchesCapability(durableBinding, capability)
      try {
        return await openOriginPrivateOutputSession({
          outputSessionId,
          transferIntentDigest: durableBinding.transferIntentDigest,
          rootIdentity: durableBinding.rootIdentity,
          storage: {
            getDirectory: async () => capability.root,
            estimate: () => navigator.storage.estimate(),
          },
          exporter: new OriginPrivateZipExporter(capability.output),
        })
      } catch (error) {
        await capability.output.abort(error).catch(() => undefined)
        throw error
      }
      }
  }
}

function assertBindingMatchesCapability(
  binding: V2OutputCheckpointBinding,
  capability: AcquiredOutputCapability,
): void {
  const expected = encodeBase64Url(capability.rootIdentity)
  if (binding.rootIdentity !== expected) {
    throw new TypeError('checkpoint root identity does not match the acquired output capability')
  }
}

function checkpointBindingFor(
  intent: TransferIntent,
  capability: AcquiredOutputCapability,
): V2OutputCheckpointBinding | undefined {
  if (capability.kind !== 'PersistentDirectory' && capability.kind !== 'OriginPrivateStaging') {
    return undefined
  }
  // The persistent output target is the backend-issued opaque root identity.
  // It is included in the frozen intent, so reopening with a different picker
  // root cannot silently join an existing checkpoint namespace.
  requireOpaqueIdentity(intent.digest, 'transfer intent digest')
  requireOpaqueIdentity(intent.output.target, 'output root identity')
  return {
    transferIntentDigest: intent.digest,
    rootIdentity: intent.output.target,
  }
}

function requireCheckpointBinding(
  binding: V2OutputCheckpointBinding | undefined,
  target: string,
): V2OutputCheckpointBinding {
  if (binding === undefined) {
    throw new Error(`Browser ${target} output requires a frozen transfer-intent/root binding`)
  }
  return {
    transferIntentDigest: requireOpaqueIdentity(binding.transferIntentDigest, 'transfer intent digest'),
    rootIdentity: requireOpaqueIdentity(binding.rootIdentity, 'output root identity'),
  }
}

function requireOpaqueIdentity(value: string, label: string): string {
  // SHA-256 identities are always unpadded base64url (32 bytes -> 43 chars).
  if (!/^[A-Za-z0-9_-]{43}$/u.test(value) || /^A{43}$/u.test(value)) {
    throw new TypeError(`${label} must be a non-zero 32-byte base64url identity`)
  }
  return value
}

function awaitOutputCapability(
  acquired: Promise<AcquiredOutputCapability>,
  signal: AbortSignal,
): Promise<AcquiredOutputCapability> {
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const aborted = () => reject(
      signal.reason ?? new DOMException('Output acquisition aborted', 'AbortError'),
    )
    signal.addEventListener('abort', aborted, { once: true })
    acquired.then(resolve, reject).finally(() => signal.removeEventListener('abort', aborted))
  })
}

async function abortOutputCapability(
  capability: AcquiredOutputCapability,
  reason: unknown,
): Promise<void> {
  if (capability.kind === 'PersistentDirectory') return
  await capability.output.abort(reason).catch(() => undefined)
}
