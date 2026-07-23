import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
const { expect, test } = require('../../../../../spikes/webrtc/node_modules/@playwright/test')

function expectBefore(events, before, after) {
  expect(events, `missing ${before}`).toContain(before)
  expect(events, `missing ${after}`).toContain(after)
  expect(events.indexOf(before)).toBeLessThan(events.indexOf(after))
}

function expectCommonInterop(config, browser, server) {
  expect(browser.errors).toEqual([])
  expect(browser.label).toBe(config.channelLabel)
  expect(browser.protocol).toBe(config.channelProtocol)
  expect(browser.ordered).toBe(true)
  expect(browser.reliable).toBe(true)
  expect(browser.negotiated).toBe(false)
  expect(browser.sctpMaxMessageSize).toBeGreaterThanOrEqual(config.maxFrameSize)
  expect(browser.clientBufferPeak).toBeGreaterThanOrEqual(config.highWaterBytes)
  expect(browser.lowObserved).toBe(true)
  expect(browser.clientBurstMessages).toBeGreaterThan(0)

  expect(server.errors).toEqual([])
  expect(server.channelLabel).toBe(config.channelLabel)
  expect(server.channelProtocol).toBe(config.channelProtocol)
  expect(server.ordered).toBe(true)
  expect(server.reliable).toBe(true)
  expect(server.negotiated).toBe(false)
  expect(server.sctpMaxMessageSize).toBeGreaterThanOrEqual(config.maxFrameSize)
  expect(server.clientProbeReceived).toBe(true)
  expect(server.clientBurstMessages).toBe(browser.clientBurstMessages)
  expect(server.serverProbeSent).toBe(true)
  expect(server.serverBufferPeak).toBeGreaterThanOrEqual(config.highWaterBytes)
}

function expectTerminalSettlement(browser, server) {
  expect(server.terminalAcknowledged).toBe(true)
  expect(server.channelDone).toBe(true)
  expect(server.channelStateClosed).toBe(true)
  expect(server.channelError).toBe('no error')
  expect(server.physicalCloseSettled).toBe(true)
  expectBefore(browser.events, 'terminal-intent', 'terminal-frame')
  expectBefore(browser.events, 'terminal-frame', 'terminal-ack-sent')
  expectBefore(browser.events, 'terminal-ack-sent', 'channel-closed')
  expectBefore(server.events, 'terminal-send-started', 'terminal-acknowledged')
  expectBefore(server.events, 'terminal-acknowledged', 'channel-done')
  expectBefore(server.events, 'channel-done', 'physical-close-settled')
}

