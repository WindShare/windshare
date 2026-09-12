import { expect, it } from 'vitest'
import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import { V2LaneSet } from '../../src/content/v2-broker'
import { V2RevisionService, V2RemoteRevisionError } from '../../src/content/v2-session-services'
import { V2OperationRouter } from '../../src/session/v2-operation-router'
import { createV2ProtocolSessionIdentity, createV2ProtocolOperationIdentity } from '../../src/session/v2-identities'
import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import type { V2SessionOperation, V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'

const identity = (first: number) => Uint8Array.from([first, ...Array<number>(15).fill(0)])

it.each([true, false])('preserves OPEN_RESULTS receive context after finalization (trace=%s)', async (traceEnabled) => {
  const protocolSessionIdentity = createV2ProtocolSessionIdentity(identity(7))
  const events: V2ProtocolTraceEvent[] = []
  const router = new V2OperationRouter(() => undefined, () => 1000, {
    protocolSessionIdentity,
    ...(traceEnabled ? { trace: { current: (event: V2ProtocolTraceEvent) => events.push(event) } } : {}),
  })
  const operationId = identity(26)
  let ready!: () => void
  const admitted = new Promise<void>(resolve => { ready = resolve })
  const session = {
    beginOperation: async (
      kind: V2SessionOperation['requestKind'], body: Uint8Array, options: { laneId?: number },
    ) => {
      expect(options.laneId).toBe(1)
      const operation = router.create(operationId, kind, body)
      ready()
      return operation
    },
    operationCorrelation: (operation: V2SessionOperation) => ({
      protocolSessionId: protocolSessionIdentity,
      protocolOperationId: createV2ProtocolOperationIdentity(operation.id),
      lane: { id: 1, epoch: 9 },
    }),
    authenticatedResponseCorrelation: (message: Parameters<V2OperationRouter['receivedCorrelationFor']>[0]) => {
      expect(router.active()).toHaveLength(0)
      const correlation = router.receivedCorrelationFor(message)
      if (correlation === undefined) throw new Error('Missing authenticated response context')
      return correlation
    },
  } as unknown as V2ReceiverSessionRuntime
  const share: V2ShareDescriptor = {
    wireVersion: 2, suite: 2, shareInstance: identity(1), shareInstanceId: 'share',
    syntheticRoot: identity(2), syntheticRootId: 'root', chunkSize: 65536,
    capabilities: 0n, senderPublicKey: new Uint8Array(32).fill(3),
    createdAtSeconds: 1n, pathPolicy: V2_PATH_POLICY,
  }
  const fileId = identity(4)
  const lanes = new V2LaneSet()
  lanes.add({ id: 1, fetchBlock: async () => { throw new Error('unused') } }, 'application-relay')
  const revisions = new V2RevisionService(session, share, new Uint8Array(16).fill(9), lanes)
  try {
    const opened = revisions.open(fileId, {
      active: true, allows: () => true, assertActive: () => undefined,
      subscribe: () => () => undefined,
    }).catch(error => error as unknown)
    await admitted
    await router.route(encodeV2Message(V2_MESSAGE_KIND.openResults, operationId,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, [[fileId, 1, 0x3001, false, null]]]]))), 7, 3)
    const error = await opened
    expect(error).toBeInstanceOf(V2RemoteRevisionError)
    if (!(error instanceof V2RemoteRevisionError)) throw error
    const response = events.find(event => event.eventName === 'protocol_operation' && event.transition === 'response_received')
    if (traceEnabled) expect(response?.correlation.lane).toEqual({ id: 7, epoch: 3 })
    else expect(events).toHaveLength(0)
    expect(error.protocolFailure.correlation.lane).toEqual({ id: 7, epoch: 3 })
    expect(error.failureFact.payload.protocolFailure.correlation.lane).toEqual({ id: 7, epoch: 3 })
    expect(error.protocolFailure.correlation.protocolOperationId.copyBytes()).toEqual(operationId)
    expect(error.protocolFailure.correlation.protocolSessionId).toEqual(protocolSessionIdentity)
    expect(Object.isFrozen(error.protocolFailure.correlation.lane)).toBe(true)
  } finally {
    revisions.close()
    lanes.close()
    router.terminate(new Error('test complete'))
  }
})
