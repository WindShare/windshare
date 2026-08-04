import { expect, test, type Page, type TestInfo } from '@playwright/test'
import type * as BrowserHarness from './browser-harness'
import { classifyNativePeerConnection } from './browser-capability'

const HARNESS_PATH = '/test/transport/webrtc/browser-harness.ts'

test('production browser adapters complete ICE, backpressure, cancellation, and terminal', async ({
  page,
}, testInfo) => {
  if (!await nativeAdapterApplicable(page, testInfo)) return
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof BrowserHarness
    return harness.runBrowserLoopback()
  }, HARNESS_PATH)

  expect(result).toEqual({
    connected: true,
    exactLeftToRight: true,
    exactRightToLeft: true,
    highWaterObserved: true,
    lowWaterObserved: true,
    cancellationWaitObserved: true,
    cancellationError: 'AbortError',
    canceledMarkerReceived: false,
    barrierReceived: true,
    terminalLast: true,
    terminalAcknowledged: true,
    leftState: 'closed',
    rightState: 'closed',
  })
})

test('remote browser close wakes a capacity-blocked production send', async ({ page }, testInfo) => {
  if (!await nativeAdapterApplicable(page, testInfo)) return
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof BrowserHarness
    return harness.runBrowserRemoteClose()
  }, HARNESS_PATH)

  expect(result).toEqual({
    highWaterObserved: true,
    capacityWaitObserved: true,
    sendError: 'WebRTCRemoteClosedError',
    leftReason: 'WebRTCRemoteClosedError',
    rightReason: 'WebRTCRemoteClosedError',
    lateMarkerReceived: false,
  })
})

test('native DataChannel closing settles both production adapters', async ({ page }, testInfo) => {
  if (!await nativeAdapterApplicable(page, testInfo)) return
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof BrowserHarness
    return harness.runBrowserDataChannelClose()
  }, HARNESS_PATH)

  expect(result).toEqual({
    leftReason: 'WebRTCRemoteClosedError',
    rightReason: 'WebRTCRemoteClosedError',
    leftState: 'closed',
    rightState: 'closed',
    leftRawState: 'closed',
    rightRawState: 'closed',
  })
})

test('actual browser channel settings are rejected before negotiation', async ({ page }, testInfo) => {
  if (!await nativeAdapterApplicable(page, testInfo)) return
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof BrowserHarness
    return harness.runBrowserInvalidConfiguration()
  }, HARNESS_PATH)

  expect(result.errorName).toBe('WebRTCDataChannelConfigurationError')
  expect(result.errorMessage).toContain('windshare-v2-invalid')
  expect(result.errorMessage).toContain(DATA_CHANNEL_PROTOCOL)
  expect(result.rawLabel).toBe(DATA_CHANNEL_LABEL)
  expect(result.rawProtocol).toBe('windshare-v2-invalid')
  expect(result.rawOrdered).toBe(true)
  expect(result.rawReliable).toBe(true)
  expect(result.rawNegotiated).toBe(false)
})

const DATA_CHANNEL_LABEL = 'windshare-frame-channel'
const DATA_CHANNEL_PROTOCOL = 'windshare-v2'

async function nativeAdapterApplicable(page: Page, testInfo: TestInfo): Promise<boolean> {
  const capability = await classifyNativePeerConnection(page)
  await testInfo.attach('rtc-capability', {
    body: JSON.stringify(capability),
    contentType: 'application/json',
  })
  if (capability.rtcCapability === 'unavailable') {
    expect(capability.pionApplicability).toBe('not-applicable')
    return false
  }
  expect(
    capability.rtcCapability,
    `native RTC API is present but the local offer probe classified it as ${capability.rtcCapability}`,
  ).toBe('available')
  return true
}