test('production Pion Channel interoperates with the active browser engine', async ({ page }) => {
  const pageErrors = []
  page.on('pageerror', (error) => pageErrors.push(error.message))
  await page.goto('/')
  const config = await page.evaluate(async () => (await fetch('/config')).json())
  const { browser, server } = await page.evaluate(() => window.runD1Interop())

  expect(pageErrors).toEqual([])
  if (config.scenario !== 'malformed-setting') expectCommonInterop(config, browser, server)

  switch (config.scenario) {
    case 'happy':
      expect(browser.serverProbeReceived).toBe(true)
      expect(browser.serverBurstMessages).toBe(server.serverBurstMessages)
      expectTerminalSettlement(browser, server)
      expectBefore(browser.events, 'server-probe', 'terminal-intent')
      expectBefore(browser.events, 'server-burst-finished', 'terminal-intent')
      expectBefore(server.events, 'server-buffer-high', 'server-burst-recovered')
      break

    case 'cancellation':
      expect(browser.serverProbeReceived).toBe(true)
      expect(server.sendWaitObserved).toBe(true)
      expect(server.sendCanceled).toBe(true)
      expect(server.sendErrorCanceled).toBe(true)
      expect(server.sendError).toContain('context canceled')
      expect(browser.canceledSendReceived).toBe(false)
      expect(browser.cancellationBarrierReceived).toBe(true)
      expect(browser.serverBurstMessages).toBe(server.serverBurstMessages)
      expectTerminalSettlement(browser, server)
      expectBefore(browser.events, 'cancellation-barrier-received', 'terminal-intent')
      expectBefore(server.events, 'server-buffer-high', 'send-wait-observed')
      expectBefore(server.events, 'send-wait-observed', 'send-cancel-requested')
      expectBefore(server.events, 'send-cancel-requested', 'send-returned-context-canceled')
      expectBefore(server.events, 'send-returned-context-canceled', 'cancellation-barrier-sent')
      expectBefore(server.events, 'cancellation-barrier-sent', 'terminal-send-started')
      break

    case 'remote-close':
      expect(server.sendWaitObserved).toBe(true)
      expect(server.sendErrorRemoteClosed).toBe(true)
      expect(server.sendError).toContain('peer closed the DataChannel')
      expect(server.channelDone).toBe(true)
      expect(server.channelStateClosed).toBe(true)
      expect(server.channelErrorRemoteClosed).toBe(true)
      expect(server.channelError).toContain('peer closed the DataChannel')
      expect(server.physicalCloseSettled).toBe(true)
      expect(server.terminalAcknowledged).toBe(false)
      expect(browser.browserCloseInitiated).toBe(true)
      expect(browser.remoteCloseSendReceived).toBe(false)
      expectBefore(browser.events, 'channel-open', 'browser-close-initiated')
      expectBefore(browser.events, 'browser-close-initiated', 'channel-closed')
      expectBefore(server.events, 'server-buffer-high', 'send-wait-observed')
      expectBefore(server.events, 'send-wait-observed', 'browser-close-requested')
      expectBefore(server.events, 'browser-close-requested', 'send-returned-remote-close')
      expectBefore(server.events, 'send-returned-remote-close', 'channel-done')
      expectBefore(server.events, 'channel-done', 'physical-close-settled')
      break

    case 'malformed-setting':
      expect(browser.errors).toEqual([])
      expect(browser.label).toBe(config.channelLabel)
      expect(browser.protocol).toBe(config.invalidProtocol)
      expect(browser.protocol).not.toBe(config.channelProtocol)
      expect(browser.ordered).toBe(true)
      expect(browser.reliable).toBe(true)
      expect(browser.negotiated).toBe(false)
      expect(browser.channelState).toBe('closed')
      expect(browser.peerClosed).toBe(true)

      expect(server.errors).toEqual([])
      expect(server.channelLabel).toBe(config.channelLabel)
      expect(server.channelProtocol).toBe(config.invalidProtocol)
      expect(server.ordered).toBe(true)
      expect(server.reliable).toBe(true)
      expect(server.negotiated).toBe(false)
      expect(server.invalidChannelRejected).toBe(true)
      expect(server.invalidChannelErrorTyped).toBe(true)
      expect(server.invalidChannelError).toContain('DataChannel configuration is invalid')
      expect(server.invalidChannelError).toContain(`protocol "${config.invalidProtocol}"`)
      expect(server.invalidChannelError).toContain(`want "${config.channelProtocol}"`)
      expect(server.channelCreated).toBe(false)
      expect(server.channelOpened).toBe(false)
      expect(server.channelStateObserved).toBe(false)
      expect(server.channelDone).toBe(false)
      expect(server.channelStateClosed).toBe(false)
      expect(server.rawChannelState).toBe('closed')
      expect(server.rawChannelStateClosed).toBe(true)
      expect(server.physicalCloseSettled).toBe(true)
      expect(server.peerCloseSettled).toBe(true)
      expect(server.events).not.toContain('channel-open')
      expect(server.events).not.toContain('channel-done')
      expectBefore(server.events, 'adapter-construction-started', 'adapter-invalid-channel-rejected')
      expectBefore(server.events, 'adapter-invalid-channel-rejected', 'raw-close-requested')
      expectBefore(server.events, 'raw-close-requested', 'raw-channel-closed')
      expectBefore(server.events, 'raw-channel-closed', 'physical-close-settled')
      expectBefore(server.events, 'physical-close-settled', 'peer-close-requested')
      expectBefore(server.events, 'peer-close-requested', 'peer-close-settled')
      expectBefore(browser.events, 'channel-closed', 'browser-peer-closed')
      break

    default:
      throw new Error(`unknown interoperability scenario ${config.scenario}`)
  }

  if (config.scenario === 'malformed-setting') {
    console.log(
      `WINDSHARE_D1_INTEROP scenario=${config.scenario}` +
        ` protocol=${browser.protocol}/${server.channelProtocol}` +
        ` typed=${server.invalidChannelErrorTyped}` +
        ` rawState=${server.rawChannelState}` +
        ` peerSettled=${server.peerCloseSettled}`,
    )
  } else {
    console.log(
      `WINDSHARE_D1_INTEROP scenario=${config.scenario}` +
        ` maxMessage=${browser.sctpMaxMessageSize}/${server.sctpMaxMessageSize}` +
        ` peaks=${browser.clientBufferPeak}/${server.serverBufferPeak}` +
        ` bursts=${browser.clientBurstMessages}/${browser.serverBurstMessages}` +
        ` terminal=${server.terminalAcknowledged}` +
        ` sendError=${server.sendError || 'none'}`,
    )
  }
})
