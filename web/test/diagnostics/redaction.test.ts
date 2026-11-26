import { describe, expect, it } from 'vitest'

import type { BrowserReceiverGateway } from '../../src/application/browser-share/gateway'
import { createFailureReporter, projectProtocolTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { parseSuite02CapabilityLink } from '../../src/crypto/suite02-link'
import { createDiagnosticsRuntime } from '../../src/diagnostics'
import { safeRelayIdentity } from '../../src/network/contracts/relay-endpoint'
import { RelayEndpointFailure } from '../../src/receiver/relay-selection/race'
import { sanitizeDiagnosticText, stripRawUrlCredentials } from '../../src/security/url-sanitizer'
import { ReceiverController } from '../../src/ui/controller'

describe('diagnostic redaction boundaries', () => {
  it('sanitizes URL credentials, queries, and fragments without stripping stack locations or normal hosts', () => {
    expect(sanitizeDiagnosticText('socket reset')).toBe('socket reset')
    expect(sanitizeDiagnosticText('email user@example.com failed')).toBe('email user@example.com failed')
    expect(sanitizeDiagnosticText(
      'at fn (http://localhost:5173/src/ui/controller.ts?t=1732531200000:583:12)',
    )).toBe('at fn (http://localhost:5173/src/ui/controller.ts:583:12)')
    expect(sanitizeDiagnosticText(
      'at fn (http://localhost:5173/src/(auth)/controller.ts?t=1732531200000:583:12)',
    )).toBe('at fn (http://localhost:5173/src/(auth)/controller.ts:583:12)')
    expect(sanitizeDiagnosticText(
      'at fn (https://windshare.top/assets/index-abc.js:1:2345)',
    )).toBe('at fn (https://windshare.top/assets/index-abc.js:1:2345)')
    expect(sanitizeDiagnosticText(
      'https://windshare.top/s/abc#secret:12:34',
    )).toBe('https://windshare.top/s/abc')
    expect(sanitizeDiagnosticText(
      '[https://user:pass@[2001:db8::1]:8443/path/@team/file?token=(s3cr3t)#frag]',
    )).toBe('[https://[2001:db8::1]:8443/path/@team/file]')
  })

  it('sanitizes RFC 3986 parentheses and apostrophes inside free-text URLs without swallowing surrounding prose', () => {
    expect(sanitizeDiagnosticText(
      "WebSocket connection to 'wss://relay.example/v2/ws?token=(SECRET)' failed",
    )).toBe("WebSocket connection to 'wss://relay.example/v2/ws' failed")
    expect(sanitizeDiagnosticText(
      "WebSocket connection to 'wss://relay.example/v2/ws?token=a)SECRET' failed",
    )).toBe("WebSocket connection to 'wss://relay.example/v2/ws' failed")
    expect(sanitizeDiagnosticText(
      'failed (see https://relay.example/v2/ws?token=(SECRET))',
    )).toBe('failed (see https://relay.example/v2/ws)')
    expect(sanitizeDiagnosticText(
      'https://host/item(1)#READ_SECRET',
    )).toBe('https://host/item(1)')
    expect(sanitizeDiagnosticText(
      'https://user:p(1)@relay.example/v2/ws?token=SECRET',
    )).toBe('https://relay.example/v2/ws')
    expect(sanitizeDiagnosticText(
      "https://user:o'hara@relay.example/v2/ws?token=SECRET",
    )).toBe('https://relay.example/v2/ws')
    expect(sanitizeDiagnosticText(
      `"https://host/don't#READ_SECRET" rejected`,
    )).toBe(`"https://host/don't" rejected`)
  })

  it('strips credentials directly from raw, whitespace-bearing, and scheme-less endpoint strings', () => {
    expect(stripRawUrlCredentials('https://user:my secret@relay.example/v2/ws?token=(SECRET)#frag'))
      .toBe('https://relay.example/v2/ws')
    expect(stripRawUrlCredentials('https://user:my secret@relay.example'))
      .toBe('https://relay.example')
    expect(stripRawUrlCredentials('user:secret@relay.example/v2/ws?token=1'))
      .toBe('relay.example/v2/ws')
    expect(stripRawUrlCredentials('user:secret@host/redirect/https://other?k=v'))
      .toBe('host/redirect/https://other')
    expect(stripRawUrlCredentials('//user:secret@relay.example/v2/ws#frag'))
      .toBe('//relay.example/v2/ws')
    expect(stripRawUrlCredentials('https://relay.example/path/@team/file?token=1'))
      .toBe('https://relay.example/path/@team/file')
    expect(stripRawUrlCredentials('invalid')).toBe('invalid')

    expect(safeRelayIdentity('https://user:my secret@relay.example/v2/ws'))
      .toBe('https://relay.example/v2/ws')
    expect(safeRelayIdentity('user:secret@relay.example/v2/ws'))
      .toBe('relay.example/v2/ws')
    expect(safeRelayIdentity('https://relay.example/base?token=(SECRET)'))
      .toBe('wss://relay.example/base/v2/ws')
  })

  it('omits raw native URL parser causes on malformed Suite-02 capability links', async () => {
    const secret = 'AgReadSecretMaterial0123456789abcdefghijklm'
    let caught: unknown
    try {
      await parseSuite02CapabilityLink(`https://windshare.top:badport/shareId#${secret}`)
    } catch (error) {
      caught = error
    }
    expect(caught).toBeInstanceOf(Error)
    expect((caught as Error).cause).toBeUndefined()
    expect(JSON.stringify(caught)).not.toContain(secret)
  })

  it('projects protocol relay traces with query-free relay_identity and redacted failure_detail', () => {
    const token = 'private-admission-token-99'
    const rawRelay = `https://relay.example/base?token=${token}`
    const failure = new RelayEndpointFailure(rawRelay, new Error(`dial ${rawRelay} rejected`))

    const recovery = projectProtocolTraceEvent({
      eventName: 'connection_recovery',
      correlation: {},
      generationId: 1,
      attempt: 1,
      phase: 'initial',
      transition: 'attempt_failed',
      relayIdentity: rawRelay,
      failure,
    })
    const heartbeat = projectProtocolTraceEvent({
      eventName: 'relay_heartbeat',
      correlation: {},
      connectionId: 1n,
      relayIdentity: rawRelay,
      round: 2n,
      stage: 'probe',
      bufferedBytes: 0,
      elapsedMilliseconds: 10,
      timeoutMilliseconds: 5_000,
    })
    const delivery = projectProtocolTraceEvent({
      eventName: 'relay_delivery',
      correlation: {},
      connectionId: 'conn-1',
      relayIdentity: rawRelay,
      sessionId: undefined,
      action: 'open',
      failure: undefined,
      progress: undefined,
      capacity: { frames: 8n, encodedBytes: 1024n },
      usage: {
        held: { frames: 0n, encodedBytes: 0n },
        outstanding: { frames: 0n, encodedBytes: 0n },
        pendingConsumed: { frames: 0n, encodedBytes: 0n },
        retainedSessions: 1,
      },
    })

    for (const entry of [recovery, heartbeat, delivery]) {
      const encoded = JSON.stringify(entry)
      expect(encoded).not.toContain(token)
      expect(encoded).not.toContain('relay_base')
      expect(entry.payload?.relay_identity).toBe('wss://relay.example/base/v2/ws')
    }
    expect(recovery.payload?.failure_detail).toContain('wss://relay.example/base/v2/ws')
  })

  it('redacts join secrets across failure ring, consoleSink, NDJSON export, and UI while preserving structure and public relays', async () => {
    const readSecret = 'AgSecretReadCapabilityFragment0123456789abcd'
    const relayToken = 'private-relay-token-777'
    const separateKey = 'AgSeparateKeyOnlySecret0123456789abcdefghijk'
    const consoleErrors: unknown[] = []
    const runtime = createDiagnosticsRuntime({
      build: { version: '1.0.0', mode: 'test' },
      consoleSink: { error: record => consoleErrors.push(record) },
    })

    let joinError: unknown
    const gateway = {
      join: async () => { throw joinError },
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway, {
      recordFailure: createFailureReporter(runtime),
    })

    try {
      // 1. Private relay query token failure keeps public relay identity visible and preserves AggregateError structure.
      const encodedRelay = encodeURIComponent(`https://relay.example/base?token=${relayToken}`)
      joinError = new AggregateError([
        new RelayEndpointFailure(`https://relay.example/base?token=${relayToken}`, new Error('connection refused')),
        new RelayEndpointFailure('https://jp1.windshare.top', new Error('timeout')),
      ], 'No relay endpoint admitted this share')
      controller.initialize({
        pageUrl: 'https://windshare.top/',
        capabilityInput: `https://windshare.top/share0123456789a?r=${encodedRelay}&r=https%3A%2F%2Fjp1.windshare.top#${readSecret}`,
      })
      await new Promise(resolve => setTimeout(resolve, 0))

      const uiRelayError = controller.getSnapshot().error
      expect(JSON.stringify(uiRelayError)).not.toContain(relayToken)
      expect(JSON.stringify(uiRelayError)).not.toContain(readSecret)
      const relayFailure = runtime.lastFailure()
      expect(relayFailure?.error).toMatchObject({
        name: 'AggregateError',
        errors: [
          {
            name: 'RelayEndpointFailure',
            message: 'Relay endpoint wss://relay.example/base/v2/ws failed',
            cause: { message: 'connection refused' },
          },
          {
            name: 'RelayEndpointFailure',
            message: 'Relay endpoint wss://jp1.windshare.top/v2/ws failed',
            cause: { message: 'timeout' },
          },
        ],
      })

      // 2. Single private relay failure shows clean identity (no token) in UI error detail.
      joinError = new RelayEndpointFailure(
        `https://relay.example/base?token=${relayToken}`,
        new Error('dial failed'),
      )
      controller.openLocation({
        pageUrl: 'https://windshare.top/',
        capabilityInput: `https://windshare.top/share0123456789b?r=${encodedRelay}#${readSecret}`,
      })
      await new Promise(resolve => setTimeout(resolve, 0))
      expect(controller.getSnapshot().error).toEqual({
        kind: 'open',
        detail: 'Relay endpoint wss://relay.example/base/v2/ws failed',
      })

      // 3. Malformed URL with embedded whitespace still redacts its #fragment via the join lease.
      const malformedWithSpace = `https://bad host/share#${readSecret}`
      joinError = new Error('Suite-02 capability URL is invalid', {
        cause: new TypeError(`URL constructor: '${malformedWithSpace}' is not a valid URL.`),
      })
      controller.openLocation({ pageUrl: 'https://windshare.top/', capabilityInput: malformedWithSpace })
      await new Promise(resolve => setTimeout(resolve, 0))

      // 4. Bare separateKey leak in error cause is structurally redacted via the join lease.
      joinError = new Error('join rejected', {
        cause: new Error(`rejected bare capability ${separateKey}`),
      })
      controller.submitKey(separateKey)
      await new Promise(resolve => setTimeout(resolve, 0))

      const bundle = runtime.export()
      const allConsole = JSON.stringify(consoleErrors)
      const last = JSON.stringify(runtime.lastFailure())
      for (const secret of [readSecret, relayToken, separateKey]) {
        expect(bundle).not.toContain(secret)
        expect(allConsole).not.toContain(secret)
        expect(last).not.toContain(secret)
      }
      expect(bundle).toContain('wss://jp1.windshare.top/v2/ws')
      expect(bundle).toContain('wss://relay.example/base/v2/ws')
      expect(last).toContain('[separate-key redacted]')
    } finally {
      await controller.dispose()
      runtime.dispose()
    }
  })
})
