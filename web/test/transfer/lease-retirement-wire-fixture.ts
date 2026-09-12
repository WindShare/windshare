import type { FrameChannel, ChannelState } from '../../src/contracts/channel'
import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import { concatBytes, encodeBase64Url } from '../../src/crypto/bytes'
import { createRevisionObjectBinding, senderObjectAuthenticationData, senderObjectSignaturePreimage } from '../../src/crypto/sender-object'
import { deriveSuite02FileObjectKey } from '../../src/crypto/suite02-key-derivation'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import type { V2ReceiverSessionOptions } from '../../src/session/v2-runtime-types'
import type { V2SessionKeys } from '../../src/session/v2-transcript'
import { V2EnvelopeOpener, V2EnvelopeSealer } from '../../src/session/v2-envelope'
import { v2LaneAcceptBody } from '../../src/session/v2-lane-codec'
import { decodeV2Message, encodeV2Body, V2_MESSAGE_KIND, type V2SessionMessage, type V2MessageKind } from '../../src/session/v2-message'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import { V2LaneSet } from '../../src/content/v2-broker'
import { V2RevisionService } from '../../src/content/v2-session-services'
import type { LeaseRetirementObservation } from '../../src/content/scheduling/lease-retirement'
import { V2SupervisedContent, type V2ContentGenerationProvider } from '../../src/receiver/v2-supervised-content'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import { senderControlKeyPair, signSenderOperationControl } from '../session/signed-control-fixture'
import { deferred } from '../session/v2-send-fixture'
import { identity, fileEntry, readerFixture } from './v2-job-fixture'

const RELAY = 1
const DIRECT = 2
const DIRECT_EPOCH = 2
const LEASE_TTL_MS = 120_000
const LEASE_RENEW_MS = 60_000
const DIRECT_BUSY_MS = 1_000
const SECRET = new Uint8Array(16).fill(7)
const KEY = new Uint8Array(32).fill(8)
const SESSION_ID = identity(9)
const FILE_BYTES = 4n

class ScriptedWire implements FrameChannel {
  state: ChannelState = 'open'
  readonly frames: ReadableStream<Uint8Array>
  readonly requests: V2SessionMessage[] = []
  readonly errors: unknown[] = []
  #controller!: ReadableStreamDefaultController<Uint8Array>
  #opener: V2EnvelopeOpener
  #sealer: V2EnvelopeSealer
  #admitted: boolean

  readonly id: number
  readonly epoch: number
  readonly share: V2ShareDescriptor
  readonly signing: Awaited<ReturnType<typeof senderControlKeyPair>>
  readonly handle: (wire: ScriptedWire, message: V2SessionMessage) => Promise<void>
  #responses = Promise.resolve()

