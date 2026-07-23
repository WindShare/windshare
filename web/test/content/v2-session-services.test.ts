import { afterEach, describe, expect, it, vi } from 'vitest'

import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import { FileGeometry } from '../../src/content/geometry'
import { V2LaneSet, type V2BlockRouteEligibility } from '../../src/content/v2-broker'
import {
  V2RevisionService,
  V2SessionBlockLane,
} from '../../src/content/v2-session-services'
import type { V2FileRevisionDescriptor } from '../../src/content/v2-records'
import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import type { V2ReceiverSessionRuntime, V2SessionOperation } from '../../src/session/v2-runtime'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

const ALL_ROUTES: V2BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

const share: V2ShareDescriptor = Object.freeze({
  wireVersion: 2,
  suite: 2,
  shareInstance: identity(1),
  shareInstanceId: 'share',
  syntheticRoot: identity(2),
  syntheticRootId: 'root',
  chunkSize: 65_536,
  capabilities: 0n,
  senderPublicKey: new Uint8Array(32).fill(3),
  createdAtSeconds: 1n,
  pathPolicy: V2_PATH_POLICY,
})

const revision: V2FileRevisionDescriptor = Object.freeze({
  shareInstance: share.shareInstance,
  shareInstanceId: share.shareInstanceId,
  fileId: identity(4),
  fileIdText: 'file',
  fileRevision: identity(5),
  fileRevisionText: 'revision',
  exactSize: 1n,
  geometry: new FileGeometry(1n, BigInt(share.chunkSize)),
})

afterEach(() => vi.useRealTimers())

interface IdentityVector extends VectorCase {
  readonly readSecretB64: string
  readonly senderPublicKeyB64: string
  readonly shareInstanceB64: string
  readonly fileIdB64: string
}

interface SenderObjectVector extends VectorCase {
  readonly domain: string
  readonly objectB64: string
}

function vectorBytes(encoded: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(b64ToBytes(encoded))
}

function responseOperation(
  requestKind: number,
  responseKind: number,
  body: Uint8Array,
): V2SessionOperation {
  const operationId = identity(30 + requestKind)
  return {
    id: operationId,
    requestKind: requestKind as V2SessionOperation['requestKind'],
    next: async () => encodeV2Message(
      responseKind as V2SessionOperation['requestKind'],
      operationId,
      body,
    ),
    close: () => undefined,
  }
}

