import { describe, expect, it, vi } from 'vitest'
import {
  RelayJoinWindowError,
  RELAY_OUTBOUND_FRAME_CAPACITY,
  RelayProtocolViolation,
  RelayServerError,
  RelaySessionIngressError,
  RelaySocketIngressError,
  createSessionId,
  dialRelayReceiver,
  encodeForwardEnvelope,
  encodeManifestEnvelope,
  encodeSignaling,
  encodeTerminalForwardEnvelope,
  formatSessionId,
  relayWebSocketUrl,
  type RelaySocket,
  type RelaySocketFactory,
  type SessionId,
} from '../../src/transport/relay'
import { loadVectorFile } from '../vectors'
import { FakeRelaySocket, gate, readAll, settle } from './helpers'

interface RelayEndpointVector {
  readonly name: string
  readonly relayUrl: string
  readonly accepted: boolean
  readonly webSocketUrl?: string
}

interface RelayEndpointASCIIComponent {
  readonly skip: string
  readonly alphanumeric: boolean
  readonly literal: string
  readonly escaped?: Readonly<Record<string, string>>
}

interface RelayEndpointASCIIMatrix {
  readonly first: number
  readonly last: number
  readonly path: RelayEndpointASCIIComponent
  readonly query: RelayEndpointASCIIComponent
  readonly userinfo: RelayEndpointASCIIComponent
}

const endpointVectors = loadVectorFile(
  new URL('../../../testvectors/relay-endpoint.json', import.meta.url),
)
const endpointASCIIMatrix = (
  endpointVectors as unknown as { readonly asciiMatrix: RelayEndpointASCIIMatrix }
).asciiMatrix

function matrixCharacterEncoding(
  contract: RelayEndpointASCIIComponent,
  character: string,
): { readonly accepted: boolean; readonly encoded: string } {
  const escaped = contract.escaped?.[character]
  if (escaped !== undefined) {
    return { accepted: true, encoded: escaped }
  }
  const accepted = (
    contract.alphanumeric && /^[A-Za-z\d]$/u.test(character)
  ) || contract.literal.includes(character)
  return { accepted, encoded: character }
}

class SocketSequence implements RelaySocketFactory {
  readonly urls: string[] = []
  readonly sockets: Array<RelaySocket | Error>

  constructor(sockets: Array<RelaySocket | Error>) {
    this.sockets = sockets
  }

  async connect(url: string): Promise<RelaySocket> {
    this.urls.push(url)
    const next = this.sockets.shift()
    if (next === undefined) {
      throw new Error('no fake socket remains')
    }
    if (next instanceof Error) {
      throw next
    }
    return next
  }
}

function joinedSocket(sessionId: SessionId, manifest = Uint8Array.of(9, 8, 7)): FakeRelaySocket {
  const socket = new FakeRelaySocket()
  socket.sendTextHook = async (text) => {
    if (JSON.parse(text).type !== 'join') {
      return
    }
    socket.pushText(encodeSignaling({
      type: 'manifest',
      sessionId: formatSessionId(sessionId),
    }))
    socket.pushBinary(encodeManifestEnvelope(manifest))
  }
  return socket
}

const firstId = createSessionId(Uint8Array.of(0, 1, 2, 3, 4, 5, 6, 7))
const secondId = createSessionId(Uint8Array.of(8, 9, 10, 11, 12, 13, 14, 15))

