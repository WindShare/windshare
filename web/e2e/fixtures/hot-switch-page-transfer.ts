import type { Page } from '@playwright/test'

import { MAIN_TRANSFER_BYTES } from '../../scripts/browser-evidence/result'

const FAILURE_DIAGNOSTIC_MAXIMUM_DEPTH = 4
const PAGE_TRANSFER_RUNTIME_PATH = '/e2e/fixtures/hot-switch-page-runtime.ts'

export async function startPageTransfer(
  page: Page,
  input: {
    readonly expectedHash: string
    readonly key: string
    readonly rtcConfiguration: RTCConfiguration
  },
): Promise<void> {
  await page.evaluate(async (runtimeInput) => {
    const runtime = await import(runtimeInput.runtimePath) as typeof import(
      './hot-switch-page-runtime'
    )
    runtime.startHotSwitchPageTransfer(runtimeInput)
  }, {
    ...input,
    failureDiagnosticMaximumDepth: FAILURE_DIAGNOSTIC_MAXIMUM_DEPTH,
    runtimePath: PAGE_TRANSFER_RUNTIME_PATH,
    transferBytes: MAIN_TRANSFER_BYTES,
  })
}

export async function sealPageRelayCut(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const seal = (
      window as Window & { __windshareSealHotSwitchRelayCut?: () => Promise<void> }
    ).__windshareSealHotSwitchRelayCut
    if (seal === undefined) throw new Error('Hot-switch relay-cut seal is unavailable')
    await seal()
  })
}

export async function releasePageOutput(page: Page): Promise<void> {
  if (page.isClosed()) return
  await page.evaluate(() => {
    const release = (
      window as Window & { __windshareReleaseHotSwitchOutput?: () => void }
    ).__windshareReleaseHotSwitchOutput
    release?.()
  })
}
