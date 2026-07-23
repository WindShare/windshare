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
}

export type AcquiredOutputCapability =
  | {
    readonly kind: 'PersistentDirectory'
    readonly root: FileSystemDirectoryHandle
  }
  | {
    readonly kind: 'SingleFileStream'
    readonly output: WritableStream<Uint8Array>
  }
  | {
    readonly kind: 'ZipStream'
    readonly output: WritableStream<Uint8Array>
  }
  | {
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
    return picked.then((root) => Object.freeze({ kind: 'PersistentDirectory', root }))
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
        }))
    }
    return picked
      .then((handle) => handle.createWritable())
      .then((output) => streamCapability(selection, output))
  }

  if (runtime.createDirectStream !== undefined) {
    const minimumBytes = selection.kind === 'KnownSingleFile'
      ? selection.exactBytes
      : selection.terminalBytes ?? 0n
    const output = runtime.createDirectStream(suggestedName, minimumBytes)
    if (shouldStage && runtime.getOriginPrivateDirectory !== undefined) {
      const root = runtime.getOriginPrivateDirectory()
      return Promise.all([output, root]).then(([stream, directory]) => Object.freeze({
        kind: 'OriginPrivateStaging' as const,
        root: directory,
        output: stream,
      }))
    }
    return Promise.resolve(output).then((stream) => streamCapability(selection, stream))
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
): AcquiredOutputCapability {
  return Object.freeze(selection.kind === 'KnownSingleFile'
    ? { kind: 'SingleFileStream' as const, output }
    : { kind: 'ZipStream' as const, output })
}

function unsupported(message: string): Promise<never> {
  return Promise.reject(new DOMException(message, 'NotSupportedError'))
}