describe('relay receiver join and lifecycle', () => {
  it('constructs /v1/ws/<shareId> and pairs manifest JSON with binary bytes', async () => {
    const socket = joinedSocket(firstId)
    const factory = new SocketSequence([socket])
    const connection = await dialRelayReceiver({
      relayUrl: 'https://relay.example/base/?token=secret',
      shareId: 'share_1',
      socketFactory: factory,
      keepaliveMs: 60_000,
    })
    expect(factory.urls).toEqual([
      'wss://relay.example/base/v1/ws/share_1?token=secret',
    ])
    expect(connection.sessionId).toEqual(firstId)
    expect(connection.sealedManifest).toEqual(Uint8Array.of(9, 8, 7))
    const copy = connection.sealedManifest
    copy.fill(0)
    expect(connection.sealedManifest).toEqual(Uint8Array.of(9, 8, 7))
    await connection.close()
  })

  it('retries not_found and rate limits only inside the bounded join window', async () => {
    const notFound = new FakeRelaySocket()
    notFound.sendTextHook = async () => {
      notFound.pushText(encodeSignaling({ type: 'not_found' }))
    }
    const limited = new FakeRelaySocket()
    limited.sendTextHook = async () => {
      limited.pushText(encodeSignaling({
        type: 'error',
        code: 'rate_limited',
        message: 'slow down',
      }))
    }
    const success = joinedSocket(firstId)
    const factory = new SocketSequence([notFound, limited, success])
    let now = 0
    const connection = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: factory,
      joinRetryWindowMs: 1_000,
      retryInitialMs: 10,
      retryMaxMs: 20,
      now: () => now,
      sleep: async (milliseconds) => {
        now += milliseconds
      },
      keepaliveMs: 60_000,
    })
    expect(factory.urls).toHaveLength(3)
    await connection.close()

    const failingFactory: RelaySocketFactory = {
      connect: async () => Promise.reject(new Error('offline')),
    }
    now = 0
    await expect(dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: failingFactory,
      joinRetryWindowMs: 25,
      retryInitialMs: 10,
      retryMaxMs: 10,
      now: () => now,
      sleep: async (milliseconds) => {
        now += milliseconds
      },
    })).rejects.toBeInstanceOf(RelayJoinWindowError)
  })

  it('rejects hostile manifest sequencing without retrying', async () => {
    const hostile = new FakeRelaySocket()
    hostile.sendTextHook = async () => {
      hostile.pushText(encodeSignaling({
        type: 'manifest',
        sessionId: formatSessionId(firstId),
      }))
      hostile.pushText(encodeSignaling({ type: 'keepalive' }))
    }
    const factory = new SocketSequence([hostile, new Error('must not retry')])
    await expect(dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: factory,
    })).rejects.toMatchObject({ kind: 'manifest-sequence' })
    expect(factory.urls).toHaveLength(1)
  })

  it('does not amplify a socket ingress violation with join retries', async () => {
    const hostile = new FakeRelaySocket()
    hostile.sendTextHook = async () => {
      hostile.fail(new RelaySocketIngressError('oversized relay message'))
    }
    const factory = new SocketSequence([hostile, new Error('must not retry')])
    await expect(dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: factory,
    })).rejects.toBeInstanceOf(RelaySocketIngressError)
    expect(factory.urls).toHaveLength(1)
  })

  it('rejoin creates a new physical connection and session ID', async () => {
    const factory = new SocketSequence([joinedSocket(firstId), joinedSocket(secondId)])
    const first = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: factory,
      keepaliveMs: 60_000,
    })
    const rejoined = await first.rejoin()
    expect(rejoined.sessionId).toEqual(secondId)
    expect(factory.urls).toHaveLength(2)
    await rejoined.close()
  })

  it('delivers an inbound terminal last and tombstones later traffic', async () => {
    const socket = joinedSocket(firstId)
    const connection = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: new SocketSequence([socket]),
      keepaliveMs: 60_000,
    })
    const frames = readAll(connection.channel.frames)
    socket.pushBinary(encodeForwardEnvelope(firstId, Uint8Array.of(1)))
    socket.pushBinary(encodeTerminalForwardEnvelope(firstId, Uint8Array.of(3)))
    socket.pushBinary(encodeForwardEnvelope(firstId, Uint8Array.of(2)))
    expect(await frames).toEqual([Uint8Array.of(1), Uint8Array.of(3)])
    await connection.done
    expect(connection.channel.state).toBe('closed')
  })

  it('rejects foreign-session traffic and bounds frame ingress', async () => {
    const foreignSocket = joinedSocket(firstId)
    const foreign = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: new SocketSequence([foreignSocket]),
      keepaliveMs: 60_000,
    })
    foreignSocket.pushBinary(encodeForwardEnvelope(secondId, Uint8Array.of(1)))
    await foreign.done
    expect(foreign.error).toBeInstanceOf(RelayProtocolViolation)

    const saturatedSocket = joinedSocket(firstId)
    const saturated = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: new SocketSequence([saturatedSocket]),
      keepaliveMs: 60_000,
    })
    for (let index = 0; index < 33; index += 1) {
      saturatedSocket.pushBinary(encodeForwardEnvelope(firstId, Uint8Array.of(index)))
    }
    await saturated.done
    expect(saturated.error).toBeInstanceOf(RelaySessionIngressError)
  })

  it('treats non-rate server errors as terminal', async () => {
    const socket = new FakeRelaySocket()
    socket.sendTextHook = async () => {
      socket.pushText(encodeSignaling({
        type: 'error',
        code: 'protocol_error',
        message: 'bad join',
      }))
    }
    const factory = new SocketSequence([socket, new Error('must not retry')])
    await expect(dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: factory,
    })).rejects.toBeInstanceOf(RelayServerError)
    expect(factory.urls).toHaveLength(1)
  })

  it('does not leak a retry timer when URL validation fails before dialing', async () => {
    vi.useFakeTimers()
    await expect(dialRelayReceiver({
      relayUrl: 'ftp://relay.example',
      shareId: 'share',
    })).rejects.toBeInstanceOf(TypeError)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('bounds keepalive work while the physical socket stays backpressured', async () => {
    vi.useFakeTimers()
    const blocked = gate()
    const socket = joinedSocket(firstId)
    const joinHook = socket.sendTextHook
    socket.sendTextHook = async (text, signal) => {
      if (JSON.parse(text).type === 'join') {
        await joinHook?.(text, signal)
        return
      }
      await blocked.promise
    }
    const connection = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: new SocketSequence([socket]),
      keepaliveMs: 1,
    })

    await vi.advanceTimersByTimeAsync(100)
    vi.clearAllTimers()
    blocked.open()
    await settle(256)
    const keepalives = socket.sentText
      .map((text) => JSON.parse(text) as { type: string })
      .filter(({ type }) => type === 'keepalive')
    expect(keepalives.length).toBeLessThanOrEqual(RELAY_OUTBOUND_FRAME_CAPACITY + 1)
    await connection.close()
  })

  it('settles the connection with the original keepalive transport failure', async () => {
    vi.useFakeTimers()
    const failure = new Error('keepalive transport failed')
    const socket = joinedSocket(firstId)
    const joinHook = socket.sendTextHook
    socket.sendTextHook = async (text, signal) => {
      if (JSON.parse(text).type === 'join') {
        await joinHook?.(text, signal)
        return
      }
      throw failure
    }
    const connection = await dialRelayReceiver({
      relayUrl: 'wss://relay.example',
      shareId: 'share',
      socketFactory: new SocketSequence([socket]),
      keepaliveMs: 1,
    })
    await vi.advanceTimersByTimeAsync(1)
    await connection.done
    expect(connection.error).toBe(failure)
    expect(connection.channel.reason).toBe(failure)
    expect(socket.closeCalls).toBeGreaterThan(0)
  })
})

