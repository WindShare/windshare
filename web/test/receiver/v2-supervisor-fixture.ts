import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { PATH_POLICY, type ShareDescriptor } from '../../src/catalog/records'
import type { ContentLaneAdmissionObservation } from '../../src/connectivity/receiver-policy'
import { ReceiverReconnectSupervisor } from '../../src/receiver/supervisor'
import type { AttachedRelay, ProtocolGenerationCore, ReceiverSessionFactory } from '../../src/receiver/session-factory'
import type { ReceiverSessionRuntime } from '../../src/session/runtime'
import { createProtocolSessionIdentity, type ProtocolSessionIdentity } from '../../src/session/identities'
import type { ProtocolTraceSource } from '../../src/session/diagnostics'
import type { LaneChange } from '../../src/session/runtime-types'
import type { RelayReceiverConnection } from '../../src/transport/relay/receiver'

export class FakeSession {
  subscribePeerPathControls(): () => void { return () => undefined }
  async sendPeerPathControl(): Promise<void> {}
  subscribeTurnControls(): () => void { return () => undefined }
  async sendTurnControl(): Promise<void> {}
  readonly initialLaneId: number
  readonly keys: { readonly protocolSessionId: Uint8Array<ArrayBuffer>; readonly initialLaneEpoch: 0 }
  readonly protocolSessionIdentity: ProtocolSessionIdentity
  readonly #laneIds: Set<number>
  readonly #listeners = new Set<(change: LaneChange) => void>()
  closeCalls = 0
  isClosed = false

  constructor(laneIds: readonly number[]) {
    if (laneIds.length === 0) throw new Error('A fake generation needs an initial lane')
    this.initialLaneId = laneIds[0]!
    this.keys = Object.freeze({
      protocolSessionId: identity(this.initialLaneId + 100),
      initialLaneEpoch: 0,
    })
    this.protocolSessionIdentity = createProtocolSessionIdentity(this.keys.protocolSessionId)
    this.#laneIds = new Set(laneIds)
  }

  laneIds(): readonly number[] {
    return [...this.#laneIds]
  }

  subscribeLaneChanges(listener: (change: LaneChange) => void): () => void {
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

  #emit(change: LaneChange): void {
    for (const listener of this.#listeners) listener(change)
  }
}

export class TrackedRelay {
  closeCalls = 0
  readonly connection: RelayReceiverConnection

  constructor(id: number) {
    this.connection = {
      endpoint: {} as RelayReceiverConnection['endpoint'],
      relaySessionId: identity(id),
      descriptorObject: Uint8Array.of(id),
      channel: {} as RelayReceiverConnection['channel'],
      close: async () => { this.closeCalls += 1 },
    }
  }
}

export class FakeSessionFactory implements ReceiverSessionFactory {
  readonly relayBases = ['https://relay.example']
  attachRelayCalls = 0
  connectFreshCalls = 0
  closeCalls = 0
  attachRelayImpl: (
    session: ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ) => Promise<AttachedRelay> = async () => {
    throw new Error('Unexpected relay attachment')
  }
  connectFreshImpl: (signal: AbortSignal) => Promise<ProtocolGenerationCore> = async () => {
    throw new Error('Unexpected fresh generation')
  }
  #closed = false

  attachRelay(
    session: ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ): Promise<AttachedRelay> {
    this.attachRelayCalls += 1
    return this.attachRelayImpl(session, signal, relayBase)
  }

  connectFresh(signal: AbortSignal): Promise<ProtocolGenerationCore> {
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


export function descriptor(seed = 1): ShareDescriptor {
  return Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance: identity(seed),
    shareInstanceId: `share-${seed}`,
    syntheticRoot: identity(seed + 1),
    syntheticRootId: `root-${seed}`,
    chunkSize: 65_536,
    capabilities: 0n,
    sender: createEd25519Verifier(new Uint8Array(32).fill(seed + 2)),
    createdAtSeconds: BigInt(seed),
    pathPolicy: PATH_POLICY,
  })
}

export function core(
  session: FakeSession,
  relay: TrackedRelay,
  relayLaneId = session.initialLaneId,
): ProtocolGenerationCore {
  return {
    relayBase: 'https://relay.example',
    session: session as unknown as ReceiverSessionRuntime,
    relay: relay.connection,
    relayLaneId,
  }
}

export function supervisorFixture(
  session: FakeSession,
  relay: TrackedRelay,
  factory = new FakeSessionFactory(),
  share = descriptor(),
  onContentLaneAdmitted?: (observation: ContentLaneAdmissionObservation) => void,
  protocolTrace?: ProtocolTraceSource,
) {
  const supervisor = new ReceiverReconnectSupervisor({
    descriptor: share,
    initial: core(session, relay),
    sessionFactory: factory,
    randomBytes: (length) => new Uint8Array(length).fill(9),
    ...(onContentLaneAdmitted === undefined ? {} : { onContentLaneAdmitted }),
    ...(protocolTrace === undefined ? {} : { protocolTrace }),
  })
  return { factory, supervisor }
}
