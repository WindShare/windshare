import {
  acquireOutputCapability,
  type AcquiredOutputCapability,
  type OutputSelectionShape,
} from '../output/capability/acquisition'
import { acquireFileSystemAccessOutputSession } from '../output/file-system-access/session'
import { openOriginPrivateOutputSession } from '../output/origin-private/session'
import { OriginPrivateZipExporter } from '../output/origin-private/zip-exporter'
import {
  browserSupportsPortableDownload,
  createPortableBrowserDownload,
  type PortableDownloadWindow,
} from '../output/portable/browser-download'
import { SingleFileStreamOutputSession } from '../output/streams/single-file'
import { StreamingZipArchiveWriter } from '../output/streams/streaming-zip'
import { ZipStreamOutputSession } from '../output/streams/zip'
import { IndexedDbZipCentralDirectorySpool } from '../output/streams/zip-spool'
import type {
  OutputDirectory,
  OutputSession,
  V2OutputAuthority,
} from '../transfer/output-session'
import type { V2OutputSelection } from '../transfer/output-selection'

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
    },
  )
}

export function browserV2OutputAuthority(
  acquired: Promise<AcquiredOutputCapability>,
  sessions: V2BrowserOutputSessionFactory = DEFAULT_V2_BROWSER_OUTPUT_SESSION_FACTORY,
): V2OutputAuthority {
  return new BrowserV2OutputAuthority(acquired, sessions)
}

export interface V2BrowserOutputSessionFactory {
  open(capability: AcquiredOutputCapability, outputSessionId: string): Promise<OutputSession>
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

  async openSelection(
    selection: V2OutputSelection,
    signal: AbortSignal,
  ): Promise<OutputSession> {
    if (this.#state !== 'pending') throw new Error('Browser output authority can only be opened once')
    this.#state = 'opening'
    let capability: AcquiredOutputCapability | undefined
    let output: OutputSession | undefined
    try {
      capability = await awaitOutputCapability(this.#acquired, signal)
      signal.throwIfAborted()
      if (this.#wasAborted()) throw this.#abortReason
      output = await this.#sessions.open(capability, selection.resumeIntentText)
      for (const directory of selection.directories) {
        signal.throwIfAborted()
        await output.ensureDirectory(outputDirectory(directory, output))
      }
      signal.throwIfAborted()
      this.#state = 'opened'
      return output
    } catch (error) {
      this.#state = 'aborted'
      this.#abortReason = error
      if (output !== undefined) {
        await output.abortJob(error).catch(() => undefined)
      } else if (capability !== undefined) {
        await abortOutputCapability(capability, error)
      } else {
        this.#scheduleCapabilityAbort(error)
      }
      throw error
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

export async function openBrowserV2OutputSession(
  capability: AcquiredOutputCapability,
  outputSessionId: string,
): Promise<OutputSession> {
  switch (capability.kind) {
    case 'PersistentDirectory': {
      return acquireFileSystemAccessOutputSession(capability.root, { outputSessionId })
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
      try {
        return await openOriginPrivateOutputSession({
          outputSessionId,
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

function outputDirectory(
  directory: V2OutputSelection['directories'][number],
  output: OutputSession,
): OutputDirectory {
  return {
    path: directory.path,
    ...(directory.modifiedTime === undefined || !output.capabilities.modificationTime
      ? {}
      : { modifiedTimeMilliseconds: directory.modifiedTime.milliseconds }),
  }
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
