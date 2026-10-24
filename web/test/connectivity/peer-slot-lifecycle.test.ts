import { expect, it } from 'vitest'
import { PeerSet } from '../../src/connectivity/peer-set/peer-set'
import { createPeerBindingForPath } from '../../src/connectivity/session-signaling'
import { encodePeerOffer, encodePeerAnswer, type PeerBinding } from '../../src/connectivity/signaling-codec'
import type { PeerAttemptResult } from '../../src/connectivity/peer-failure'
import {
  createPeerAttemptIdentity,
  createPeerPathIdentityValue,
  createProtocolSessionIdentity,
  type PeerPathIdentity,
} from '../../src/session/identities'
import { encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { OperationRouter, OPERATION_TOMBSTONE_MILLISECONDS } from '../../src/session/operation-router'
import type { SessionOperation } from '../../src/session/runtime'

it('replaces physical paths repeatedly while preserving live and retired signaling authority', async () => {
  let now = 0
  let terminal = false
  const router = new OperationRouter(() => { terminal = true }, () => now)
  const peers = new PeerSet()
  const protocolSessionId = createProtocolSessionIdentity(new Uint8Array(16).fill(1))
  type Opened = { path: PeerPathIdentity; binding: PeerBinding; operation: SessionOperation }
  function open(seed: number): Promise<Opened> {
    const path = createPeerPathIdentityValue(new Uint8Array(16).fill(seed))
    return new Promise((resolve, reject) => {
      const supervisor = peers.add({ protocolSessionId, peerPathId: path, attempts: {
        createAttempt(context) {
          try {
            const binding = createPeerBindingForPath(path.copyBytes(), context)
            const operation = router.create(MESSAGE_KIND.peerOffer, encodePeerOffer({ ...binding, sdp: 'v=0' }))
            let settle!: (result: PeerAttemptResult) => void
            const result = new Promise<PeerAttemptResult>((resolve) => { settle = resolve })
            resolve({ path, binding, operation })
            return {
              attemptId: createPeerAttemptIdentity(binding.attemptId), result,
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
      now += OPERATION_TOMBSTONE_MILLISECONDS + 1
      // Fresh operation IDs ensure the slot watermark, not a tombstone, owns rejection.
      await expect(router.route(encodeMessage(
        MESSAGE_KIND.peerAnswer,
        new Uint8Array(16).fill(seed + 100),
        encodePeerAnswer({ ...oldest.binding, sdp: 'v=0' }),
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
