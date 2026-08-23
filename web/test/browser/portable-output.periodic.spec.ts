import { expect, test } from '@playwright/test'

import { ZIP_OUTPUT_METADATA_BUFFER_BYTES } from '../../src/output/streams/zip-output-sink'

const FULL_PORTABLE_STRESS_BYTES = 64 * 1024 * 1024
const CROSS_ENGINE_PORTABLE_STRESS_BYTES = 4 * 1024 * 1024
const WEEKLY_MILLION_MEMBER_WRITER_TIMEOUT_MILLISECONDS = 150_000
const WEEKLY_MILLION_MEMBER_PROGRESS_KIND = 'weekly-million-member-zip-progress'
// These storage probes need a same-origin document, not product-shell rendering work.
const BROWSER_CONTRACT_HOST_PATH = '/test/browser/contract-host.html'

test('streams one million ZIP members through the production writer and durable spool', async ({
  browserName,
  page,
}) => {
  // Other engines run the same production quota/fencing paths below; this single
  // structural stress avoids tripling a deliberately million-entry browser gate.
  test.skip(browserName !== 'chromium', 'The million-member structural stress runs once in Chromium')
  test.setTimeout(WEEKLY_MILLION_MEMBER_WRITER_TIMEOUT_MILLISECONDS)
  page.on('console', (message) => {
    const text = message.text()
    if (message.type() === 'info' && text.includes(WEEKLY_MILLION_MEMBER_PROGRESS_KIND)) {
      console.info(text)
    }
  })
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async () => {
    const probePath = '/test/browser/portable-output-periodic-probe.ts'
    const probe = await import(probePath) as typeof import('./portable-output-periodic-probe')
    return probe.probeMillionMemberZipWriter()
  })

  expect(result).toMatchObject({
    memberCount: 1_000_000,
    closed: true,
    afterClose: [0, 0],
  })
  const durableChunkCount = result.beforeClose[0]
  if (durableChunkCount === undefined) throw new Error('Durable ZIP chunk count is missing')
  expect(durableChunkCount).toBeGreaterThan(1_000)
  expect(result.beforeClose[1]).toBe(1)
  expect(result.outputBytes).toBeGreaterThan(0)
  const maximumEntryWrites = Math.ceil(
    result.entryStreamBytes / ZIP_OUTPUT_METADATA_BUFFER_BYTES,
  )
  expect(result.outputWritesBeforeClose).toBeLessThanOrEqual(maximumEntryWrites)
  expect(result.outputWrites).toBeLessThanOrEqual(
    maximumEntryWrites + durableChunkCount + 2,
  )
  expect(result.maximumWriteBytes).toBeLessThanOrEqual(ZIP_OUTPUT_METADATA_BUFFER_BYTES)
})

test('assembles the exact portable ceiling and rejects the first over-limit admission', async ({
  browserName,
  page,
}) => {
  test.setTimeout(120_000)
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const stressBytes = browserName === 'chromium'
    ? FULL_PORTABLE_STRESS_BYTES
    : CROSS_ENGINE_PORTABLE_STRESS_BYTES
  const result = await page.evaluate(async (exactBytes) => {
    const portablePath = '/src/output/portable/browser-download.ts'
    const fixturePath = '/test/browser/portable-output-fixture.ts'
    const portable = await import(portablePath) as typeof import(
      '../../src/output/portable/browser-download'
    )
    const fixture = await import(fixturePath) as typeof import('./portable-output-fixture')

    let maximumParts = 0
    let publishCount = 0
    const prepared = await fixture.createPortableBrowserFixture(
      'portable-periodic.bin',
      BigInt(exactBytes),
    )
    const session = await portable.openPortableHandoff({
      ...prepared,
      publisher: {
        handoff: (request) => {
          publishCount += 1
          return request.context.attemptKind === 'workspace'
            ? {
                kind: 'download-started' as const,
                suggestedName: request.suggestedName,
                retryableUntil: request.context.retryableUntil,
              }
            : {
                kind: 'download-started' as const,
                suggestedName: request.suggestedName,
              }
        },
      },
      assembly: {
        Blob: window.Blob,
        WritableStream: window.WritableStream,
        observeAssembly: (snapshot) => {
          maximumParts = Math.max(maximumParts, snapshot.retainedParts)
        },
      },
    })
    const writer = session.writable.getWriter()
    const chunk = new Uint8Array(256 * 1024)
    let written = 0
    while (written < exactBytes) {
      const count = Math.min(chunk.byteLength, exactBytes - written)
      await writer.write(count === chunk.byteLength ? chunk : chunk.slice(0, count))
      written += count
    }
    await writer.close()
    const terminal = await session.result

    const overLimit = await fixture.createPortableBrowserFixture(
      'portable-over-limit.bin',
      BigInt(portable.PORTABLE_HANDOFF_MAXIMUM_BYTES) + 1n,
    )
    let overLimitName = ''
    try {
      await portable.openPortableHandoff({
        ...overLimit,
        publisher: {
          handoff: () => {
            throw new Error('over-limit admission reached publisher')
          },
        },
        assembly: { Blob: window.Blob, WritableStream: window.WritableStream },
      })
    } catch (error) {
      overLimitName = error instanceof DOMException ? error.name : String(error)
    }

    return {
      terminal,
      publishCount,
      maximumParts,
      maximumAllowedParts: Math.ceil(exactBytes / portable.PORTABLE_HANDOFF_PART_BYTES),
      overLimitName,
      exactBytes,
    }
  }, stressBytes)

  expect(result.terminal).toEqual({
    kind: 'download-started',
    suggestedName: 'portable-periodic.bin',
  })
  expect(result.publishCount).toBe(1)
  expect(result.maximumParts).toBe(result.maximumAllowedParts)
  expect(result.overLimitName).toBe('NotSupportedError')
  expect(result.exactBytes).toBe(stressBytes)
})
