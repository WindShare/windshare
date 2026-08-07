import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
  SINGLE_FILE_STREAM_BACKEND,
  ZIP_STREAM_BACKEND,
  createOutputCapabilityIdentity,
  snapshotOutputCapabilityIdentity,
  type OutputCapabilityFormat,
  type OutputCapabilityIdentity,
  type OutputCapabilityMetadata,
} from './contract'

const MEBIBYTE = 1024n * 1024n
// This threshold chooses staging durability; the final sink owns its independent capacity limit.
export const OPFS_STAGING_PREFERENCE_BYTES = 512n * MEBIBYTE
export const DEFAULT_ARCHIVE_NAME = 'windshare.zip'

interface SaveFilePickerOptions {
  readonly suggestedName?: string
}

export interface KnownSingleFileSelection {
  readonly kind: 'KnownSingleFile'
  readonly suggestedName: string
  readonly exactBytes: bigint
}

export interface ProgressiveSelection {
  readonly kind: 'Progressive'
  /** Undefined means recursive discovery has not established a terminal total. */
  readonly terminalBytes?: bigint
  readonly suggestedArchiveName?: string
}

export type OutputSelectionShape = KnownSingleFileSelection | ProgressiveSelection
export type OutputAcquisitionIntent = 'DirectoryTree' | 'BrowserDownload'

export type OutputRootIdentityResolver = (
  root: FileSystemDirectoryHandle,
  backend: string,
) => Promise<OutputCapabilityIdentity> | OutputCapabilityIdentity

export type OutputTargetIdentityFactory = (
  backend: string,
) => Promise<OutputCapabilityIdentity> | OutputCapabilityIdentity

export interface OutputCapabilityRuntime {
  showDirectoryPicker?: () => Promise<FileSystemDirectoryHandle>
  showSaveFilePicker?: (
    options?: SaveFilePickerOptions,
  ) => Promise<FileSystemFileHandle>
  getOriginPrivateDirectory?: () => Promise<FileSystemDirectoryHandle>
  createDirectStream?: (
    suggestedName: string,
    minimumBytes: bigint,
  ) => WritableStream<Uint8Array> | Promise<WritableStream<Uint8Array>>
  /**
   * Durable roots must be assigned by the owner of the root-binding store.  A
   * missing resolver is a construction error rather than an implicit fallback.
   */
  resolveRootIdentity?: OutputRootIdentityResolver
  /** Test adapters may inject deterministic stream identities. */
  createTargetIdentity?: OutputTargetIdentityFactory
}

export type AcquiredOutputCapability =
  | OutputCapabilityMetadata & {
    readonly kind: 'PersistentDirectory'
    readonly root: FileSystemDirectoryHandle
  }
  | OutputCapabilityMetadata & {
    readonly kind: 'SingleFileStream'
    readonly output: WritableStream<Uint8Array>
  }
  | OutputCapabilityMetadata & {
    readonly kind: 'ZipStream'
    readonly output: WritableStream<Uint8Array>
  }
  | OutputCapabilityMetadata & {
    readonly kind: 'OriginPrivateStaging'
    readonly root: FileSystemDirectoryHandle
    readonly output: WritableStream<Uint8Array>
  }

/**
 * Picker and stream-factory calls occur before this function returns. Later
 * progressive discovery can await the capability without retaining activation.
 */
