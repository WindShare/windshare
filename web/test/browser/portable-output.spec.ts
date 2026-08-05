import { expect, test } from '@playwright/test'

test('runs the native picker adapter synchronously inside a real browser click handler', async ({
  page,
}) => {
  await page.goto('/')
  await page.evaluate(async () => {
    const modulePath = '/src/ui/v2-output.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const events: string[] = []
    Object.assign(window, { __windsharePickerEvents: events })

    const button = document.createElement('button')
    button.textContent = 'Acquire output'
    button.addEventListener('click', () => {
      events.push('handler')
      const runtime = {
        navigator: window.navigator,
        showSaveFilePicker: async () => {
          events.push('picker')
          return {
            createWritable: async () => new WritableStream<Uint8Array>(),
          } as FileSystemFileHandle
        },
      } as unknown as import('../../src/ui/v2-output').V2BrowserOutputWindow
      const acquired = output.acquireBrowserV2Output(
        'download',
        { kind: 'KnownSingleFile', suggestedName: 'matrix.bin', exactBytes: 1n },
        runtime,
      )
      events.push('returned')
      acquired.then(
        () => events.push('resolved'),
        () => events.push('rejected'),
      )
    })
    document.body.append(button)
  })

  const button = page.getByRole('button', { name: 'Acquire output' })
  await expect(button).toHaveCount(1)
  await button.click()
  await expect.poll(
    () => page.evaluate(() => (
      window as Window & { __windsharePickerEvents?: readonly string[] }
    ).__windsharePickerEvents),
  ).toEqual(['handler', 'picker', 'returned', 'resolved'])
})

test('rejects a declared portable output above the production memory bound', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const outputPath = '/src/ui/v2-output.ts'
    const portablePath = '/src/output/portable/browser-download.ts'
    const output = await import(outputPath) as typeof import('../../src/ui/v2-output')
    const portable = await import(portablePath) as typeof import(
      '../../src/output/portable/browser-download'
    )
    try {
      await output.acquireBrowserV2Output(
        'download',
        {
          kind: 'KnownSingleFile',
          suggestedName: 'too-large.bin',
          exactBytes: BigInt(portable.PORTABLE_DOWNLOAD_MAXIMUM_BYTES) + 1n,
        },
        {
          Blob: window.Blob,
          WritableStream: window.WritableStream,
          URL: window.URL,
          document: window.document,
          navigator: window.navigator,
          setTimeout: window.setTimeout.bind(window),
        },
      )
      return { name: '', message: '' }
    } catch (error) {
      return error instanceof DOMException
        ? { name: error.name, message: error.message }
        : { name: 'UnexpectedError', message: String(error) }
    }
  })

  expect(result).toEqual({
    name: 'NotSupportedError',
    message: 'Portable browser downloads are limited to 64 MiB',
  })
})