  constructor(id: number, epoch: number, share: V2ShareDescriptor,
    signing: Awaited<ReturnType<typeof senderControlKeyPair>>,
    handle: (wire: ScriptedWire, message: V2SessionMessage) => Promise<void>,
  ) {
    this.id = id
    this.epoch = epoch
    this.share = share
    this.signing = signing
    this.handle = handle
    this.#admitted = id === RELAY
    const binding = { shareInstance: share.shareInstance, protocolSessionId: SESSION_ID, laneId: id, laneEpoch: epoch }
    this.#opener = new V2EnvelopeOpener(KEY, { ...binding, direction: 0 })
    this.#sealer = new V2EnvelopeSealer(KEY, { ...binding, direction: 1 })
    this.frames = new ReadableStream({ start: controller => { this.#controller = controller } })
  }

  async send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    if (this.state === 'closed') throw new Error('Wire is closed')
    if (!this.#admitted) {
      const body = await v2LaneAcceptBody(frame, identity(15))
      const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', body))
      const preimage = concatBytes([new TextEncoder().encode('windshare/v2 lane-accept\0'), digest])
      const signature = new Uint8Array(await crypto.subtle.sign('Ed25519', this.signing.privateKey, preimage))
      this.#admitted = true
      this.#controller.enqueue(concatBytes([body, signature]))
      return
    }
    const opened = await this.#opener.open(frame)
    const message = decodeV2Message(opened.plaintext)
    this.requests.push(message)
    // Transport acceptance must precede the response; the held reply models a
    // connection disappearing after the sender could already have handled it.
    this.handle(this, message).catch(error => { this.errors.push(error); this.#controller.error(error) })
  }

  respond(message: V2SessionMessage, kind: V2MessageKind, body: Uint8Array): Promise<void> {
    this.#responses = this.#responses.then(() => this.#respond(message, kind, body))
    return this.#responses
  }

  async #respond(message: V2SessionMessage, kind: V2MessageKind, body: Uint8Array): Promise<void> {
    const signed = await signSenderOperationControl({
      kind, operationId: message.operationId!, semanticBody: body,
      binding: { shareInstance: this.share.shareInstance, protocolSessionId: SESSION_ID,
        laneId: this.id, laneEpoch: this.epoch, direction: 1, sequence: this.#sealer.nextSequence },
      privateKey: this.signing.privateKey,
    })
    const sealed = await this.#sealer.seal(signed.message.plaintext)
    if (this.state !== 'closed') this.#controller.enqueue(sealed)
  }

  sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> { return this.send(frame, signal) }

  async close(): Promise<void> {
    if (this.state === 'closed') return
    this.state = 'closed'
    try { this.#controller.close() } catch { /* Runtime cancellation may have closed the reader. */ }
  }
}

async function sealedRevision(share: V2ShareDescriptor, fileId: Uint8Array, signing: Awaited<ReturnType<typeof senderControlKeyPair>>) {
  const body = encodeCanonicalCbor(new Map<number, unknown>([
    [0, 1n], [1, share.shareInstance], [2, fileId], [3, identity(fileId[0]! + 40)],
    [4, FILE_BYTES], [5, null], [6, 0n], [7, 0n],
  ]))
  const binding = createRevisionObjectBinding(share.shareInstance, fileId)
  const header = new Uint8Array(8)
  header[0] = 2
  new DataView(header.buffer).setUint32(4, body.byteLength + 16, false)
  const nonce = crypto.getRandomValues(new Uint8Array(12))
  const keyBytes = await deriveSuite02FileObjectKey(SECRET, share.shareInstance, fileId)
  const key = await crypto.subtle.importKey('raw', keyBytes, 'AES-GCM', false, ['encrypt'])
  const ciphertext = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce,
    additionalData: await senderObjectAuthenticationData(binding, header), tagLength: 128 }, key, body))
  const prefix = concatBytes([header, nonce, ciphertext])
  const signature = new Uint8Array(await crypto.subtle.sign('Ed25519', signing.privateKey,
    await senderObjectSignaturePreimage(binding, prefix)))
  return concatBytes([prefix, signature])
}

export async function wireFixture(options: {
  beforeLeaseRelease?: () => Promise<void>
  releaseCompletion?: 'invalid'
} = {}) {
  const signing = await senderControlKeyPair()
  const share: V2ShareDescriptor = {
    wireVersion: 2, suite: 2, shareInstance: identity(1), shareInstanceId: encodeBase64Url(identity(1)),
    syntheticRoot: identity(2), syntheticRootId: encodeBase64Url(identity(2)), chunkSize: 4,
    capabilities: 0n, senderPublicKey: signing.publicKey, createdAtSeconds: 1n, pathPolicy: V2_PATH_POLICY,
  }
  const files = [fileEntry(identity(11), 'first.bin', FILE_BYTES), fileEntry(identity(12), 'second.bin', FILE_BYTES)]
  const objects = new Map(await Promise.all(files.map(async file => [file.idText, await sealedRevision(share, file.id, signing)] as const)))
  const releaseReached = deferred<void>()
  let releaseReply: (() => Promise<void>) | undefined
  const releaseCompletion = () => encodeV2Body(new Map([[0, 1], [1, options.releaseCompletion === 'invalid' ? 1 : 0]]))
  let held = false
  let leaseSequence = 30
  const handle = async (wire: ScriptedWire, message: V2SessionMessage) => {
    if (message.kind === V2_MESSAGE_KIND.openRevisions) {
      const { decodeCanonicalCbor } = await import('../../src/protocol/cbor')
      const fields = decodeCanonicalCbor(message.body, 65_536, 'open request') as [Uint8Array, unknown][]
      const fileId = fields[0]![0]
      const object = objects.get(encodeBase64Url(fileId))
      if (object === undefined) throw new Error('Unknown authenticated file')
      await wire.respond(message, V2_MESSAGE_KIND.openResults, encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, [[fileId, 0, object, identity(leaseSequence++), LEASE_TTL_MS, LEASE_RENEW_MS]]],
      ])))
    } else if (message.kind === V2_MESSAGE_KIND.releaseLease) {
      if (!held) {
        held = true
        releaseReply = () => wire.respond(message, V2_MESSAGE_KIND.operationComplete, releaseCompletion())
        releaseReached.resolve()
      } else await wire.respond(message, V2_MESSAGE_KIND.operationComplete, releaseCompletion())
    } else if (message.kind !== V2_MESSAGE_KIND.cancel) throw new Error('Unexpected message kind ' + message.kind)
  }
  const relay = new ScriptedWire(RELAY, 0, share, signing, handle)
  const direct = new ScriptedWire(DIRECT, DIRECT_EPOCH, share, signing, handle)
  const events: V2ProtocolTraceEvent[] = []
  const keys: V2SessionKeys = { protocolSessionId: SESSION_ID.slice(), transcriptHash: new Uint8Array(32),
    receiverToSenderKey: KEY.slice(), senderToReceiverKey: KEY.slice(), initialLaneId: RELAY, initialLaneEpoch: 0 }
  const Runtime = V2ReceiverSessionRuntime as unknown as new (
    options: V2ReceiverSessionOptions, keys: V2SessionKeys, receiverInstanceId: Uint8Array,
    reader: ReadableStreamDefaultReader<Uint8Array>,
  ) => V2ReceiverSessionRuntime
  const runtime = new Runtime({ descriptor: share, readSecret: SECRET.slice(), initialChannel: relay,
    protocolTrace: { current: event => events.push(event) } }, keys, identity(6), relay.frames.getReader())
  const adoption = await runtime.adoptGrantedLane(direct, {
    laneId: DIRECT, laneEpoch: DIRECT_EPOCH, grantOperationId: identity(16), attachNonce: identity(17),
  })
  if (adoption.installation !== 'installed') throw new Error('Direct lane admission failed', { cause: adoption })
  const lanes = new V2LaneSet()
  lanes.add({ id: RELAY, fetchBlock: async () => { throw new Error('Content supplied by bounded output fixture') } }, 'application-relay', 0)
  lanes.add({ id: DIRECT, fetchBlock: async () => { throw new Error('Content supplied by bounded output fixture') } }, 'direct', DIRECT_EPOCH)
  // A queued direct lane makes the first retirement use relay, as in the incident.
  lanes.requests.add({ id: DIRECT, epoch: DIRECT_EPOCH, route: 'direct', content: { estimateQueueMilliseconds: () => DIRECT_BUSY_MS } })
  runtime.subscribeLaneChanges(change => { if (change.type === 'detached') lanes.remove(change.laneId) })
  const retirements: LeaseRetirementObservation[] = []
  const revisions = new V2RevisionService(runtime, share, SECRET, lanes, {
    ...options, onLeaseRetirement: event => retirements.push(event),
  })
  const readers = readerFixture(files)
  const generation = { id: 1, revisions, lanes, broker: { readRouteAuthorizedRange: readers.broker.readRange } }
  const provider: V2ContentGenerationProvider = {
    execute: async (signal, operation) => { signal?.throwIfAborted(); return { generation, value: await operation(generation) } },
    isCurrent: value => value === generation,
    recover: async () => false,
    contentLaneCount: () => lanes.size,
  }
  const content = new V2SupervisedContent(provider)
  const scoped = content.forRoutes(new V2ConnectivityRouteAuthority())
  return { share, files, relay, direct, runtime, lanes, revisions, scoped, events, retirements, releaseReached,
    completeRelease: async () => {
      if (releaseReply === undefined) throw new Error('No held release')
      await releaseReply()
    },
    close: async () => { content.close(); revisions.close(); lanes.close(); await runtime.close() },
  }
}
