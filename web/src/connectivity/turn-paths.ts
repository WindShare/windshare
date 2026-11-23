import type { TURNTraceStage } from './diagnostics'
import type { LaneSet } from '../content/lane-set'
import { encodeBase64Url } from '../crypto/bytes'
import {
  decodeTURNGrants,
  encodeTURNDemand,
  MAX_TURN_PATHS,
  type TURNGrant,
  type TURNMode,
} from '../protocol/turn-control'
import { createPeerPathIdentityValue, type PeerPathIdentity } from '../session/identities'
import { PeerSlotPool, type PeerSlotLease, type PeerAttemptOrdinal } from '../session/peer-slots/slots'
import type { ReceiverSessionRuntime } from '../session/runtime'
import type { PeerAttemptHandle } from './peer-set/contract'
import type { PeerAdmittedLane } from './peer-failure'

const BACKOFF = [2000, 10_000, 30_000] as const
const RESPONSE_MS = 8000
const LARGE_TRANSFER = 4 * 1024 * 1024
const SLOW_RATE = 1024 * 1024
type Demand = ReturnType<LaneSet['schedulingDemand']>
type Request = { mode: TURNMode; service?: string; until: number }
type Leg = {
  slot?: PeerSlotLease
  identity?: PeerPathIdentity
  handle?: PeerAttemptHandle
  lane?: PeerAdmittedLane
  refresh: number
}
type Service = { current: Leg; replacement?: Leg; busy: boolean; failures: number; due: number }
interface Options {
  session: Pick<ReceiverSessionRuntime, 'subscribeTURNControls' | 'sendTURNControl' | 'availableLaneIds'>
  slots: PeerSlotPool
  now(): number
  identity(): Uint8Array
  start(grant: TURNGrant, identity: PeerPathIdentity, ordinal: PeerAttemptOrdinal): PeerAttemptHandle
  release(lane: PeerAdmittedLane): Promise<void>
  trace?(
    stage: TURNTraceStage,
    detail: { serviceId?: string; grantId?: string; status?: string; mode?: string },
    path?: PeerPathIdentity,
    lane?: PeerAdmittedLane,
  ): void
}

export class TURNPaths {
  readonly #options: Options
  readonly #services = new Map<string, Service>()
  readonly #identities = new Map<number, PeerPathIdentity>()
  readonly #requests = new Map<bigint, Request>()
  readonly #work = new Set<Promise<void>>()
  #unsubscribe: (() => void) | undefined
  #next = 0n
  #asked: TURNMode | undefined
  #disabled = false
  #failures = 0
  #due = 0
  #pressure: { start: number; last: number; useful: number; progress: number; slow: number } | undefined
  settlingUntil = 0

  constructor(options: Options) {
    this.#options = options
  }

  owns(path: PeerPathIdentity): boolean {
    const key = encodeBase64Url(path.copyBytes())
    return [...this.#services.values()].some((s) =>
      [s.current, s.replacement].some(
        (l) => l?.identity !== undefined && encodeBase64Url(l.identity.copyBytes()) === key,
      ),
    )
  }

  step(demand: Demand): void {
    if (this.#disabled) return
    const now = this.#options.now()
    for (const [id, request] of this.#requests)
      if (now >= request.until) {
        this.#requests.delete(id)
        this.#retry(request.service)
      }
    const mode = this.#mode(now, demand)
    if (mode && this.#asked !== 'all' && mode !== this.#asked && now >= this.#due && this.#failures <= BACKOFF.length) {
      this.#request(mode)
    }
    for (const [id, s] of this.#services) {
      if (
        s.busy ||
        (demand.unfinishedBytes === 0 && demand.networkWaitingBytes === 0) ||
        now < s.due ||
        s.failures > BACKOFF.length
      )
        continue
      if (!s.current.lane || now >= s.current.refresh) this.#request('all', id)
    }
  }

  #mode(now: number, d: Demand): TURNMode | undefined {
    const windowFull =
      d.queuedBytes === 0 && d.inFlightBytes > 0 && d.remainingBytes > d.inFlightBytes && d.capacityWaitingBytes === 0
    if (!windowFull && d.networkWaitingBytes === 0) {
      this.#pressure = undefined
      return
    }
    const p = (this.#pressure ??= { start: now, last: now, useful: d.usefulBytes, progress: now, slow: now })
    const delta = d.usefulBytes - p.useful
    if (delta > 0) p.progress = now
    if (now > p.last && delta * 1000 >= SLOW_RATE * (now - p.last)) p.slow = now
    p.last = now
    p.useful = d.usefulBytes
    if (d.remainingBytes <= LARGE_TRANSFER && now - p.start <= 2000) return
    return now - p.progress >= 3000 || now - p.slow >= 5000 ? 'all' : 'parallel'
  }

  #request(mode: TURNMode, service?: string): void {
    if ([...this.#requests.values()].some((r) => r.service === service)) return
    this.#unsubscribe ??= this.#options.session.subscribeTURNControls((body) => this.#accept(body))
    const id = ++this.#next
    this.#requests.set(id, {
      mode,
      ...(service === undefined ? {} : { service }),
      until: this.#options.now() + RESPONSE_MS,
    })
    this.#options.trace?.(service === undefined ? 'turn_demand_sent' : 'turn_refresh_requested', {
      mode,
      ...(service ? { serviceId: service } : {}),
    })
    void this.#options.session
      .sendTURNControl(encodeTURNDemand({ requestId: id, mode, serviceIds: service ? [service] : [] }))
      .catch(() => {
        if (this.#requests.delete(id)) this.#retry(service)
      })
  }

