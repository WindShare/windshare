import { describe, expect, it } from 'vitest'
import { PeerPathControl } from '../../src/connectivity/peer-set/path-control'
import { PeerNetworkGeneration } from '../../src/connectivity/peer-set/network-generation'
import { decodePeerPathControl, encodePeerPathControl, PEER_PATH_CONTROL_KIND as KIND } from '../../src/protocol/peer-path-control'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import type { ReceiverSessionRuntime } from '../../src/session/runtime'
import { deferred, openSent, runtimeFixture } from '../session/v2-send-fixture'

describe('authenticated cross-attempt path control', () => {
  it('joins demand renewal when the session writer remains stalled', async () => {
    const sent: Uint8Array[] = []
    const session = {
      subscribePeerPathControls: () => () => undefined,
      sendPeerPathControl: (body: Uint8Array) => {
        sent.push(body)
        return new Promise<void>(() => undefined)
      },
    } as unknown as ReceiverSessionRuntime
    const control = new PeerPathControl(session, new Uint8Array(16).fill(1), new PeerNetworkGeneration(), {
      now: () => 0,
      sleep: async () => { throw new Error('cancelled send must not begin a timer') },
    }, () => undefined)
    control.activate()
    await control.close()
    expect(sent.map((body) => decodePeerPathControl(body).kind)).toEqual([KIND.demand, KIND.revoke])
  })

  it('renews only content demand, retains replay watermark, and revokes without reviving an operation', async () => {
    let receive: ((body: Uint8Array<ArrayBuffer>) => void) | undefined
    const sent: Uint8Array[] = []
    const session = {
      subscribePeerPathControls(listener: typeof receive) { receive = listener; return () => { receive = undefined } },
      async sendPeerPathControl(body: Uint8Array) { sent.push(body) },
    } as unknown as ReceiverSessionRuntime
    const path = new Uint8Array(16).fill(1)
    const network = new PeerNetworkGeneration()
    const waits: (() => void)[] = []
    const clock = {
      now: () => 0,
      sleep: (_ms: number, signal: AbortSignal) => new Promise<void>((resolve, reject) => {
        waits.push(resolve)
        signal.addEventListener('abort', () => reject(signal.reason), { once: true })
      }),
    }
    let hints = 0
    const control = new PeerPathControl(session, path, network, clock, () => { hints += 1 })
    expect(sent).toHaveLength(0)
    control.activate()
    for (let index = 0; index < 6; index += 1) await Promise.resolve()
    expect(decodePeerPathControl(sent[0]!)).toMatchObject({
      controlSequence: 1n, kind: KIND.demand, validForMilliseconds: 120_000, holdForMilliseconds: 85_000,
    })
    const notice = (sequence: bigint, identity = path) => encodePeerPathControl({
      peerPathId: identity, networkGenerationId: network.copyBytes(), controlSequence: sequence,
      kind: KIND.mappingReady, validForMilliseconds: 10_000, holdForMilliseconds: 0,
    })
    receive?.(notice(2n))
    receive?.(notice(2n))
    receive?.(notice(1n))
    receive?.(notice(100n, new Uint8Array(16).fill(9)))
    expect(hints).toBe(1)
    waits[0]?.()
    for (let index = 0; index < 6; index += 1) await Promise.resolve()
    expect(decodePeerPathControl(sent[1]!).controlSequence).toBe(2n)
    control.deactivate()
    expect(decodePeerPathControl(sent.at(-1)!)).toMatchObject({
      peerPathId: path, kind: KIND.demand, validForMilliseconds: 120_000, holdForMilliseconds: 0,
    })
    receive?.(notice(3n))
    expect(hints).toBe(1)
    control.activate()
    expect(decodePeerPathControl(sent.at(-1)!)).toMatchObject({
      peerPathId: path, kind: KIND.demand, holdForMilliseconds: 85_000,
    })
    receive?.(notice(3n))
    expect(hints).toBe(1)
    receive?.(notice(4n))
    expect(hints).toBe(2)
    control.deactivate()
    await control.close()
    expect(decodePeerPathControl(sent.at(-1)!)).toMatchObject({ kind: KIND.revoke, validForMilliseconds: 0 })
    expect(sent.map((body) => decodePeerPathControl(body).kind).filter((kind) => kind === KIND.revoke)).toHaveLength(1)
    expect(receive).toBeUndefined()
  })

  it('delivers queued revoke without blocking close when the session writer is sealing', async () => {
    const completed: ProtocolTraceEvent[] = []
    const twoSent = deferred<void>()
    const { runtime, channel } = runtimeFixture({
      current: (event) => {
        if (event.eventName === 'protocol_operation' && event.transition === 'send_completed') {
          completed.push(event)
          if (completed.length === 2) twoSent.resolve()
        }
      },
    })
    channel.unblock()
    const path = new Uint8Array(16).fill(2)
    const control = new PeerPathControl(runtime, path, new PeerNetworkGeneration(), {
      now: () => 0,
      sleep: async () => { throw new Error('closed control must not schedule renewal') },
    }, () => undefined)
    try {
      control.activate()
      await control.close()
      await twoSent.promise
      const sent = (await openSent(channel)).map(({ message }) => decodePeerPathControl(message.body))
      expect(sent).toMatchObject([
        { peerPathId: path, controlSequence: 1n, kind: KIND.demand, holdForMilliseconds: 85_000 },
        { peerPathId: path, controlSequence: 2n, kind: KIND.revoke, validForMilliseconds: 0, holdForMilliseconds: 0 },
      ])
    } finally {
      await runtime.close()
    }
  })

  it('withdraws queued demand on close while retaining queued revoke behind a backpressured writer', async () => {
    const transitions: string[] = []
    const twoSent = deferred<void>()
    let sentCount = 0
    const { runtime, channel } = runtimeFixture({
      current: (event) => {
        if (event.eventName === 'protocol_operation') {
          transitions.push(event.transition)
          if (event.transition === 'send_completed' && ++sentCount === 2) twoSent.resolve()
        }
      },
    })
    const path = new Uint8Array(16).fill(3)
    const control = new PeerPathControl(runtime, path, new PeerNetworkGeneration(), {
      now: () => 0,
      sleep: (_ms, signal) => new Promise<void>((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true })
      }),
    }, () => undefined)
    try {
      control.activate()
      await channel.sending.promise
      control.deactivate()
      await control.close()
      expect(transitions).toContain('send_withdrawn')
      expect(channel.sent).toHaveLength(0)
      channel.unblock()
      await twoSent.promise
      const sent = (await openSent(channel)).map(({ message }) => decodePeerPathControl(message.body))
      expect(sent).toMatchObject([
        { peerPathId: path, controlSequence: 1n, kind: KIND.demand, holdForMilliseconds: 85_000 },
        { peerPathId: path, controlSequence: 3n, kind: KIND.revoke, validForMilliseconds: 0, holdForMilliseconds: 0 },
      ])
    } finally {
      await runtime.close()
    }
  })
})
