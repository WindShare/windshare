import { describe, expect, it, vi } from 'vitest'
import { createDiagnosticsDelivery, type DiagnosticsDeliveryBrowser } from '../../../src/diagnostics/browser/delivery'
import type { DiagnosticFile } from '../../../src/diagnostics/browser/file'

const FILE: DiagnosticFile = {
  name: 'windshare-diagnostics.ndjson',
  runtimeRunId: 'AQAAAAAAAAAAAAAAAAAAAA',
  text: '{"line_type":"bundle_header"}\n',
}

describe('diagnostic file delivery', () => {
  it('shares only the diagnostic file, using a text attachment when NDJSON is unsupported', async () => {
    const browser = browserPort()
    browser.navigator.canShare.mockImplementation(data => data?.files?.[0]?.name.endsWith('.txt') ?? false)
    const delivery = createDiagnosticsDelivery(browser.port)
    expect(delivery.supportsFileSharing()).toBe(true)
    await expect(delivery.share(FILE)).resolves.toBe('shared')
    const request = browser.navigator.share.mock.calls[0]![0]!
    expect(request).not.toHaveProperty('url')
    expect(request.files?.[0]?.name).toBe(FILE.name + '.txt')
    expect(await request.files?.[0]?.text()).toBe(FILE.text)
  })

  it('treats cancel as cancel without starting an unwanted download', async () => {
    const browser = browserPort()
    browser.navigator.share.mockRejectedValue(new DOMException('canceled', 'AbortError'))
    await expect(createDiagnosticsDelivery(browser.port).share(FILE)).resolves.toBe('canceled')
    expect(browser.anchor.click).not.toHaveBeenCalled()
  })

  it('revokes download URLs after handoff and preserves the complete diagnostic content', async () => {
    const browser = browserPort()
    const delivery = createDiagnosticsDelivery(browser.port)
    delivery.save(FILE)
    expect(browser.anchor.download).toBe(FILE.name)
    expect(browser.anchor.click).toHaveBeenCalledOnce()
    const blob = browser.urls.createObjectURL.mock.calls[0]![0]
    expect(await (blob as Blob).text()).toBe(FILE.text)
    expect(browser.urls.revokeObjectURL).not.toHaveBeenCalled()
    browser.deferred[0]!()
    expect(browser.urls.revokeObjectURL).toHaveBeenCalledWith('blob:diagnostics')
    await delivery.copy(FILE)
    expect(browser.navigator.clipboard.writeText).toHaveBeenCalledWith(FILE.text)
  })

  it('isolates unsupported or denied native sharing from saving and copying', () => {
    const browser = browserPort()
    browser.navigator.canShare.mockImplementation(() => { throw new Error('denied') })
    const delivery = createDiagnosticsDelivery(browser.port)
    expect(delivery.supportsFileSharing()).toBe(false)
    expect(() => delivery.save(FILE)).not.toThrow()
  })
})

function browserPort() {
  const navigator = {
    canShare: vi.fn<(data?: ShareData) => boolean>(() => true),
    share: vi.fn<(data?: ShareData) => Promise<void>>(async () => undefined),
    clipboard: { writeText: vi.fn<(text: string) => Promise<void>>(async () => undefined) },
  }
  const anchor = { href: '', download: '', hidden: false, click: vi.fn(), remove: vi.fn() }
  const urls = { createObjectURL: vi.fn<(blob: Blob | MediaSource) => string>(() => 'blob:diagnostics'), revokeObjectURL: vi.fn() }
  const deferred: (() => void)[] = []
  const port = {
    navigator,
    document: { createElement: () => anchor, body: { append: vi.fn() } },
    urls,
    defer: (callback: () => void) => { deferred.push(callback) },
  } as unknown as DiagnosticsDeliveryBrowser
  return { port, navigator, anchor, urls, deferred }
}
