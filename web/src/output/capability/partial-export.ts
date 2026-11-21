import type { BrowserReceiveWindow } from '../browser/environment'
import { browserSupportsObjectUrlHandoff } from '../portable/browser-download'

export const DEFAULT_PARTIAL_EXPORT_NAME = 'windshare-partial.zip'

interface PartialExportPicker {
  showSaveFilePicker(options: { suggestedName: string; types: readonly {
    description: string; accept: Readonly<Record<string, readonly string[]>>
  }[] }): Promise<FileSystemFileHandle>
}

export type PartialExportDestination =
  | Readonly<{ kind: 'file-handle'; handle: FileSystemFileHandle }>
  | Readonly<{ kind: 'browser-download'; windowPort: BrowserReceiveWindow; suggestedName?: string }>

export function supportsPartialExportPicker(windowPort: BrowserReceiveWindow): boolean {
  return 'showSaveFilePicker' in windowPort && typeof windowPort.showSaveFilePicker === 'function'
}

export function supportsPartialExportHandoff(windowPort: BrowserReceiveWindow): boolean {
  return browserSupportsObjectUrlHandoff(windowPort) && typeof windowPort.Blob === 'function'
}

export function supportsPartialExport(windowPort: BrowserReceiveWindow): boolean {
  return supportsPartialExportPicker(windowPort) || supportsPartialExportHandoff(windowPort)
}

export function partialExportSuggestedName(artifactName?: string): string {
  const trimmed = artifactName?.trim()
  if (!trimmed) return DEFAULT_PARTIAL_EXPORT_NAME
  const stem = trimmed.toLowerCase().endsWith('.zip') ? trimmed.slice(0, -4).trim() : trimmed
  return stem.length === 0 ? DEFAULT_PARTIAL_EXPORT_NAME : `${stem}-partial.zip`
}

/** Called synchronously in the explicit action stack so the file picker keeps user activation. */
export function preparePartialExport(windowPort: BrowserReceiveWindow): Promise<PartialExportDestination> {
  if (supportsPartialExportPicker(windowPort)) {
    return (windowPort as unknown as PartialExportPicker).showSaveFilePicker({
      suggestedName: DEFAULT_PARTIAL_EXPORT_NAME,
      types: [{ description: 'Partial ZIP — fully received files only', accept: { 'application/zip': ['.zip'] } }],
    }).then(handle => Object.freeze({ kind: 'file-handle' as const, handle }))
  }
  if (supportsPartialExportHandoff(windowPort)) {
    return Promise.resolve(Object.freeze({ kind: 'browser-download' as const, windowPort }))
  }
  return Promise.reject(new DOMException(
    'Saving a partial ZIP requires either a file destination picker or browser download handoff',
    'NotSupportedError',
  ))
}
