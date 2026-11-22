import { DIAGNOSTICS_MIME_TYPE, type DiagnosticFile } from './file'

const DOWNLOAD_URL_LIFETIME_MS = 60_000
const PLAIN_TEXT_MIME_TYPE = 'text/plain'

export interface DiagnosticsDelivery {
  supportsFileSharing(): boolean
  share(file: DiagnosticFile): Promise<'shared' | 'canceled'>
  save(file: DiagnosticFile): void
  copy(file: DiagnosticFile): Promise<void>
}

export interface DiagnosticsDeliveryBrowser {
  readonly navigator: Pick<Navigator, 'canShare' | 'share' | 'clipboard'>
  readonly document: Pick<Document, 'createElement' | 'body'>
  readonly urls: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>
  readonly defer: (callback: () => void, milliseconds: number) => void
}

export function createDiagnosticsDelivery(browser: DiagnosticsDeliveryBrowser): DiagnosticsDelivery {
  const shareable = (file: Pick<DiagnosticFile, 'name' | 'text'>): File | undefined => {
    const native = browser.navigator
    if (typeof native.share !== 'function' || typeof native.canShare !== 'function') return undefined
    // Mobile share targets often allow text attachments but reject NDJSON's extension.
    for (const candidate of [
      new File([file.text], file.name, { type: DIAGNOSTICS_MIME_TYPE }),
      new File([file.text], file.name + '.txt', { type: PLAIN_TEXT_MIME_TYPE }),
    ]) {
      try { if (native.canShare({ files: [candidate] })) return candidate } catch {
        // Capability probing is optional; downloading and manual copying remain usable.
      }
    }
    return undefined
  }
  return Object.freeze<DiagnosticsDelivery>({
    supportsFileSharing: () => shareable({ name: 'windshare-diagnostics.ndjson', text: '' }) !== undefined,
    share: async file => {
      const candidate = shareable(file)
      if (candidate === undefined) throw new Error('File sharing is unavailable')
      try {
        await browser.navigator.share({ files: [candidate], title: 'WindShare diagnostics' })
        return 'shared'
      } catch (error) {
        if (error instanceof Error && error.name === 'AbortError') return 'canceled'
        throw error
      }
    },
    save: file => {
      const url = browser.urls.createObjectURL(new Blob([file.text], { type: DIAGNOSTICS_MIME_TYPE }))
      const anchor = browser.document.createElement('a')
      try {
        anchor.href = url
        anchor.download = file.name
        anchor.hidden = true
        browser.document.body.append(anchor)
        anchor.click()
      } finally {
        anchor.remove()
        // Download handoff can outlive click(), particularly in mobile browsers.
        browser.defer(() => browser.urls.revokeObjectURL(url), DOWNLOAD_URL_LIFETIME_MS)
      }
    },
    copy: file => browser.navigator.clipboard.writeText(file.text),
  })
}
