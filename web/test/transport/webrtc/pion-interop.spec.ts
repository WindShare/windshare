import { expect, test } from '@playwright/test'

import type * as BrowserHarness from './browser-harness'
import { classifyNativePeerConnection } from './browser-capability'

const HARNESS_PATH = '/test/transport/webrtc/browser-harness.ts'
const MAX_FRAME_BYTES = 65_536
const HIGH_WATER_BYTES = 1024 * 1024

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
