import { expect, test } from '@playwright/test'

function expectEventBefore(events, before, after) {
  const beforeIndex = events.indexOf(before)
  const afterIndex = events.indexOf(after)
  expect(beforeIndex, `missing event ${before}`).toBeGreaterThanOrEqual(0)
  expect(afterIndex, `missing event ${after}`).toBeGreaterThanOrEqual(0)
  expect(beforeIndex, `${before} must precede ${after}`).toBeLessThan(afterIndex)
}

test('Pion and Chromium honor the WindShare DataChannel contract', async ({ page }) => {
  const pageErrors = []
  page.on('pageerror', (error) => pageErrors.push(error.message))

  await page.goto('/')
  const config = await page.evaluate(async () => {
    const response = await fetch('/config')
    return response.json()
  })
  const observation = await page.evaluate(() => window.runWindShareSpike())

  expect(pageErrors).toEqual([])
  expect(config.maxFrameSize).toBe(64 * 1024)

  const { browser, server } = observation
  expect(browser.errors).toEqual([])
  expect(browser.label).toBe(config.channelLabel)
  expect(browser.protocol).toBe(config.channelProtocol)
  expect(browser.ordered).toBe(true)
  expect(browser.reliable).toBe(true)
  expect(browser.negotiated).toBe(false)
  expect(browser.sctpMaxMessageSize).toBeGreaterThanOrEqual(config.maxFrameSize)
  expect(browser.chromeCandidates).toBeGreaterThan(0)
  expect(browser.pionCandidates).toBeGreaterThan(0)

  expect(browser.browserBackpressurePeak).toBeGreaterThanOrEqual(config.highWaterMark)
  expect(browser.browserBufferedAmountLow).toBe(true)
  expect(browser.browserBurstMessages).toBeGreaterThan(0)
  expect(browser.serverBackpressurePeak).toBeGreaterThanOrEqual(config.highWaterMark)
  expect(browser.serverBufferedAmountLow).toBe(true)
  expect(browser.serverBurstMessages).toBeGreaterThan(0)
  expect(browser.serverProbeValid).toBe(true)

  expect(server.errors).toEqual([])
  expect(server.offerSignals).toBe(1)
  expect(server.answerSignals).toBe(1)
  expect(server.browserCandidateSignals).toBeGreaterThan(0)
  expect(server.pionCandidateSignals).toBeGreaterThan(0)
  expect(server.channelLabel).toBe(config.channelLabel)
  expect(server.channelProtocol).toBe(config.channelProtocol)
  expect(server.ordered).toBe(true)
  expect(server.reliable).toBe(true)
  expect(server.negotiated).toBe(false)
  expect(server.remoteSdpMaxMessageSize).toBeGreaterThanOrEqual(config.maxFrameSize)
  expect(server.browserProbeReceived).toBe(true)
  expect(server.pionProbeSent).toBe(true)
  expect(server.browserBurstMessages).toBe(browser.browserBurstMessages)
  expect(server.pionBurstMessages).toBe(browser.serverBurstMessages)
  expect(server.browserBackpressurePeak).toBe(browser.browserBackpressurePeak)
  expect(server.pionBackpressurePeak).toBe(browser.serverBackpressurePeak)
  expect(server.browserBufferedAmountLow).toBe(true)
  expect(server.pionBufferedAmountLow).toBe(true)
  expect(server.clientTerminalReceived).toBe(true)
  expect(server.serverTerminalSent).toBe(true)
  expect(server.channelClosed).toBe(true)

  expectEventBefore(browser.events, 'server-terminal-received', 'channel-closed')
  expectEventBefore(server.events, 'client-terminal-received', 'server-terminal-sent')
  expectEventBefore(server.events, 'server-terminal-sent', 'server-terminal-acknowledged')
  expectEventBefore(server.events, 'server-terminal-acknowledged', 'channel-close-requested')
  expectEventBefore(server.events, 'server-terminal-sent', 'channel-closed')

  console.log(
    'WINDSHARE_WEBRTC_OBSERVATION' +
      ` candidates=${browser.chromeCandidates}/${browser.pionCandidates}` +
      ` maxMessage=${browser.sctpMaxMessageSize}/${server.remoteSdpMaxMessageSize}` +
      ` peaks=${browser.browserBackpressurePeak}/${browser.serverBackpressurePeak}` +
      ` bursts=${browser.browserBurstMessages}/${browser.serverBurstMessages}` +
      ' terminal=verified',
  )
})
