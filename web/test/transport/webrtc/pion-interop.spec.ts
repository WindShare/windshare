import { expect, test, type Page, type TestInfo } from '@playwright/test'

import type * as BrowserHarness from './browser-harness'
import { classifyNativePeerConnection } from './browser-capability'

const HARNESS_PATH = '/test/transport/webrtc/browser-harness.ts'
const MAX_FRAME_BYTES = 65_536
const HIGH_WATER_BYTES = 1024 * 1024
const DATA_CHANNEL_LABEL = 'windshare-frame-channel'
const DATA_CHANNEL_PROTOCOL = 'windshare-v2'

test('production browser adapter interoperates with the accepted Pion adapter', async ({ page }, testInfo) => {
  const capability = await classifyNativePeerConnection(page)
  await testInfo.attach('rtc-capability', {
    body: JSON.stringify(capability),
    contentType: 'application/json',
  })
  if (capability.rtcCapability === 'unavailable') {
    expect(capability.pionApplicability).toBe('not-applicable')
    return
  }
  expect(
    capability.rtcCapability,
    `native RTC API is present but its local offer probe was ${capability.rtcCapability}`,
  ).toBe('available')

  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof BrowserHarness
    return harness.runPionInterop()
  }, HARNESS_PATH)
  await testInfo.attach('pion-interop-diagnostic', {
    body: JSON.stringify(result),
    contentType: 'application/json',
  })

  expect(result.browser).toMatchObject({
    label: 'windshare-frame-channel',
    protocol: 'windshare-v2',
    ordered: true,
    reliable: true,
    negotiated: false,
    highWaterObserved: true,
    lowWaterObserved: true,
    cancellationWaitObserved: true,
    cancellationError: 'AbortError',
    canceledMarkerReceived: false,
    exactServerProbe: true,
    serverFinished: true,
    terminalLast: true,
    channelState: 'closed',
    channelReason: 'none',
  })
  expect(result.browser.maximumMessageSize).toBeGreaterThanOrEqual(MAX_FRAME_BYTES)
  expect(result.browser.clientBurstMessages).toBeGreaterThan(0)
  expect(result.browser.serverBurstMessages).toBeGreaterThan(0)

  expect(result.server).toMatchObject({
    errors: [],
    channelLabel: 'windshare-frame-channel',
    channelProtocol: 'windshare-v2',
    ordered: true,
    reliable: true,
    negotiated: false,
    clientProbeReceived: true,
    serverProbeSent: true,
    terminalAcknowledged: true,
    channelDone: true,
    channelStateClosed: true,
    channelError: 'no error',
    physicalCloseSettled: true,
  })
  expect(result.server['sctpMaxMessageSize']).toBeGreaterThanOrEqual(MAX_FRAME_BYTES)
  expect(result.server['serverBufferPeak']).toBeGreaterThanOrEqual(HIGH_WATER_BYTES)
  expect(result.server['clientBurstMessages']).toBe(result.browser.clientBurstMessages)
  expect(result.server['serverBurstMessages']).toBe(result.browser.serverBurstMessages)
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

test('native channel configuration is rejected before negotiation', async ({ page }, testInfo) => {
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
