import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import type { V2ContentLaneAdmissionObservation } from '../../src/connectivity/v2-receiver-policy'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import type { V2AttachedRelay, V2ProtocolGenerationCore, V2ReceiverSessionFactory } from '../../src/receiver/v2-session-factory'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import { createV2ProtocolSessionIdentity, type V2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import type { V2ProtocolTraceSource } from '../../src/session/v2-diagnostics'
import type { V2LaneChange } from '../../src/session/v2-runtime-types'
import type { V2RelayReceiverConnection } from '../../src/transport/relay/v2-receiver'

export class FakeSession {
  subscribePeerPathControls(): () => void { return () => undefined }
  async sendPeerPathControl(): Promise<void> {}
  readonly initialLaneId: number
  readonly keys: { readonly protocolSessionId: Uint8Array<ArrayBuffer>; readonly initialLaneEpoch: 0 }
  readonly protocolSessionIdentity: V2ProtocolSessionIdentity
  readonly #laneIds: Set<number>
  readonly #listeners = new Set<(change: V2LaneChange) => void>()
  closeCalls = 0
  isClosed = false

  constructor(laneIds: readonly number[]) {
    if (laneIds.length === 0) throw new Error('A fake generation needs an initial lane')
    this.initialLaneId = laneIds[0]!
    this.keys = Object.freeze({
      protocolSessionId: identity(this.initialLaneId + 100),
      initialLaneEpoch: 0,
    })
    this.protocolSessionIdentity = createV2ProtocolSessionIdentity(this.keys.protocolSessionId)
    this.#laneIds = new Set(laneIds)
  }

  laneIds(): readonly number[] {
    return [...this.#laneIds]
  }

  subscribeLaneChanges(listener: (change: V2LaneChange) => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  attach(laneId: number): void {
    this.#laneIds.add(laneId)
    this.#emit({ type: 'attached', laneId, laneEpoch: 1 })
  }

  detach(laneId: number, failure?: unknown): void {
    this.#laneIds.delete(laneId)
    this.#emit({
      type: 'detached',
      laneId,
      laneEpoch: 1,
      ...(failure === undefined ? {} : { failure }),
    })
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    this.isClosed = true
  }

  #emit(change: V2LaneChange): void {
    for (const listener of this.#listeners) listener(change)
  }
}

export class TrackedRelay {
  closeCalls = 0
  readonly connection: V2RelayReceiverConnection

  constructor(id: number) {
    this.connection = {
      endpoint: {} as V2RelayReceiverConnection['endpoint'],
      relaySessionId: identity(id),
      descriptorObject: Uint8Array.of(id),
      channel: {} as V2RelayReceiverConnection['channel'],
      close: async () => { this.closeCalls += 1 },
    }
  }
}

export class FakeSessionFactory implements V2ReceiverSessionFactory {
  readonly relayBases = ['https://relay.example']
  attachRelayCalls = 0
  connectFreshCalls = 0
  closeCalls = 0
  attachRelayImpl: (
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ) => Promise<V2AttachedRelay> = async () => {
    throw new Error('Unexpected relay attachment')
  }
  connectFreshImpl: (signal: AbortSignal) => Promise<V2ProtocolGenerationCore> = async () => {
    throw new Error('Unexpected fresh generation')
  }
  #closed = false

  attachRelay(
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ): Promise<V2AttachedRelay> {
    this.attachRelayCalls += 1
    return this.attachRelayImpl(session, signal, relayBase)
  }

  connectFresh(signal: AbortSignal): Promise<V2ProtocolGenerationCore> {
    this.connectFreshCalls += 1
    return this.connectFreshImpl(signal)
  }

  copyReadSecret(): Uint8Array<ArrayBuffer> {
    return new Uint8Array(32).fill(7)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.closeCalls += 1
  }
}

export interface Deferred<T> {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accepted) => { resolve = accepted })
  return { promise, resolve }
}

export function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}


export function descriptor(seed = 1): V2ShareDescriptor {
  return Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance: identity(seed),
    shareInstanceId: `share-${seed}`,
    syntheticRoot: identity(seed + 1),
    syntheticRootId: `root-${seed}`,
    chunkSize: 65_536,
    capabilities: 0n,
    senderPublicKey: new Uint8Array(32).fill(seed + 2),
    createdAtSeconds: BigInt(seed),
    pathPolicy: V2_PATH_POLICY,
  })
}

export function core(
  session: FakeSession,
  relay: TrackedRelay,
  relayLaneId = session.initialLaneId,
): V2ProtocolGenerationCore {
  return {
    relayBase: 'https://relay.example',
    session: session as unknown as V2ReceiverSessionRuntime,
    relay: relay.connection,
    relayLaneId,
  }
}

export function supervisorFixture(
  session: FakeSession,
  relay: TrackedRelay,
  factory = new FakeSessionFactory(),
  share = descriptor(),
  onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void,
  protocolTrace?: V2ProtocolTraceSource,
) {
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: share,
    initial: core(session, relay),
    sessionFactory: factory,
    randomBytes: (length) => new Uint8Array(length).fill(9),
    ...(onContentLaneAdmitted === undefined ? {} : { onContentLaneAdmitted }),
    ...(protocolTrace === undefined ? {} : { protocolTrace }),
  })
  return { factory, supervisor }
}
