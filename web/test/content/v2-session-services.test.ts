import { afterEach, describe, expect, it, vi } from 'vitest'

import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import {
  createProtocolFailure,
  type ProtocolFailure,
} from '../../src/diagnostics/incident/fact'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import { FileGeometry } from '../../src/content/geometry'
import { V2LaneSet, type V2BlockRouteEligibility } from '../../src/content/v2-broker'
import { V2_REVISION_CODE_QUOTA } from '../../src/content/v2-flow'
import {
  V2RemoteOperationError,
  V2RemoteRevisionError,
  V2RevisionCapacityBusyError,
  V2RevisionService,
  V2SessionBlockLane,
} from '../../src/content/v2-session-services'
import type { V2FileRevisionDescriptor } from '../../src/content/v2-records'
import {
  decodeV2Message,
  decodeV2OperationErrorControl,
  encodeV2Body,
  encodeV2Message,
  type V2SessionMessage,
  V2_MESSAGE_KIND,
} from '../../src/session/v2-message'
import type { V2ReceiverSessionRuntime, V2SessionOperation } from '../../src/session/v2-runtime'
import {
  createV2ProtocolOperationIdentity,
  createV2ProtocolSessionIdentity,
} from '../../src/session/v2-identities'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
import { fragmentRecord } from './v2-fragment-fixture'
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

function blockProtocolFailure(message: V2SessionMessage) {
  const decoded = decodeV2OperationErrorControl(message.body)
  return createProtocolFailure({
    requestKind: 'request_blocks',
    wireScope: decoded.scope,
    wireCode: decoded.code,
    retryable: decoded.retryable,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: Object.freeze({
      protocolSessionId: createV2ProtocolSessionIdentity(identity(7)),
      protocolOperationId: createV2ProtocolOperationIdentity(message.operationId!),
      lane: Object.freeze({ id: 1, epoch: 1 }),
    }),
  })
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
    cancel: () => undefined,
  }
}

function revisionOperationCorrelation(
  operation: V2SessionOperation,
): ProtocolFailure['correlation'] {
  return Object.freeze({
    protocolSessionId: createV2ProtocolSessionIdentity(identity(7)),
    protocolOperationId: createV2ProtocolOperationIdentity(operation.id),
    lane: Object.freeze({ id: 1, epoch: 9 }),
  })
}

async function openRevisionFailure(input: {
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
}): Promise<{ readonly error: unknown; readonly correlationCapturedBeforeFinal: boolean }> {
  const operationId = identity(26)
  let finalRead = false
  let correlationCapturedBeforeFinal = false
  const operation: V2SessionOperation = {
    id: operationId,
    requestKind: V2_MESSAGE_KIND.openRevisions,
    next: async () => {
      finalRead = true
      return encodeV2Message(
        V2_MESSAGE_KIND.openResults,
        operationId,
        encodeV2Body(new Map<number, unknown>([
          [0, 1],
          [1, [[
            revision.fileId,
            1,
            input.code,
            input.retryable,
            input.retryAfterMilliseconds ?? null,
          ]]],
        ])),
      )
    },
    cancel: () => undefined,
  }
  const session = {
    beginOperation: async () => operation,
    operationCorrelation: (candidate: V2SessionOperation) => {
      correlationCapturedBeforeFinal = !finalRead
      return revisionOperationCorrelation(candidate)
    },
  } as unknown as V2ReceiverSessionRuntime
  const lanes = new V2LaneSet()
  lanes.add({
    id: 1,
    fetchBlock: async () => Promise.reject(new Error('unused test content lane')),
  }, 'application-relay')
  const revisions = new V2RevisionService(
    session,
    share,
    new Uint8Array(16).fill(9),
    lanes,
  )
  let error: unknown
  try {
    await revisions.open(revision.fileId, ALL_ROUTES)
  } catch (caught) {
    error = caught
  } finally {
    revisions.close()
    lanes.close()
  }
  if (error === undefined) throw new Error('Expected revision open to fail')
  return { error, correlationCapturedBeforeFinal }
}

