import type { TURNTraceStage } from './diagnostics'
import { encodeBase64Url } from '../crypto/bytes'
import { decodeTURNGrants, encodeTURNDemand, MAX_TURN_PATHS, type TURNGrant, type TURNMode } from '../protocol/turn-control'
import { createPeerPathIdentityValue, equalDiagnosticIdentities, type PeerPathIdentity } from '../session/identities'
import { PeerSlotPool, type PeerSlotLease, type PeerAttemptOrdinal } from '../session/peer-slots/slots'
import type { ReceiverSessionRuntime } from '../session/runtime'
import type { PeerAttemptHandle } from './peer-set/contract'
import type { PeerAdmittedLane } from './peer-failure'
import { TURNDeliveryPressure, type TURNContentDemand } from './turn-demand'

const BACKOFF_MS = [2000, 10_000, 30_000] as const
const RESPONSE_MS = 8000
const SETTLING_MS = 2000
const MAXIMUM_TTL_MS = 86_400_000n
const REFRESH_MARGIN_MS = 30_000

interface Retry { failures: number; due: number }
interface FullRequest extends Retry { mode: TURNMode; complete: boolean }
interface Request { mode: TURNMode; owner: Service | FullRequest; until: number }
interface Leg {
  slot?: PeerSlotLease
  identity?: PeerPathIdentity
  handle?: PeerAttemptHandle
  lane?: PeerAdmittedLane
  grantId?: string
  detachedDuringAdmission?: PeerAdmittedLane
  awaitingDelivery?: boolean
  refresh: number
}
interface Service extends Retry { id: string; current: Leg; replacement?: Leg; work?: Promise<void> }
interface Options {
  session: Pick<ReceiverSessionRuntime, 'subscribeTURNControls' | 'sendTURNControl' | 'availableLaneIds' | 'laneEpoch' | 'retireLaneIfCovered'>
  slots: PeerSlotPool
  now(): number
  identity(): Uint8Array
  hasDelivered(lane: PeerAdmittedLane): boolean
  start(grant: TURNGrant, identity: PeerPathIdentity, ordinal: PeerAttemptOrdinal): PeerAttemptHandle
  release(lane: PeerAdmittedLane, retirement?: Promise<void>): Promise<void>
  trace?(
    stage: TURNTraceStage,
    detail: { serviceId?: string; grantId?: string; status?: string; mode?: string; reason?: 'owner_closed' | 'lane_lost' },
    path?: PeerPathIdentity,
    lane?: PeerAdmittedLane,
  ): void
}

function sameLane(a: PeerAdmittedLane | undefined, b: PeerAdmittedLane): boolean {
  return a?.laneId === b.laneId && a.laneEpoch === b.laneEpoch
}

function closeSlot(leg: Leg): void {
  leg.slot?.close()
  delete leg.slot
}

export class TURNPaths {
  readonly #options: Options
  #services = new Map<string, Service>()
  readonly #identities = new Map<number, PeerPathIdentity>()
  readonly #requests = new Map<bigint, Request>()
  readonly #pressure = new TURNDeliveryPressure()
  #unsubscribe: (() => void) | undefined
  #next = 0n
  #full: FullRequest | undefined
  #disabled = false
  #resetting: Promise<void> | undefined
  settlingUntil = 0

