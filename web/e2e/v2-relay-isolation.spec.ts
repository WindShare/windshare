import { randomBytes } from 'node:crypto'
import { writeFile } from 'node:fs/promises'
import { test } from '@playwright/test'

import { capabilityUrl, DirectProductStack } from './fixtures/direct-product-stack'
import { ISOLATION_BLOCK_BYTES, ISOLATION_FILE_BYTES, runRelayIsolationScenario } from './fixtures/relay-isolation-scenario'

const DOWNLOAD_TIMEOUT_MILLISECONDS = 45_000

test('receiver interruptions preserve the sender and healthy sibling connections', async ({ browser, page }, testInfo) => {
  const stack = new DirectProductStack('receiver-disconnect-isolation')
  await stack.start()
  try {
    const payload = randomBytes(ISOLATION_FILE_BYTES)
    const file = await stack.createFile('isolation.bin', payload)
    const share = await stack.share(file, { blockSizeBytes: ISOLATION_BLOCK_BYTES, senderTrace: true })
    await runRelayIsolationScenario(browser, page, testInfo, {
      url: capabilityUrl(share), payload, senderTrace: () => stack.senderTraceArtifact(share),
      timeoutMilliseconds: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
  } finally {
    await writeFile(testInfo.outputPath('processes.json'), JSON.stringify(stack.diagnostic(), null, 2))
    await page.close()
    await stack.dispose()
  }
})