describe('v2 authenticated revision capacity results', () => {
  it('creates retry authority only for the exact authenticated quota tuple', async () => {
    const { error, correlationCapturedBeforeFinal } = await openRevisionFailure({
      code: V2_REVISION_CODE_QUOTA,
      retryable: true,
      retryAfterMilliseconds: 275,
    })

    expect(error).toBeInstanceOf(V2RevisionCapacityBusyError)
    if (!(error instanceof V2RevisionCapacityBusyError)) throw error
    expect(correlationCapturedBeforeFinal).toBe(true)
    expect(error.retryAfterMilliseconds).toBe(275)
    expect(error.failure).toEqual({
      code: V2_REVISION_CODE_QUOTA,
      retryable: true,
      retryAfterMilliseconds: 275,
    })
    expect(error.protocolFailure).toMatchObject({
      requestKind: 'open_revisions',
      wireScope: 'revision',
      wireCode: V2_REVISION_CODE_QUOTA,
      retryable: true,
      retryAfterMilliseconds: 275,
      settlement: { kind: 'received_authenticated' },
      correlation: { lane: { id: 1, epoch: 9 } },
    })
    expect(error.protocolFailure.correlation.protocolSessionId.copyBytes()).toEqual(identity(7))
    expect(error.protocolFailure.correlation.protocolOperationId.copyBytes()).toEqual(identity(26))
    expect(error.failureFact.recoveryDisposition).toBe('none')
    expect(error.failureFact.payload.protocolFailure).toEqual(error.protocolFailure)
  })

  it('does not promote a generic authenticated operation error into capacity authority', async () => {
    const operation = responseOperation(
      V2_MESSAGE_KIND.openRevisions,
      V2_MESSAGE_KIND.operationError,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, 3], [2, V2_REVISION_CODE_QUOTA], [3, true], [4, 275],
        [5, 'sender operation is busy'],
      ])),
    )
    const protocolFailure = createProtocolFailure({
      requestKind: 'open_revisions',
      wireScope: 'revision',
      wireCode: V2_REVISION_CODE_QUOTA,
      retryable: true,
      retryAfterMilliseconds: 275,
      settlement: Object.freeze({ kind: 'received_authenticated' }),
      correlation: revisionOperationCorrelation(operation),
    })
    const session = {
      beginOperation: async () => operation,
      operationCorrelation: revisionOperationCorrelation,
      authenticatedProtocolFailure: () => protocolFailure,
    } as unknown as V2ReceiverSessionRuntime
    const lanes = new V2LaneSet()
    lanes.add({
      id: 1,
      fetchBlock: async () => Promise.reject(new Error('unused test content lane')),
    }, 'application-relay')
    const revisions = new V2RevisionService(
      session,
      share,
      new Uint8Array(16).fill(9),
      lanes,
    )

    let error: unknown
    try {
      await revisions.open(revision.fileId, ALL_ROUTES)
    } catch (caught) {
      error = caught
    } finally {
      revisions.close()
      lanes.close()
    }
    expect(error).toBeInstanceOf(V2RemoteOperationError)
    expect(error).not.toBeInstanceOf(V2RevisionCapacityBusyError)
  })

  it.each([
    ['non-retryable quota or seal budget', V2_REVISION_CODE_QUOTA, false, undefined],
    ['source missing', 0x3002, false, undefined],
    ['source unreadable', 0x3003, false, undefined],
    ['unsupported source stability', 0x3004, false, undefined],
    ['source drift', 0x3007, false, undefined],
    ['retryable non-capacity revision result', 0x3001, true, 50],
  ] as const)(
    'keeps %s as an ordinary correlated revision failure',
    async (_label, code, retryable, retryAfterMilliseconds) => {
      const { error } = await openRevisionFailure({
        code,
        retryable,
        ...(retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds }),
      })

      expect(error).toBeInstanceOf(V2RemoteRevisionError)
      expect(error).not.toBeInstanceOf(V2RevisionCapacityBusyError)
      if (!(error instanceof V2RemoteRevisionError)) throw error
      expect(error.failure).toEqual({
        code,
        retryable,
        ...(retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds }),
      })
      expect(error.protocolFailure).toMatchObject({
        requestKind: 'open_revisions',
        wireScope: 'revision',
        wireCode: code,
        retryable,
        settlement: { kind: 'received_authenticated' },
      })
      expect(error.failureFact.recoveryDisposition).toBe('none')
    },
  )
})

