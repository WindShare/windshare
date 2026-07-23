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
import type { OutputSession } from '../transfer/output-session'

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