describe('v2 session block lane deadlines', () => {
  it('binds revision OPEN to the activation-eligible physical route', async () => {
    const lanes = new V2LaneSet()
    const unusedLane = (id: number) => ({
      id,
      fetchBlock: async () => Promise.reject(new Error('unused test content lane')),
    })
    lanes.add(unusedLane(1), 'relay')
    const beginFailure = new Error('stop after lane provenance observation')
    let observedLaneId: number | undefined
    const session = {
      beginOperation: async (
        _kind: number,
        _body: Uint8Array,
        options: { readonly laneId?: number } = {},
      ) => {
        observedLaneId = options.laneId
        throw beginFailure
      },
    } as unknown as V2ReceiverSessionRuntime
    const revisions = new V2RevisionService(
      session,
      share,
      new Uint8Array(32).fill(9),
      lanes,
    )

    const canceledRoutes = new V2ConnectivityRouteAuthority()
    const canceled = revisions.open(revision.fileId, canceledRoutes)
    canceledRoutes.close()
    await expect(canceled).rejects.toMatchObject({ name: 'AbortError' })
    expect(observedLaneId).toBeUndefined()

    const peerRoutes = new V2ConnectivityRouteAuthority()
    const pending = revisions.open(revision.fileId, peerRoutes)
    await Promise.resolve()
    expect(observedLaneId).toBeUndefined()
    lanes.add(unusedLane(2), 'peer')
    await expect(pending).rejects.toBe(beginFailure)
    expect(observedLaneId).toBe(2)

    revisions.close()
    lanes.close()
  })

  it('cancels a block operation when the sender withholds every fragment', async () => {
    vi.useFakeTimers()
    const operationId = identity(8)
    const operation: V2SessionOperation = {
      id: operationId,
      requestKind: 7,
      next: (signal?: AbortSignal) => new Promise((_resolve, reject) => {
        const abort = () => reject(signal?.reason)
        signal?.addEventListener('abort', abort, { once: true })
        if (signal?.aborted) abort()
      }),
      close: () => undefined,
    }
    let cancellations = 0
    const session = {
      beginOperation: async () => operation,
      cancelOperation: async () => { cancellations += 1 },
    } as unknown as V2ReceiverSessionRuntime
    const lane = new V2SessionBlockLane(
      1,
      session,
      share,
      new Uint8Array(16).fill(9),
      { leaseError: () => undefined } as never,
    )

    const pending = lane.fetchBlock({
      descriptor: revision,
      leaseId: identity(6),
      localBlockIndex: 0n,
    }, new AbortController().signal)
    const rejected = expect(pending).rejects.toMatchObject({
      name: 'V2FragmentTimeoutError',
      scope: 'lane',
    })
    await vi.advanceTimersByTimeAsync(15_000)

    await rejected
    expect(cancellations).toBe(1)
    lane.close()
  })

  it('retries a lane-scoped lease renewal until a replacement lane succeeds', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const identityVector = loadVectorFile(
      new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
    ).cases[0] as IdentityVector
    const revisionVector = loadVectorFile(
      new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url),
    ).cases.find((candidate) => (candidate as SenderObjectVector).domain ===
      'windshare/v2 object/file-revision') as SenderObjectVector
    const fileId = vectorBytes(identityVector.fileIdB64)
    const leaseId = identity(24)
    const vectorShare: V2ShareDescriptor = Object.freeze({
      ...share,
      shareInstance: vectorBytes(identityVector.shareInstanceB64),
      shareInstanceId: 'vector-share',
      senderPublicKey: vectorBytes(identityVector.senderPublicKeyB64),
      chunkSize: 1 << 20,
    })
    let renewAttempts = 0
    const session = {
      beginOperation: async (kind: number) => {
        if (kind === V2_MESSAGE_KIND.openRevisions) {
          return responseOperation(kind, V2_MESSAGE_KIND.openResults, encodeV2Body(
            new Map<number, unknown>([
              [0, 1],
              [1, [[
                fileId,
                0,
                vectorBytes(revisionVector.objectB64),
                leaseId,
                120_000,
                60_000,
              ]]],
            ]),
          ))
        }
        if (kind === V2_MESSAGE_KIND.renewLease) {
          renewAttempts += 1
          if (renewAttempts === 1) {
            throw new V2SessionRuntimeError('lane', 'old physical lane disappeared')
          }
          return responseOperation(kind, V2_MESSAGE_KIND.leaseResult, encodeV2Body(
            new Map<number, unknown>([
              [0, 1], [1, leaseId], [2, 120_000], [3, 60_000],
            ]),
          ))
        }
        if (kind === V2_MESSAGE_KIND.releaseLease) {
          const operationId = identity(29)
          return {
            id: operationId,
            requestKind: kind,
            next: (signal?: AbortSignal) => new Promise((_resolve, reject) => {
              const abort = () => reject(signal?.reason)
              signal?.addEventListener('abort', abort, { once: true })
              if (signal?.aborted) abort()
            }),
            close: () => undefined,
          } satisfies V2SessionOperation
        }
        throw new Error(`unexpected operation kind ${kind}`)
      },
    } as unknown as V2ReceiverSessionRuntime
    const lanes = new V2LaneSet()
    lanes.add({
      id: 1,
      fetchBlock: async () => Promise.reject(new Error('unused test content lane')),
    }, 'relay')
    const revisions = new V2RevisionService(
      session,
      vectorShare,
      vectorBytes(identityVector.readSecretB64),
      lanes,
      { now: () => Date.now() },
    )

    const opened = await revisions.open(fileId, ALL_ROUTES)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(renewAttempts).toBe(1)
    await vi.advanceTimersByTimeAsync(250)

    expect(renewAttempts).toBe(2)
    expect(revisions.leaseError(opened.leaseId)).toBeUndefined()
    await vi.advanceTimersByTimeAsync(59_750)
    expect(revisions.leaseError(opened.leaseId)).toBeUndefined()
    const releasing = expect(opened.release()).rejects.toMatchObject({
      scope: 'lane',
    })
    await vi.advanceTimersByTimeAsync(30_000)
    await releasing
    revisions.close()
    lanes.close()
  })
})
