import { expect, test } from '@playwright/test'

const PORTABLE_FILE_NAME = 'portable-contract.bin'
const PORTABLE_BYTES = Uint8Array.of(0, 1, 2, 127, 128, 254, 255)

test('hands an explicitly admitted portable artifact to the browser as DownloadStarted', async ({
  page,
}) => {
  await page.goto('/')
  const downloadPromise = page.waitForEvent('download')
  const result = await page.evaluate(async ({ bytes, suggestedName }) => {
    const portablePath = '/src/output/portable/browser-download.ts'
    const fixturePath = '/test/browser/portable-output-fixture.ts'
    const portable = await import(portablePath) as typeof import(
      '../../src/output/portable/browser-download'
    )
    const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')
    const prepared = await fixture.createPortableBrowserFixture(
      suggestedName,
      BigInt(bytes.length),
    )
    const session = await portable.openPortableBrowserHandoff({
      ...prepared,
      windowPort: fixture.currentPortableHandoffWindow(),
    })
    const writer = session.writable.getWriter()
    await writer.write(Uint8Array.from(bytes))
    await writer.close()
    return session.result
  }, { bytes: [...PORTABLE_BYTES], suggestedName: PORTABLE_FILE_NAME })

  expect(result).toEqual({
    kind: 'download-started',
    suggestedName: PORTABLE_FILE_NAME,
  })
  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe(PORTABLE_FILE_NAME)
  expect(await readDownload(download)).toEqual(Buffer.from(PORTABLE_BYTES))
})

test('retries one immutable OPFS package through fresh bounded File handoffs', async ({
  page,
}) => {
  await page.goto('/')
  await page.evaluate(async ({ bytes, suggestedName }) => {
    const fixturePath = '/test/browser/portable-output-fixture.ts'
    const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')
    await fixture.preparePackagedFileRetries(suggestedName, bytes)
  }, { bytes: [...PORTABLE_BYTES], suggestedName: 'packaged-retry.bin' })

  const proofs: Array<Awaited<ReturnType<
    typeof import('./portable-output-fixture').handoffNextPackagedFileRetry
  >>> = []
  try {
    for (let retry = 0; retry < 2; retry++) {
      const downloadPromise = page.waitForEvent('download')
      const proof = await page.evaluate(async () => {
        const fixturePath = '/test/browser/portable-output-fixture.ts'
        const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')
        return fixture.handoffNextPackagedFileRetry()
      })
      const download = await downloadPromise
      expect(download.suggestedFilename()).toBe('packaged-retry.bin')
      expect(await readDownload(download)).toEqual(Buffer.from(PORTABLE_BYTES))
      proofs.push(proof)
    }
  } finally {
    await page.evaluate(async () => {
      const fixturePath = '/test/browser/portable-output-fixture.ts'
      const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')
      await fixture.cleanupPackagedFileRetries()
    })
  }

  expect(proofs).toHaveLength(2)
  expect(proofs.every((proof) => proof.packageIdentityUnchanged)).toBe(true)
  expect(proofs.every((proof) => proof.sourceFileFresh)).toBe(true)
  expect(proofs.every((proof) => proof.immutableFileSource)).toBe(true)
  expect(proofs.every((proof) => proof.freshObjectUrl)).toBe(true)
  expect(proofs[0]!.packageDigest).toBe(proofs[1]!.packageDigest)
  expect(proofs[0]!.receiveIntentDigest).toBe(proofs[1]!.receiveIntentDigest)
  for (const proof of proofs) {
    expect(proof.started.result).toEqual({
      kind: 'download-started',
      suggestedName: 'packaged-retry.bin',
      retryableUntil: proof.retryableUntil,
    })
    expect(proof.started.urlLeaseEndsAt - proof.started.urlLeaseStartedAt).toBe(60_000)
  }
})

test('rejects an over-limit portable artifact before a browser handoff can start', async ({
  page,
}) => {
  await page.goto('/')
  const downloadNames: string[] = []
  page.on('download', (download) => downloadNames.push(download.suggestedFilename()))

  const result = await page.evaluate(async () => {
    const portablePath = '/src/output/portable/browser-download.ts'
    const fixturePath = '/test/browser/portable-output-fixture.ts'
    const portable = await import(portablePath) as typeof import(
      '../../src/output/portable/browser-download'
    )
    const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')
    const prepared = await fixture.createPortableBrowserFixture(
      'too-large.bin',
      BigInt(portable.PORTABLE_HANDOFF_MAXIMUM_BYTES) + 1n,
    )
    try {
      await portable.openPortableBrowserHandoff({
        ...prepared,
        windowPort: fixture.currentPortableHandoffWindow(),
      })
      return { name: '', message: '' }
    } catch (error) {
      return error instanceof DOMException
        ? { name: error.name, message: error.message }
        : { name: 'UnexpectedError', message: String(error) }
    }
  })

  expect(result).toEqual({
    name: 'NotSupportedError',
    message: 'Portable browser handoff is limited to 64 MiB',
  })
  await page.waitForTimeout(50)
  expect(downloadNames).toEqual([])
})

async function readDownload(
  download: import('@playwright/test').Download,
): Promise<Buffer> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks)
}