export function acquireOutputCapability(
  intent: OutputAcquisitionIntent,
  selection: OutputSelectionShape,
  runtime: OutputCapabilityRuntime,
): Promise<AcquiredOutputCapability> {
  requireSelectionShape(selection)
  if (intent === 'DirectoryTree') {
    const picker = runtime.showDirectoryPicker
    if (picker === undefined) return unsupported('Directory output is unavailable')
    const picked = picker()
    return picked.then(async (root) => Object.freeze({
      kind: 'PersistentDirectory' as const,
      root,
      rootIdentity: await resolveRootIdentity(runtime, root, FILE_SYSTEM_ACCESS_BACKEND),
      targetKind: 2 as const,
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      format: 'directory' as const,
    }))
  }

  const suggestedName = selection.kind === 'KnownSingleFile'
    ? selection.suggestedName
    : selection.suggestedArchiveName ?? DEFAULT_ARCHIVE_NAME
  const shouldStage = selection.kind === 'Progressive' &&
    (selection.terminalBytes === undefined ||
      selection.terminalBytes >= OPFS_STAGING_PREFERENCE_BYTES)
  const savePicker = runtime.showSaveFilePicker
  if (savePicker !== undefined) {
    const picked = savePicker({ suggestedName })
    if (shouldStage && runtime.getOriginPrivateDirectory !== undefined) {
      const root = runtime.getOriginPrivateDirectory()
      return Promise.all([picked, root])
        .then(async ([handle, directory]) => Object.freeze({
          kind: 'OriginPrivateStaging' as const,
          root: directory,
          output: await handle.createWritable(),
          rootIdentity: await resolveRootIdentity(runtime, directory, ORIGIN_PRIVATE_BACKEND),
          targetKind: 2 as const,
          backend: ORIGIN_PRIVATE_BACKEND,
          format: 'zip' as const,
        }))
    }
    return picked
      .then((handle) => handle.createWritable())
      .then((output) => streamCapability(selection, output, runtime))
  }

  if (runtime.createDirectStream !== undefined) {
    const minimumBytes = selection.kind === 'KnownSingleFile'
      ? selection.exactBytes
      : selection.terminalBytes ?? 0n
    const output = runtime.createDirectStream(suggestedName, minimumBytes)
    if (shouldStage && runtime.getOriginPrivateDirectory !== undefined) {
      const root = runtime.getOriginPrivateDirectory()
      return Promise.all([output, root]).then(async ([stream, directory]) => Object.freeze({
        kind: 'OriginPrivateStaging' as const,
        root: directory,
        output: stream,
        rootIdentity: await resolveRootIdentity(runtime, directory, ORIGIN_PRIVATE_BACKEND),
        targetKind: 2 as const,
        backend: ORIGIN_PRIVATE_BACKEND,
        format: 'zip' as const,
      }))
    }
    return Promise.resolve(output).then((stream) => streamCapability(selection, stream, runtime))
  }
  return unsupported('No browser output capability is available')
}

function requireSelectionShape(selection: OutputSelectionShape): void {
  const bytes = selection.kind === 'KnownSingleFile'
    ? selection.exactBytes
    : selection.terminalBytes
  if (bytes !== undefined && bytes < 0n) {
    throw new RangeError('Output selection bytes must not be negative')
  }
  if (selection.kind === 'KnownSingleFile' && selection.suggestedName.length === 0) {
    throw new TypeError('Known single-file output requires a suggested name')
  }
}

function streamCapability(
  selection: OutputSelectionShape,
  output: WritableStream<Uint8Array>,
  runtime: OutputCapabilityRuntime,
): Promise<AcquiredOutputCapability> {
  const backend = selection.kind === 'KnownSingleFile'
    ? SINGLE_FILE_STREAM_BACKEND
    : ZIP_STREAM_BACKEND
  const format: OutputCapabilityFormat = selection.kind === 'KnownSingleFile'
    ? 'single-file'
    : 'zip'
  return createTargetIdentity(runtime, backend).then((rootIdentity) => Object.freeze({
    kind: selection.kind === 'KnownSingleFile'
      ? 'SingleFileStream' as const
      : 'ZipStream' as const,
    output,
    rootIdentity,
    // Browser streams are opaque output objects too; kind 1 is reserved for
    // canonical absolute filesystem paths owned by the native authority.
    targetKind: 2 as const,
    backend,
    format,
  }))
}

function resolveRootIdentity(
  runtime: OutputCapabilityRuntime,
  root: FileSystemDirectoryHandle,
  backend: string,
): Promise<OutputCapabilityIdentity> {
  if (runtime.resolveRootIdentity === undefined) {
    return Promise.reject(new TypeError(
      `Output capability ${backend} requires an owned root-identity resolver`,
    ))
  }
  return Promise.resolve(runtime.resolveRootIdentity(root, backend))
    .then((identity) => snapshotOutputCapabilityIdentity(identity, `${backend} root identity`))
}

function createTargetIdentity(
  runtime: OutputCapabilityRuntime,
  backend: string,
): Promise<OutputCapabilityIdentity> {
  const identity = runtime.createTargetIdentity === undefined
    ? createOutputCapabilityIdentity()
    : runtime.createTargetIdentity(backend)
  return Promise.resolve(identity)
    .then((value) => snapshotOutputCapabilityIdentity(value, `${backend} target identity`))
}

function unsupported(message: string): Promise<never> {
  return Promise.reject(new DOMException(message, 'NotSupportedError'))
}
