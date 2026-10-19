import { createHash } from 'node:crypto'
import { readFile, writeFile } from 'node:fs/promises'
import { expect, type Browser, type Download, type Page, type TestInfo } from '@playwright/test'

import { senderRecoveryEvidence, senderTraceSequence } from '../cloudflare/recovery-evidence'
import { installRelayIsolationFaults, type RelaySocketSnapshot } from './relay-isolation-faults'

export const ISOLATION_BLOCK_BYTES = 1024 * 1024
export const ISOLATION_FILE_BYTES = 49 * ISOLATION_BLOCK_BYTES
const RECEIVER_DISCONNECTS = 4
const CUT_WIRE_BYTES = 2 * ISOLATION_BLOCK_BYTES
const FEEDBACK_DELAY_MILLISECONDS = 100
const EVIDENCE_POLL_MILLISECONDS = 50

interface IsolationShare {
  readonly url: string
  readonly payload: Uint8Array
  readonly senderTrace: () => Promise<string>
  readonly timeoutMilliseconds: number
}

export async function runRelayIsolationScenario(
  browser: Browser, page: Page, testInfo: TestInfo, share: IsolationShare,
): Promise<void> {
  const healthyContext = await browser.newContext()
  const healthy = await healthyContext.newPage()
  const interruptedDownloads: Download[] = []
  const healthyDownloads: Download[] = []
  page.on('download', download => interruptedDownloads.push(download))
  healthy.on('download', download => healthyDownloads.push(download))
  const options = { timeout: share.timeoutMilliseconds, intervals: [EVIDENCE_POLL_MILLISECONDS] }
  let senderCut = '0'
  let healthyBefore: RelaySocketSnapshot | undefined
  let result: unknown
  try {
    for (const target of [page, healthy]) {
      await installRelayIsolationFaults(target)
      await target.goto(share.url)
      await expect(target.getByRole('button', { name: 'Download file', exact: true })).toBeEnabled()
      await target.evaluate(() => window.windshareDiagnostics.enable())
    }
    senderCut = senderTraceSequence(await share.senderTrace())
    healthyBefore = await healthy.evaluate(() => window.relayIsolationFaults.snapshot())
    await healthy.evaluate(delay => window.relayIsolationFaults.paceFeedback(delay), FEEDBACK_DELAY_MILLISECONDS)
    await page.evaluate(({ bytes, count }) => window.relayIsolationFaults.disrupt(bytes, count),
      { bytes: CUT_WIRE_BYTES, count: RECEIVER_DISCONNECTS })
    await healthy.getByRole('button', { name: 'Download file', exact: true }).click()
    await page.getByRole('button', { name: 'Download file', exact: true }).click()
    for (let cut = 1; cut <= RECEIVER_DISCONNECTS; cut += 1) {
      await expect.poll(() => page.evaluate(() => window.relayIsolationFaults.snapshot().cuts), options)
        .toBeGreaterThanOrEqual(cut)
      const sibling = await healthy.evaluate(() => window.relayIsolationFaults.snapshot())
      console.info(JSON.stringify({ event: 'relay-receiver-cut', cut, sibling }))
      expect(sibling.closed).toBe(healthyBefore.closed)
      expect(sibling.opened).toBe(healthyBefore.opened)
    }
    expect(healthyDownloads, 'The sibling must remain in flight across the interruptions').toHaveLength(0)
    expect(await healthy.evaluate(() => window.relayIsolationFaults.snapshot().receivedBytes))
      .toBeGreaterThan(CUT_WIRE_BYTES)
    await healthy.evaluate(() => window.relayIsolationFaults.paceFeedback(0))
    await expect.poll(() => interruptedDownloads.length, options).toBe(1)
    await expect.poll(() => healthyDownloads.length, options).toBe(1)
    const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex')
    const expected = digest(share.payload)
    for (const download of [interruptedDownloads[0]!, healthyDownloads[0]!]) {
      const path = await download.path()
      expect(path).not.toBeNull()
      const bytes = await readFile(path!)
      expect(bytes.byteLength).toBe(share.payload.byteLength)
      expect(digest(bytes)).toBe(expected)
    }
    const sender = senderRecoveryEvidence(await share.senderTrace(), senderCut)
    const sibling = await healthy.evaluate(() => window.relayIsolationFaults.snapshot())
    expect(sender.outcome).toBe('uninterrupted')
    expect(sibling.opened).toBe(healthyBefore.opened)
    expect(sibling.closed).toBe(healthyBefore.closed)
    const evidence = { fileBytes: share.payload.byteLength, sha256: expected, sender, sibling,
      interrupted: await page.evaluate(() => window.relayIsolationFaults.snapshot()) }
    result = evidence
    console.info(JSON.stringify({ event: 'relay-receiver-isolation', ...evidence }))
  } finally {
    const observed = {
      result, senderCut, healthyBefore,
      interrupted: await page.evaluate(() => window.relayIsolationFaults?.snapshot()).catch(() => undefined),
      sibling: await healthy.evaluate(() => window.relayIsolationFaults?.snapshot()).catch(() => undefined),
    }
    await writeFile(testInfo.outputPath('isolation.json'), JSON.stringify(observed, null, 2))
    await writeFile(testInfo.outputPath('sender.ndjson'), await share.senderTrace())
    for (const [name, target] of [['interrupted', page], ['healthy', healthy]] as const) {
      const trace = await target.evaluate(() => window.windshareDiagnostics?.export()).catch(() => undefined)
      if (trace) await writeFile(testInfo.outputPath(name + '.ndjson'), trace)
    }
    await healthyContext.close()
  }
}
