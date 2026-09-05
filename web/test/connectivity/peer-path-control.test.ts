import { describe, expect, it } from 'vitest'
import { PeerPathControl } from '../../src/connectivity/peer-set/path-control'
import { PeerNetworkGeneration } from '../../src/connectivity/peer-set/network-generation'
import { decodeV2PeerPathControl, encodeV2PeerPathControl, V2_PEER_PATH_CONTROL_KIND as KIND } from '../../src/connectivity/v2-path-control-codec'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'

describe('authenticated cross-attempt path control', () => {
  it('joins demand renewal when the session writer remains stalled', async () => {
    const sent: Uint8Array[] = []
    const session = {
      subscribePeerPathControls: () => () => undefined,
      sendPeerPathControl: (body: Uint8Array) => {
        sent.push(body)
        return new Promise<void>(() => undefined)
      },
    } as unknown as V2ReceiverSessionRuntime
    const control = new PeerPathControl(session, new Uint8Array(16).fill(1), new PeerNetworkGeneration(), {
      now: () => 0,
      sleep: async () => { throw new Error('cancelled send must not begin a timer') },
    }, () => undefined, 'chromium-151.0.7922.34-windows')
    control.activate()
    await control.close()
    expect(sent.map((body) => decodeV2PeerPathControl(body).kind)).toEqual([KIND.demand, KIND.revoke])
    expect(decodeV2PeerPathControl(sent[0]!).providerProfile).toBe('chromium-151.0.7922.34-windows')
  })

  it('renews only content demand, retains replay watermark, and revokes without reviving an operation', async () => {
    let receive: ((body: Uint8Array<ArrayBuffer>) => void) | undefined
    const sent: Uint8Array[] = []
    const session = {
      subscribePeerPathControls(listener: typeof receive) { receive = listener; return () => { receive = undefined } },
      async sendPeerPathControl(body: Uint8Array) { sent.push(body) },
    } as unknown as V2ReceiverSessionRuntime
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
    expect(decodeV2PeerPathControl(sent[0]!)).toMatchObject({
      controlSequence: 1n, kind: KIND.demand, validForMilliseconds: 120_000, holdForMilliseconds: 85_000,
    })
    const notice = (sequence: bigint, identity = path) => encodeV2PeerPathControl({
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
    expect(decodeV2PeerPathControl(sent[1]!).controlSequence).toBe(2n)
    control.revoke()
    expect(decodeV2PeerPathControl(sent.at(-1)!)).toMatchObject({ kind: KIND.revoke, validForMilliseconds: 0 })
    receive?.(notice(3n))
    expect(hints).toBe(1)
    control.activate()
    receive?.(notice(3n))
    expect(hints).toBe(1)
    receive?.(notice(4n))
    expect(hints).toBe(2)
    await control.close()
    expect(receive).toBeUndefined()
  })
})
