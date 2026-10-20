import { expect, it } from 'vitest'
import { V2PeerSet } from '../../src/connectivity/peer-set/peer-set'
import { createV2PeerBindingForPath } from '../../src/connectivity/v2-session-signaling'
import { encodeV2PeerOffer, encodeV2PeerAnswer, type V2PeerBinding } from '../../src/connectivity/v2-signaling-codec'
import type { V2PeerAttemptResult } from '../../src/connectivity/v2-peer-failure'
import {
  createV2PeerAttemptIdentity,
  createV2PeerPathIdentityValue,
  createV2ProtocolSessionIdentity,
  type V2PeerPathIdentity,
} from '../../src/session/v2-identities'
import { encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2OperationRouter, V2_OPERATION_TOMBSTONE_MILLISECONDS } from '../../src/session/v2-operation-router'
import type { V2SessionOperation } from '../../src/session/v2-runtime'

it('replaces physical paths repeatedly while preserving live and retired signaling authority', async () => {
  let now = 0
  let terminal = false
  const router = new V2OperationRouter(() => { terminal = true }, () => now)
  const peers = new V2PeerSet()
  const protocolSessionId = createV2ProtocolSessionIdentity(new Uint8Array(16).fill(1))
  type Opened = { path: V2PeerPathIdentity; binding: V2PeerBinding; operation: V2SessionOperation }
  function open(seed: number): Promise<Opened> {
    const path = createV2PeerPathIdentityValue(new Uint8Array(16).fill(seed))
    return new Promise((resolve, reject) => {
      const supervisor = peers.add({ protocolSessionId, peerPathId: path, attempts: {
        createAttempt(context) {
          try {
            const binding = createV2PeerBindingForPath(path.copyBytes(), context)
            const operation = router.create(V2_MESSAGE_KIND.peerOffer, encodeV2PeerOffer({ ...binding, sdp: 'v=0' }))
            let settle!: (result: V2PeerAttemptResult) => void
            const result = new Promise<V2PeerAttemptResult>((resolve) => { settle = resolve })
            resolve({ path, binding, operation })
            return {
              attemptId: createV2PeerAttemptIdentity(binding.attemptId), result,
              cancel(owner) {
                operation.close()
                settle({ type: 'lifecycle-cancelled', owner })
              },
            }
          } catch (error) {
            reject(error)
            throw error
          }
        },
      } })
      supervisor.activate()
    })
  }

  try {
    const direct = await open(2)
    let current = await open(3)
    const oldest = current
    for (let seed = 4; seed < 28; seed++) {
      const replacement = await open(seed)
      expect(new Set([direct.binding.peerSlot, current.binding.peerSlot, replacement.binding.peerSlot]).size).toBe(3)
      await peers.remove(current.path)
      now += V2_OPERATION_TOMBSTONE_MILLISECONDS + 1
      // Fresh operation IDs ensure the slot watermark, not a tombstone, owns rejection.
      await expect(router.route(encodeV2Message(
        V2_MESSAGE_KIND.peerAnswer,
        new Uint8Array(16).fill(seed + 100),
        encodeV2PeerAnswer({ ...oldest.binding, sdp: 'v=0' }),
      ))).resolves.toBeUndefined()
      expect(router.active()).toEqual([direct.operation, replacement.operation])
      current = replacement
    }
    expect(current.binding.attemptSequence).toBeGreaterThan(1n)
    expect(terminal).toBe(false)
  } finally {
    await peers.close()
  }
  expect(router.active()).toEqual([])
})