  constructor(options: Options) { this.#options = options }

  owns(path: PeerPathIdentity): boolean {
    return [...this.#services.values()].some((s) =>
      [s.current, s.replacement].some((l) => l?.identity && equalDiagnosticIdentities(l.identity, path)),
    )
  }

  step(demand: TURNContentDemand): void {
    if (this.#disabled || this.#resetting) return
    const now = this.#options.now()
    for (const [id, request] of this.#requests) {
      if (now < request.until) continue
      this.#requests.delete(id)
      this.#retry(request.owner)
    }
    this.#observeDeliveries()
    const mode = this.#pressure.observe(now, demand)
    if ((!mode && !this.#services.size) || !this.#options.session.availableLaneIds().length) return
    if (mode && this.#hasRoom()) this.#considerFull(mode, now)
    if (demand.unfinishedBytes === 0 && demand.networkWaitingBytes === 0) return
    for (const service of this.#services.values()) {
      if (!service.work && now >= service.due && (!service.current.lane || now >= service.current.refresh)) {
        this.#request('all', service)
      }
    }
  }

  #observeDeliveries(): void {
    for (const service of this.#services.values()) {
      const leg = service.current
      if (leg.awaitingDelivery && leg.lane && this.#options.hasDelivered(leg.lane)) {
        delete leg.awaitingDelivery
        service.failures = service.due = 0
      }
    }
  }

  #considerFull(mode: TURNMode, now: number): void {
    if (this.#full?.mode === 'all' && mode === 'parallel') return
    if ([...this.#requests.values()].some((request) => !('id' in request.owner))) return
    // A quiet fallback must still be reachable after the parallel issuers exhausted their retries.
    if (this.#full?.mode !== mode) this.#full = { mode, complete: false, failures: 0, due: 0 }
    if (!this.#full.complete && now >= this.#full.due) this.#request(mode)
  }

  #request(mode: TURNMode, service?: Service): void {
    const owner = service ?? this.#full
    if (!owner || [...this.#requests.values()].some((r) => r.owner === owner)) return
    this.#unsubscribe ??= this.#options.session.subscribeTURNControls((body) => this.#accept(body))
    const id = ++this.#next
    this.#requests.set(id, { mode, owner, until: this.#options.now() + RESPONSE_MS })
    this.#options.trace?.(service ? 'turn_refresh_requested' : 'turn_demand_sent', {
      mode, ...(service && { serviceId: service.id }), ...(service?.current.grantId && { grantId: service.current.grantId }),
    }, service?.current.identity, service?.current.lane)
    if (!this.#requests.has(id)) return
    void this.#options.session.sendTURNControl(encodeTURNDemand({
      requestId: id, mode, serviceIds: service ? [service.id] : [],
    })).catch(() => {
      if (this.#requests.delete(id)) this.#retry(owner)
    })
  }

  #accept(body: Uint8Array): void {
    const grants = decodeTURNGrants(body)
    const request = this.#requests.get(grants.requestId)
    if (!request) return
    this.#requests.delete(grants.requestId)
    const activation = this.#services
    const owner = request.owner
    const service = 'id' in owner ? owner : undefined
    const targetedGrant = service && grants.grants.find((grant) => grant.serviceId === service.id)
    this.#options.trace?.('turn_grants_received', {
      status: grants.status, mode: request.mode, ...(service && { serviceId: service.id }),
      ...(targetedGrant && { grantId: encodeBase64Url(targetedGrant.grantId) }),
    }, service?.current.identity, service?.current.lane)
    // A stronger request in this activation can still use these grants; a new activation cannot.
    if (this.#services !== activation) return
    if (grants.status === 'unconfigured') {
      this.#disabled = true
      return
    }
    if (grants.status === 'retry_later') {
      this.#retry(owner)
      return
    }
    if (service) {
      if (targetedGrant) this.#connectService(service, targetedGrant)
      else this.#retry(service)
      return
    }
    this.#admitNewServices(activation, grants.grants)
    if (grants.status === 'partial_retryable') this.#retry(owner)
    else if (!('id' in owner)) owner.complete = true
  }

  #admitNewServices(activation: Map<string, Service>, grants: readonly TURNGrant[]): void {
    for (const grant of grants) {
      if (this.#services !== activation) return
      if (this.#services.has(grant.serviceId)) continue
      if (!this.#hasRoom()) break
      const service: Service = { id: grant.serviceId, current: { refresh: Infinity }, failures: 0, due: 0 }
      this.#services.set(service.id, service)
      this.#connectService(service, grant)
    }
  }

  #hasRoom(): boolean {
    let occupied = 0
    for (const service of this.#services.values()) {
      if (!this.#exhausted(service)) occupied++
    }
    return occupied < MAX_TURN_PATHS
  }

  #exhausted(service: Service): boolean {
    return service.due === Infinity && !service.work &&
      (!service.current.lane || !this.#available(service.current.lane))
  }

  #connectService(service: Service, grant: TURNGrant): void {
    if (service.work) return
    let finish!: () => void
    // Both starting a peer and withdrawing the previous lane can synchronously end the activation.
    service.work = new Promise<void>(resolve => { finish = resolve })
    void this.#connect(service, grant).catch(() => this.#retry(service)).finally(() => {
      delete service.work
      if (this.#services.get(service.id) === service && this.#exhausted(service)) closeSlot(service.current)
      finish()
    })
  }

  #available(lane: PeerAdmittedLane): boolean {
    return this.#options.session.availableLaneIds().includes(lane.laneId) &&
      this.#options.session.laneEpoch(lane.laneId) === lane.laneEpoch
  }

  async #connect(service: Service, grant: TURNGrant): Promise<void> {
    const old = service.current
    const predecessor = old.lane
    const retirement = predecessor && this.#options.session.retireLaneIfCovered(predecessor)
    const leg: Leg = predecessor && !retirement ? { refresh: Infinity } : old
    if (leg !== old) service.replacement = leg
    else if (predecessor) await this.#releaseLane(service, old, predecessor, retirement)
    if (this.#services.get(service.id) !== service) return

    try {
      const slot = leg.slot ?? this.#options.slots.tryAcquire()
      if (!slot) {
        this.#retry(service)
        return
      }
      leg.slot = slot
      const ordinal = slot.next()
      const identity = this.#identities.get(ordinal.peerSlot) ?? createPeerPathIdentityValue(this.#options.identity())
      this.#identities.set(ordinal.peerSlot, identity)
      leg.identity = identity
      const ttl = Number(grant.validForMs > MAXIMUM_TTL_MS ? MAXIMUM_TTL_MS : grant.validForMs)
      leg.refresh = ttl === 0 ? Infinity : this.#options.now() + ttl - Math.min(REFRESH_MARGIN_MS, ttl / 4)
      leg.handle = this.#options.start(grant, identity, ordinal)
      const result = await leg.handle.result
      if (result.type !== 'admitted') throw new Error('TURN negotiation ended before admission')
      await this.#admit(service, leg, result.lane, encodeBase64Url(grant.grantId))
    } finally {
      delete leg.handle
      delete leg.detachedDuringAdmission
      if (service.replacement === leg) {
        delete service.replacement
        closeSlot(leg)
      }
    }
  }

  #canPublish(service: Service, leg: Leg, lane: PeerAdmittedLane): boolean {
    return this.#services.get(service.id) === service && !sameLane(leg.detachedDuringAdmission, lane) && this.#available(lane)
  }

  async #rejectAdmission(lane: PeerAdmittedLane): Promise<never> {
    await this.#options.release(lane)
    throw new Error('TURN lane withdrawn before admission settled')
  }

  async #admit(service: Service, leg: Leg, lane: PeerAdmittedLane, grantId: string): Promise<void> {
    if (!this.#canPublish(service, leg, lane)) return this.#rejectAdmission(lane)
    const old = service.current
    const replacing = leg !== old
    const predecessor = replacing ? old.lane : undefined
    const retirement = predecessor && this.#options.session.retireLaneIfCovered(predecessor, lane)
    if (predecessor && !retirement) return this.#rejectAdmission(lane)

    const drain = predecessor && this.#releaseLane(service, old, predecessor, retirement)
    try {
      // Claiming retirement can synchronously notify route, content and trace observers.
      if (!this.#canPublish(service, leg, lane)) return await this.#rejectAdmission(lane)
      leg.lane = lane
      leg.grantId = grantId
      // Require authenticated block delivery before clearing retry debt, so a
      // blackhole or flapping TURN path cannot reset backoff on empty handshakes.
      if (service.failures > 0) leg.awaitingDelivery = true
      else delete leg.awaitingDelivery
      if (replacing) {
        service.current = leg
        delete service.replacement
      }
      this.settlingUntil = this.#options.now() + SETTLING_MS
      this.#options.trace?.('turn_path_admitted', { serviceId: service.id, grantId }, leg.identity, lane)
    } finally {
      if (replacing) {
        try { await drain } finally { closeSlot(old) }
      }
    }
  }

  detached(path: PeerPathIdentity, lane: PeerAdmittedLane): void {
    for (const service of this.#services.values()) {
      for (const leg of [service.current, service.replacement]) {
        if (!leg?.identity || !equalDiagnosticIdentities(leg.identity, path)) continue
        if (sameLane(leg.lane, lane)) {
          this.#closeLane(service, leg, lane, 'lane_lost')
          this.#retry(service)
        } else if (leg.handle) {
          leg.detachedDuringAdmission = lane
        }
      }
    }
  }

  #retry(retry: Service | FullRequest): void {
    const service = 'id' in retry ? retry : undefined
    if (service ? this.#services.get(service.id) !== retry : this.#full !== retry) return
    retry.due = this.#options.now() + (BACKOFF_MS[retry.failures++] ?? Infinity)
    if (service) {
      // A failure while the current lane stays live belongs to its refresh or replacement;
      // delivery on the outgoing lane must not forgive that backoff.
      if (service.current.lane) delete service.current.awaitingDelivery
      if (this.#exhausted(service)) {
        // Keep the exhausted service in the activation, so another full grant cannot restart its retry budget.
        closeSlot(service.current)
      }
    }
  }

  #closeLane(service: Service, leg: Leg, lane: PeerAdmittedLane, reason: 'owner_closed' | 'lane_lost'): void {
    delete leg.lane
    delete leg.awaitingDelivery
    if (this.#services.get(service.id) === service) this.settlingUntil = this.#options.now() + SETTLING_MS
    this.#options.trace?.('turn_path_closed', {
      serviceId: service.id, ...(leg.grantId && { grantId: leg.grantId }), reason,
    }, leg.identity, lane)
    delete leg.grantId
  }

