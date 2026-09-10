import { afterEach, describe, expect, it, vi } from 'vitest'
import { V2RevisionService } from '../../src/content/v2-session-services'
import { V2LaneSet, type V2BlockRouteEligibility } from '../../src/content/v2-broker'
import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND, type V2MessageKind } from '../../src/session/v2-message'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import { createV2ProtocolOperationIdentity, createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import { transferDirectZipFileV1, type DirectZipOrderedFileV1, type DirectZipOutputSessionV1 } from '../../src/transfer/direct-zip'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { deferred, id, SHARE } from '../session/v2-send-fixture'

interface IdentityVector extends VectorCase {
  readonly readSecretB64: string
  readonly senderPublicKeyB64: string
  readonly shareInstanceB64: string
  readonly fileIdB64: string
}
interface ObjectVector extends VectorCase { readonly domain: string; readonly objectB64: string }
const identity = loadVectorFile(new URL('../../../core/testvectors/v2-identity.json', import.meta.url))
  .cases[0] as IdentityVector
const revision = loadVectorFile(new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url))
  .cases.find(value => (value as ObjectVector).domain === 'windshare/v2 object/file-revision') as ObjectVector
const bytes = (value: string) => new Uint8Array(b64ToBytes(value))
const ROUTES: V2BlockRouteEligibility = {
  active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
}
const RELEASE_WAIT = 30_000

function fixture() {
  const barrier = deferred<void>()
  const leaseId = id(24)
  const fileId = bytes(identity.fileIdB64)
  const share = {
    ...SHARE, shareInstance: bytes(identity.shareInstanceB64),
    senderPublicKey: bytes(identity.senderPublicKeyB64), chunkSize: 1 << 20,
  }
  const requests: V2MessageKind[] = []
  let nextId = 30
  const beginOperation = vi.fn(async (kind: V2MessageKind) => {
    requests.push(kind)
    const operationId = id(nextId++)
    let body: Uint8Array
    let response: V2MessageKind
    if (kind === V2_MESSAGE_KIND.openRevisions) {
      response = V2_MESSAGE_KIND.openResults
      body = encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, [[fileId, 0, bytes(revision.objectB64), leaseId, 120_000, 60_000]]],
      ]))
    } else if (kind === V2_MESSAGE_KIND.renewLease) {
      response = V2_MESSAGE_KIND.leaseResult
      body = encodeV2Body(new Map<number, unknown>([[0, 1], [1, leaseId], [2, 120_000], [3, 60_000]]))
    } else {
      expect(kind).toBe(V2_MESSAGE_KIND.releaseLease)
      response = V2_MESSAGE_KIND.operationComplete
      body = encodeV2Body([0])
    }
    return { id: operationId, requestKind: kind, cancel: () => undefined,
      next: async () => encodeV2Message(response, operationId, body) }
  })
  const session = {
    beginOperation,
    operationCorrelation: (operation: { id: Uint8Array }) => ({
      protocolSessionId: createV2ProtocolSessionIdentity(id(7)),
      protocolOperationId: createV2ProtocolOperationIdentity(operation.id),
      lane: { id: 1, epoch: 0 },
    }),
  } as unknown as V2ReceiverSessionRuntime
  const lanes = new V2LaneSet()
  lanes.add({ id: 1, fetchBlock: async () => { throw new Error('unused') } }, 'direct')
  const revisions = new V2RevisionService(session, share, bytes(identity.readSecretB64), lanes, {
    now: () => Date.now(), beforeLeaseRelease: () => barrier.promise,
  })
  return { barrier, revisions, share, requests, open: () => revisions.open(fileId, ROUTES),
    close: () => { revisions.close(); lanes.close() } }
}

afterEach(() => vi.useRealTimers())

describe('revision release caller and shared-read ownership', () => {
  it('bounds the idle barrier while renewing another consumer lease and eventually releasing it', async () => {
    vi.useFakeTimers()
    const harness = fixture()
    try {
      const opened = await harness.open()
      const release = opened.release()
      expect(opened.release()).toBe(release)
      const timedOut = expect(release).rejects.toThrow('Revision lease release timed out')
      await vi.advanceTimersByTimeAsync(RELEASE_WAIT)
      await timedOut
      expect(harness.requests).not.toContain(V2_MESSAGE_KIND.releaseLease)
      expect(harness.revisions.leaseError(opened.leaseId)).toBeUndefined()
      await vi.advanceTimersByTimeAsync(RELEASE_WAIT)
      expect(harness.requests).toContain(V2_MESSAGE_KIND.renewLease)
      harness.barrier.resolve()
      await vi.advanceTimersByTimeAsync(0)
      expect(harness.requests.filter(kind => kind === V2_MESSAGE_KIND.releaseLease)).toHaveLength(1)
      expect(harness.revisions.leaseError(opened.leaseId)).toBeInstanceOf(Error)
    } finally { harness.close() }
  })

  it('wakes the departing caller on service closure without releasing a shared lease early', async () => {
    const harness = fixture()
    const opened = await harness.open()
    const releasing = expect(opened.release()).rejects.toMatchObject({ name: 'AbortError' })
    harness.close()
    await releasing
    harness.barrier.resolve()
    await Promise.resolve()
    expect(harness.requests).not.toContain(V2_MESSAGE_KIND.releaseLease)
  })

  it('allows cancelled direct ZIP transfer cleanup to finish even while a shared read retains its lease', async () => {
    vi.useFakeTimers()
    const harness = fixture()
    try {
      const opened = await harness.open()
      const controller = new AbortController()
      const reason = new DOMException('ZIP paused', 'AbortError')
      const reachedOutput = deferred<void>()
      const file = {
        pending: { entry: {
          kind: 'file', id: opened.descriptor.fileId, idText: opened.descriptor.fileIdText,
          expectedSize: opened.descriptor.exactSize, name: 'file.txt',
        } },
      } as DirectZipOrderedFileV1
      const output = {
        beginFile: async () => {
          controller.abort(reason)
          reachedOutput.resolve()
          throw reason
        },
      } as unknown as DirectZipOutputSessionV1
      const transfer = expect(transferDirectZipFileV1({
        descriptor: harness.share, revisions: { open: async () => opened },
        broker: { readRange: () => { throw new Error('cancelled ZIP must not read') } },
        output, signal: controller.signal, onWriteAcknowledged: () => undefined, onComplete: () => undefined,
      }, file)).rejects.toMatchObject({
        cause: reason,
        errors: [reason, expect.objectContaining({ message: 'Revision lease release timed out' })],
      })
      await reachedOutput.promise
      await vi.advanceTimersByTimeAsync(RELEASE_WAIT)
      await transfer
      expect(harness.requests).not.toContain(V2_MESSAGE_KIND.releaseLease)
      harness.barrier.resolve()
      await vi.advanceTimersByTimeAsync(0)
      expect(harness.requests).toContain(V2_MESSAGE_KIND.releaseLease)
    } finally { harness.close() }
  })
})
