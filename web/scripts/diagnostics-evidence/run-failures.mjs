/* global console, process */
import { chromium } from '@playwright/test'
import { createServer } from 'vite'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('../..', import.meta.url))
const server = await createServer({ root, server: { host: '127.0.0.1', port: 0 }, logLevel: 'error' })
let browser
try {
  await server.listen()
  browser = await chromium.launch({ headless: true })
  const results = []
  for (const traceEnabled of [false, true]) {
    const page = await browser.newPage()
    await page.goto(server.resolvedUrls.local[0] + 'scripts/diagnostics-evidence/index.html')
    await page.waitForFunction(() => typeof window.runFailureEvidenceWorkload === 'function')
    const result = await page.evaluate(enabled => window.runFailureEvidenceWorkload(enabled), traceEnabled)
    if (result.retainedBytes > 262144 || result.retainedRecords > 64 ||
        result.saves > 3 || result.retainedTrigger !== 'native write rejected') {
      throw new Error('Failure evidence workload exceeded its bounded contract: ' + JSON.stringify(result))
    }
    results.push(result)
    await page.close()
  }
  console.log(JSON.stringify({ browser: browser.version(), results }, null, 2))
} catch (error) {
  console.error(error)
  process.exitCode = 1
} finally {
  await browser?.close()
  await server.close()
}