  #accept(body: Uint8Array): void {
    const g = decodeTURNGrants(body)
    const r = this.#requests.get(g.requestId)
    if (!r) return
    this.#requests.delete(g.requestId)
    this.#options.trace?.('turn_grants_received', {
      status: g.status,
      mode: r.mode,
      ...(r.service ? { serviceId: r.service } : {}),
    })
    if (g.status === 'unconfigured') {
      this.#disabled = true
      return
    }
    if (g.status === 'retry_later') {
      this.#retry(r.service)
      return
    }
    if (!r.service) {
      this.#asked = r.mode
      this.#failures = 0
    } else if (g.grants.length === 0) this.#retry(r.service)
    for (const grant of g.grants) this.#acceptService(grant, r)
  }

  #acceptService(grant: TURNGrant, r: Request): void {
    const id = grant.serviceId
    let s = this.#services.get(id)
    if (r.service && r.service !== id) return
    if (!s) {
      if (r.service || this.#services.size >= MAX_TURN_PATHS) return
      s = { current: { refresh: Infinity }, busy: false, failures: 0, due: 0 }
      this.#services.set(id, s)
    } else if (!r.service || s.busy) return
    s.busy = true
    this.#track(
      this.#connect(s, grant)
        .catch(() => this.#retry(id))
        .finally(() => {
          s.busy = false
        }),
    )
  }

  async #connect(service: Service, grant: TURNGrant): Promise<void> {
    const id = grant.serviceId
    let leg = service.current
    const replacement = leg.lane !== undefined && this.#options.session.availableLaneIds().length === 1
    if (replacement) {
      leg = { refresh: Infinity }
      service.replacement = leg
    } else if (leg.lane) {
      await this.#release(leg)
    }
    if (this.#services.get(id) !== service) return
    try {
      leg.slot ??= this.#options.slots.acquire()
      const ordinal = leg.slot.next()
      let identity = this.#identities.get(ordinal.peerSlot)
      if (!identity) {
        identity = createPeerPathIdentityValue(this.#options.identity())
        this.#identities.set(ordinal.peerSlot, identity)
      }
      leg.identity = identity
      leg.refresh =
        grant.validForMs === 0n
          ? Infinity
          : this.#options.now() + Number(grant.validForMs) - Math.min(30_000, Number(grant.validForMs) / 4)
      const handle = this.#options.start(grant, identity, ordinal)
      leg.handle = handle
      const result = await handle.result
      if (result.type !== 'admitted') throw new Error('TURN negotiation ended before admission')
      leg.lane = result.lane
      if (this.#services.get(id) !== service) {
        await this.#release(leg)
        return
      }
      if (replacement) {
        const old = service.current
        service.current = leg
        delete service.replacement
        await this.#release(old)
        old.slot?.close()
      }
      service.failures = 0
      this.settlingUntil = this.#options.now() + 2000
      this.#options.trace?.(
        'turn_path_admitted',
        { serviceId: id, grantId: encodeBase64Url(grant.grantId) },
        identity,
        result.lane,
      )
    } finally {
      delete leg.handle
      if (service.replacement === leg) {
        delete service.replacement
        leg.slot?.close()
      }
    }
  }

  detached(laneId: number, laneEpoch: number): void {
    for (const [id, s] of this.#services)
      if (s.current.lane?.laneId === laneId && s.current.lane.laneEpoch === laneEpoch) {
        delete s.current.lane
        this.settlingUntil = this.#options.now() + 2000
        this.#options.trace?.('turn_path_closed', { serviceId: id }, s.current.identity)
        this.#retry(id)
      }
  }
  #retry(id?: string): void {
    if (id !== undefined && !this.#services.has(id)) return
    const s = id === undefined ? undefined : this.#services.get(id)
    const failures = s ? s.failures++ : this.#failures++
    const due = this.#options.now() + (BACKOFF[failures] ?? Infinity)
    if (s) s.due = due
    else this.#due = due
  }
  async #release(leg: Leg): Promise<void> {
    leg.handle?.cancel('last-activation')
    if (leg.handle) await leg.handle.result
    const lane = leg.lane
    delete leg.lane
    if (lane) await this.#options.release(lane)
  }
  #track(task: Promise<void>): void {
    this.#work.add(task)
    void task.finally(() => this.#work.delete(task))
  }
  async reset(): Promise<void> {
    const services = [...this.#services.values()]
    this.#services.clear()
    this.#requests.clear()
    this.#unsubscribe?.()
    this.#unsubscribe = undefined
    this.#pressure = undefined
    this.#asked = undefined
    this.#failures = this.#due = 0
    const draining = [...this.#work]
    await Promise.allSettled(
      services.flatMap((s) =>
        [s.current, s.replacement].map(async (leg) => {
          if (leg) {
            await this.#release(leg)
            leg.slot?.close()
          }
        }),
      ),
    )
    await Promise.allSettled(draining)
  }
}
