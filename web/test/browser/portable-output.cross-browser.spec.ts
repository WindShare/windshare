import { expect, test } from '@playwright/test'

const CROSS_BROWSER_FILE_NAME = 'portable-cross-browser.bin'
const CROSS_BROWSER_BYTES = Uint8Array.of(0, 1, 2, 127, 128, 254, 255)

test('starts the same exact portable object-URL handoff in each supported engine', async ({
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
  }, { bytes: [...CROSS_BROWSER_BYTES], suggestedName: CROSS_BROWSER_FILE_NAME })

  expect(result).toEqual({
    kind: 'download-started',
    suggestedName: CROSS_BROWSER_FILE_NAME,
  })
  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe(CROSS_BROWSER_FILE_NAME)
  expect(await readDownload(download)).toEqual(Buffer.from(CROSS_BROWSER_BYTES))
})

test('supports immutable OPFS packaged File retries without weakening the URL lease', async ({
  page,
}) => {
  await page.goto('/')
  const packageReadSupported = await page.evaluate(() =>
    typeof navigator.storage?.getDirectory === 'function')
  if (!packageReadSupported) {
    const facts = await page.evaluate(async () => {
      const capabilityPath = '/src/output/capability/acquisition.ts'
      const capability = await import(capabilityPath) as typeof import(
        '../../src/output/capability/acquisition'
      )
      const snapshot = capability.probeBrowserEnvironment({
        browserHandoff: {
          Blob: window.Blob,
          WritableStream: window.WritableStream,
          URL: window.URL,
          document: window.document,
          setTimeout: window.setTimeout.bind(window),
          clearTimeout: window.clearTimeout.bind(window),
        },
      })
      return {
        workspace: snapshot.browserHandoff?.supportsWorkspacePackage,
        portable: snapshot.browserHandoff?.supportsPortableArtifact,
      }
    })
    expect(facts).toEqual({ workspace: false, portable: true })
    return
  }

  await page.evaluate(async ({ bytes, suggestedName }) => {
    const fixturePath = '/test/browser/portable-output-fixture.ts'
    const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')
    await fixture.preparePackagedFileRetries(suggestedName, bytes)
  }, {
    bytes: [...CROSS_BROWSER_BYTES],
    suggestedName: 'packaged-cross-browser.bin',
  })

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
      expect(download.suggestedFilename()).toBe('packaged-cross-browser.bin')
      expect(await readDownload(download)).toEqual(Buffer.from(CROSS_BROWSER_BYTES))
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
    expect(proof.started.result.retryableUntil).toBe(proof.retryableUntil)
    expect(proof.started.urlLeaseEndsAt - proof.started.urlLeaseStartedAt).toBe(60_000)
  }
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