describe('v2 session block lane deadlines', () => {
  it('uses relay immediately while rejecting a closed route authority', async () => {
    const lanes = new V2LaneSet()
    const unusedLane = (id: number) => ({
      id,
      fetchBlock: async () => Promise.reject(new Error('unused test content lane')),
    })
    lanes.add(unusedLane(1), 'application-relay')
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
      new Uint8Array(16).fill(9),
      lanes,
    )

    const canceledRoutes = new V2ConnectivityRouteAuthority()
    canceledRoutes.close()
    const canceled = revisions.open(revision.fileId, canceledRoutes)
    await expect(canceled).rejects.toMatchObject({ name: 'AbortError' })
    expect(observedLaneId).toBeUndefined()

    const peerRoutes = new V2ConnectivityRouteAuthority()
    await expect(revisions.open(revision.fileId, peerRoutes)).rejects.toBe(beginFailure)
    expect(observedLaneId).toBe(1)

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
      cancel: () => undefined,
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
      name: 'V2FragmentInactivityTimeoutError',
      scope: 'lane',
    })
    await vi.advanceTimersByTimeAsync(15_000)

    await rejected
    expect(cancellations).toBe(1)
    lane.close()
  })

  it('keeps a revision-scoped block rejection inside the file boundary', async () => {
    let cancellations = 0
    const close = vi.fn(async () => undefined)
    const session = {
      beginOperation: async () => responseOperation(
        V2_MESSAGE_KIND.requestBlocks,
        V2_MESSAGE_KIND.operationError,
        encodeV2Body(new Map<number, unknown>([
          [0, 1], [1, 3], [2, 0x3008], [3, false], [4, null], [5, 'invalid lease'],
        ])),
      ),
      cancelOperation: async () => { cancellations += 1 },
      close,
      authenticatedProtocolFailure: blockProtocolFailure,
    } as unknown as V2ReceiverSessionRuntime
    const lane = new V2SessionBlockLane(
      1,
      session,
      share,
      new Uint8Array(32).fill(9),
      { leaseError: () => undefined } as never,
    )

    await expect(lane.fetchBlock({
      descriptor: revision,
      leaseId: identity(6),
      localBlockIndex: 0n,
    }, new AbortController().signal)).rejects.toMatchObject({
      name: 'V2RemoteOperationError',
      scope: 'revision',
      code: 0x3008,
    })
    expect(cancellations).toBe(1)
    expect(close).not.toHaveBeenCalled()
    lane.close()
  })

  it('renews the lane deadline when each authenticated fragment advances assembly', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const operationId = identity(9)
    const fragments = fragmentRecord(operationId, new Uint8Array(65_441).fill(0x33))
    const responses = [
      decodeV2Message(fragments[0]!),
      decodeV2Message(fragments[1]!),
      encodeV2Message(
        V2_MESSAGE_KIND.operationComplete,
        operationId,
        encodeV2Body(new Map<number, unknown>([[0, 1], [1, 1]])),
      ),
    ]
    let responseIndex = 0
    const operation: V2SessionOperation = {
      id: operationId,
      requestKind: V2_MESSAGE_KIND.requestBlocks,
      next: (signal?: AbortSignal) => {
        const response = responses[responseIndex++]
        if (response === undefined) return Promise.reject(new Error('missing test response'))
        if (responseIndex === responses.length) return Promise.resolve(response)
        return new Promise((resolve, reject) => {
          const timer = globalThis.setTimeout(() => {
            signal?.removeEventListener('abort', abort)
            resolve(response)
          }, 10_000)
          const abort = () => {
            globalThis.clearTimeout(timer)
            reject(signal?.reason)
          }
          signal?.addEventListener('abort', abort, { once: true })
        })
      },
      cancel: () => undefined,
    }
    const session = {
      beginOperation: async () => operation,
      cancelOperation: async () => undefined,
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
      name: 'V2BlockOperationError',
      code: 'object-auth',
    })
    await vi.advanceTimersByTimeAsync(20_000)

    await rejected
    lane.close()
  })

  it('retries a promised block whose fragments became ambiguous across a lane cut', async () => {
    let cancellations = 0
    const session = {
      beginOperation: async () => responseOperation(
        V2_MESSAGE_KIND.requestBlocks,
        V2_MESSAGE_KIND.operationComplete,
        encodeV2Body(new Map<number, unknown>([[0, 1], [1, 1]])),
      ),
      cancelOperation: async () => { cancellations += 1 },
    } as unknown as V2ReceiverSessionRuntime
    const incomplete = new V2SessionBlockLane(
      1,
      session,
      share,
      new Uint8Array(32).fill(9),
      { leaseError: () => undefined } as never,
    )
    let peerCalls = 0
    const lanes = new V2LaneSet()
    lanes.add(incomplete, 'application-relay')
    lanes.add({
      id: 2,
      fetchBlock: async (input) => {
        peerCalls += 1
        return {
          descriptor: input.descriptor,
          localBlockIndex: input.localBlockIndex,
          data: Uint8Array.of(2),
        }
      },
    }, 'direct')

    await expect(lanes.fetch({
      descriptor: revision,
      leaseId: identity(6),
      localBlockIndex: 0n,
    }, ALL_ROUTES, new AbortController().signal)).resolves.toMatchObject({
      data: Uint8Array.of(2),
    })
    expect(cancellations).toBe(1)
    expect(peerCalls).toBe(1)
    lanes.close()
  })

  it('does not retry an authenticated block completion with the wrong result count', async () => {
    const session = {
      beginOperation: async () => responseOperation(
        V2_MESSAGE_KIND.requestBlocks,
        V2_MESSAGE_KIND.operationComplete,
        encodeV2Body(new Map<number, unknown>([[0, 1], [1, 0]])),
      ),
      cancelOperation: async () => undefined,
    } as unknown as V2ReceiverSessionRuntime
    const invalid = new V2SessionBlockLane(
      1,
      session,
      share,
      new Uint8Array(32).fill(9),
      { leaseError: () => undefined } as never,
    )
    let peerCalls = 0
    const lanes = new V2LaneSet()
    lanes.add(invalid, 'application-relay')
    lanes.add({
      id: 2,
      fetchBlock: async () => {
        peerCalls += 1
        throw new Error('wrong-result-count fallback must not run')
      },
    }, 'direct')

    await expect(lanes.fetch({
      descriptor: revision,
      leaseId: identity(6),
      localBlockIndex: 0n,
    }, ALL_ROUTES, new AbortController().signal)).rejects.toThrow(
      'Block operation completed without exactly one result',
    )
    expect(peerCalls).toBe(0)
    lanes.close()
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
      operationCorrelation: revisionOperationCorrelation,
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
            cancel: () => undefined,
          } satisfies V2SessionOperation
        }
        throw new Error(`unexpected operation kind ${kind}`)
      },
    } as unknown as V2ReceiverSessionRuntime
    const lanes = new V2LaneSet()
    lanes.add({
      id: 1,
      fetchBlock: async () => Promise.reject(new Error('unused test content lane')),
    }, 'application-relay')
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
