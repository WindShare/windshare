import type { BrowserReceiveWindow } from '../browser/environment'

interface PartialExportPicker {
  showSaveFilePicker(options: { suggestedName: string; types: readonly {
    description: string; accept: Readonly<Record<string, readonly string[]>>
  }[] }): Promise<FileSystemFileHandle>
}

export function supportsPartialExport(windowPort: BrowserReceiveWindow): boolean {
  return 'showSaveFilePicker' in windowPort && typeof windowPort.showSaveFilePicker === 'function'
}

/** Called synchronously in the explicit action stack so the picker keeps user activation. */
export function pickPartialExport(windowPort: BrowserReceiveWindow): Promise<FileSystemFileHandle> {
  if (!supportsPartialExport(windowPort)) {
    throw new DOMException('Saving a partial ZIP requires a file destination picker', 'NotSupportedError')
  }
  return (windowPort as unknown as PartialExportPicker).showSaveFilePicker({
    suggestedName: 'windshare-partial.zip',
    types: [{ description: 'Partial ZIP — fully received files only', accept: { 'application/zip': ['.zip'] } }],
  })
}
