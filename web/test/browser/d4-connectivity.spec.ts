import { expect, test } from '@playwright/test'
import type * as D4Harness from './d4-harness'

const HARNESS_PATH = '/test/browser/d4-harness.ts'

test('gesture-started production negotiation survives signaling loss through terminal', async ({
  page,
}) => {
  await page.goto('/')
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof D4Harness
    return harness.runD4BrowserLoopback()
  }, HARNESS_PATH)

  expect(result).toEqual({
    peerCreationsBeforeOffer: 0,
    peerCreationsAfterOffer: 1,
    localFrames: [0x22],
    remoteFrames: [0x11, 0x33, 0x44],
    survivedSignalingLoss: true,
    terminalAcknowledged: true,
    localState: 'closed',
    remoteState: 'closed',
  })
})