describe('relay URL validation', () => {
  for (const vector of endpointVectors.cases as unknown as RelayEndpointVector[]) {
    it(`matches shared endpoint contract ${vector.name}`, () => {
      if (!vector.accepted) {
        expect(() => relayWebSocketUrl(vector.relayUrl, 'AAAAAAAAAAAA')).toThrow(TypeError)
        return
      }
      expect(relayWebSocketUrl(vector.relayUrl, 'AAAAAAAAAAAA')).toBe(vector.webSocketUrl)
    })
  }

  const matrixComponents = [
    {
      name: 'path',
      contract: endpointASCIIMatrix.path,
      buildUrls: (raw: string, encoded: string) => ({
        raw: `https://relay.example/matrix/x${raw}y/tail`,
        expected: `wss://relay.example/matrix/x${encoded}y/tail/v1/ws/AAAAAAAAAAAA`,
      }),
    },
    {
      name: 'query',
      contract: endpointASCIIMatrix.query,
      buildUrls: (raw: string, encoded: string) => ({
        raw: `https://relay.example/base?q=x${raw}y`,
        expected: `wss://relay.example/base/v1/ws/AAAAAAAAAAAA?q=x${encoded}y`,
      }),
    },
    {
      name: 'userinfo',
      contract: endpointASCIIMatrix.userinfo,
      buildUrls: (raw: string, encoded: string) => ({
        raw: `https://user:px${raw}y@relay.example/base`,
        expected: `wss://user:px${encoded}y@relay.example/base/v1/ws/AAAAAAAAAAAA`,
      }),
    },
  ] as const
  for (const component of matrixComponents) {
    it(`matches the shared printable-ASCII ${component.name} matrix`, () => {
      for (let value = endpointASCIIMatrix.first; value <= endpointASCIIMatrix.last; value += 1) {
        const character = String.fromCharCode(value)
        if (component.contract.skip.includes(character)) continue
        const { accepted, encoded } = matrixCharacterEncoding(component.contract, character)
        const urls = component.buildUrls(character, encoded)
        if (!accepted) {
          expect(() => relayWebSocketUrl(urls.raw, 'AAAAAAAAAAAA')).toThrow(TypeError)
          continue
        }
        expect(relayWebSocketUrl(urls.raw, 'AAAAAAAAAAAA')).toBe(urls.expected)
      }
    })
  }

  it('rejects non-WebSocket schemes and invalid share IDs', () => {
    expect(() => relayWebSocketUrl('ftp://relay.example', 'share')).toThrow(TypeError)
    expect(() => relayWebSocketUrl('https://relay.example', '../share')).toThrowError(
      /base64url/u,
    )
    expect(() => relayWebSocketUrl('https:\\relay.example', 'share')).toThrow(TypeError)
    expect(() => relayWebSocketUrl('https://relay.example/%zz', 'share')).toThrow(TypeError)
    expect(() => relayWebSocketUrl(' https://relay.example', 'share')).toThrow(TypeError)
    expect(() => relayWebSocketUrl('https://relay.example/\ud800', 'share')).toThrow(TypeError)
  })
})