  #releaseLane(service: Service, leg: Leg, lane: PeerAdmittedLane, retirement?: Promise<void>): Promise<void> {
    if (sameLane(leg.lane, lane)) this.#closeLane(service, leg, lane, 'owner_closed')
    return this.#options.release(lane, retirement)
  }

  async #release(service: Service, leg: Leg): Promise<void> {
    if (leg.handle) {
      leg.handle.cancel('last-activation')
      await leg.handle.result
    }
    const lane = leg.lane
    if (lane) await this.#releaseLane(service, leg, lane)
  }

  reset(): Promise<void> {
    if (this.#resetting) return this.#resetting
    const services = [...this.#services.values()]
    this.#services = new Map()
    this.#requests.clear()
    this.#unsubscribe?.()
    this.#unsubscribe = undefined
    this.#pressure.reset()
    this.#full = undefined
    this.settlingUntil = 0
    if (!services.length) return Promise.resolve()
    this.#resetting = Promise.resolve().then(async () => {
      await Promise.allSettled(services.map(async service => {
        const legs = service.replacement ? [service.current, service.replacement] : [service.current]
        // An admitted result can still be handing its lane back after cancellation.
        // Its slot belongs to this attempt until both the handoff and lane cleanup finish.
        await Promise.allSettled([service.work, ...legs.map(leg => this.#release(service, leg))])
        for (const leg of legs) closeSlot(leg)
      }))
      this.#resetting = undefined
    })
    return this.#resetting
  }
}
