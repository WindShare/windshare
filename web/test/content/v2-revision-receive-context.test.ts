import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { expect, it } from 'vitest'
import { PATH_POLICY, type ShareDescriptor } from '../../src/catalog/records'
import { LaneSet } from '../../src/content/broker'
import { RevisionService, RemoteRevisionError } from '../../src/content/session-services'
import { OperationRouter } from '../../src/session/operation-router'
import { createProtocolSessionIdentity, createProtocolOperationIdentity } from '../../src/session/identities'
import { encodeBody, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import type { SessionOperation, ReceiverSessionRuntime } from '../../src/session/runtime'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'

const identity = (first: number) => Uint8Array.from([first, ...Array<number>(15).fill(0)])

it.each([true, false])('preserves OPEN_RESULTS receive context after finalization (trace=%s)', async (traceEnabled) => {
  const protocolSessionIdentity = createProtocolSessionIdentity(identity(7))
  const events: ProtocolTraceEvent[] = []
  const router = new OperationRouter(() => undefined, () => 1000, {
    protocolSessionIdentity,
    ...(traceEnabled ? { trace: { current: (event: ProtocolTraceEvent) => events.push(event) } } : {}),
  })
  let operationId!: Uint8Array<ArrayBuffer>
  let ready!: () => void
  const admitted = new Promise<void>(resolve => { ready = resolve })
  const session = {
    beginOperation: async (
      kind: SessionOperation['requestKind'], body: Uint8Array, options: { laneId?: number },
    ) => {
      expect(options.laneId).toBe(1)
      const operation = router.create(kind, body)
      operationId = operation.id
      ready()
      return operation
    },
    operationCorrelation: (operation: SessionOperation) => ({
      protocolSessionId: protocolSessionIdentity,
      protocolOperationId: createProtocolOperationIdentity(operation.id),
      lane: { id: 1, epoch: 9 },
    }),
    authenticatedResponseCorrelation: (message: Parameters<OperationRouter['receivedCorrelationFor']>[0]) => {
      expect(router.active()).toHaveLength(0)
      const correlation = router.receivedCorrelationFor(message)
      if (correlation === undefined) throw new Error('Missing authenticated response context')
      return correlation
    },
  } as unknown as ReceiverSessionRuntime
  const share: ShareDescriptor = {
    wireVersion: 2, suite: 2, shareInstance: identity(1), shareInstanceId: 'share',
    syntheticRoot: identity(2), syntheticRootId: 'root', chunkSize: 65536,
    capabilities: 0n, sender: createEd25519Verifier(new Uint8Array(32).fill(3)),
    createdAtSeconds: 1n, pathPolicy: PATH_POLICY,
  }
  const fileId = identity(4)
  const lanes = new LaneSet()
  lanes.add({ id: 1, fetchBlock: async () => { throw new Error('unused') } }, 'application-relay')
  const revisions = new RevisionService(session, share, new Uint8Array(16).fill(9), lanes)
  try {
    const opened = revisions.open(fileId, {
      active: true, allows: () => true, assertActive: () => undefined,
      subscribe: () => () => undefined,
    }).catch(error => error as unknown)
    await admitted
    await router.route(encodeMessage(MESSAGE_KIND.openResults, operationId,
      encodeBody(new Map<number, unknown>([[0, 1], [1, [[fileId, 1, 0x3001, false, null]]]]))), 7, 3)
    const error = await opened
    expect(error).toBeInstanceOf(RemoteRevisionError)
    if (!(error instanceof RemoteRevisionError)) throw error
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
