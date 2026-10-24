import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import type { ReceiverSessionRuntime } from '../../session/runtime'
import type { PeerRecoveryClock, PeerRecoveryState } from '../peer-set/contract'
import { awaitWithAbort } from '../peer-offer-remote'
import { decodeTurnControl, encodeTurnControl, TURN_DEMAND_LIFETIME_MS, TURN_RETIRE, type TurnGrant, type TurnHeader, type TurnLane } from '../../protocol/turn-control'

const RENEW_DEMAND_MS = 60_000
const MAX_PATHS = 3
const MAX_TOMBSTONES = 128
const MEASUREMENT_INTERVAL_MS = 1000
const CONTROL_WRITE_BUDGET_MS = 5000
const MAX_ACTIVE_SERVICES = 2

export interface TurnReceiverOptions {
  readonly session: ReceiverSessionRuntime
  readonly clock: PeerRecoveryClock
  readonly enabled: () => boolean
  readonly open: (grant: TurnGrant) => void
  readonly retire: (pathId: Uint8Array<ArrayBuffer>) => Promise<void>
  readonly observe?: (event: TurnObservation) => void
  readonly profilesChanged?: (profiles: readonly string[]) => void
  readonly measure?: (pathId: Uint8Array<ArrayBuffer>) => TurnMeasurement
  readonly networkGeneration?: () => Uint8Array<ArrayBuffer>
}

export interface TurnMeasurement {
  readonly usefulBytes: number
  readonly unhelpful: boolean
  readonly busyMilliseconds: number
  readonly pendingBytes: number
}
export interface TurnObservation {
  readonly profile: string
  readonly provider?: string
  readonly account?: string
  readonly pathId?: string
  readonly state: 'available' | 'connecting' | 'retrying' | 'ready' | 'retired' | 'unavailable' | 'withdrawn'
  readonly reason?: string
  readonly usefulBytes?: number
}
interface Path {
  readonly grant: TurnGrant
  readonly lifetime: AbortController
  admission?: Admission
  reported: number
  sampleSequence: bigint
  status: Pick<TurnObservation, 'state' | 'reason'>
}

interface Admission {
  readonly lane: TurnLane
  readonly lifetime: AbortController
  readySent: boolean
  accountedBytes: number
}

type Inventory = Extract<ReturnType<typeof decodeTurnControl>, { kind: 'available' }>

/** Delegation outlives relay connections and never owns a download job. */
export class TurnReceiver {
  readonly #options: TurnReceiverOptions
  readonly #sessionId: Uint8Array<ArrayBuffer>
  readonly #lifetime = new AbortController()
  readonly #paths = new Map<string, Path>()
  readonly #retired = new Set<string>()
  readonly #tasks = new Set<Promise<unknown>>()
  readonly #unsubscribe: () => void
  #available: Inventory | undefined
  #demand: AbortController | undefined
  #active = false
  #sequence = 0n
  #activationSequence = 0n

  constructor(options: TurnReceiverOptions) {
    this.#options = options
    this.#sessionId = options.session.protocolSessionIdentity.copyBytes()
    this.#unsubscribe = options.session.subscribeTurnControls((body) => {
      if (this.#lifetime.signal.aborted) return
      const message = decodeTurnControl(body)
      if (!equalBytes(message.sessionId, this.#sessionId)) return
      if (message.kind === 'available') {
        this.#replaceInventory(message)
        return
      }
      if (this.#available === undefined || !equalBytes(message.ownerPathId, this.#available.ownerPathId)) return
      if (message.kind === 'grant') this.#grant(message)
      if (message.kind === 'retire' && this.grantFor(message.pathId)?.sequence === message.sequence) {
        this.#retire(message.pathId, Object.entries(TURN_RETIRE).find(([, code]) => code === message.reason)![0])
      }
      if (message.kind === 'unavailable' && this.#available.profiles.includes(message.profile)) {
        this.#emit({ profile: message.profile, state: 'unavailable', reason: message.reason })
      }
    })
  }

  setDemand(active: boolean): void {
    if (this.#lifetime.signal.aborted || this.#active === active) return
    this.#active = active
    if (active) this.#startDemand()
    else {
      this.#demand?.abort()
      this.#demand = undefined
      for (const path of this.#paths.values()) this.#retire(path.grant.pathId)
      if (this.#available !== undefined && this.#sequence !== 0n) this.#track(this.#sendDemand(0))
    }
  }

  grantFor(pathId: Uint8Array): TurnGrant | undefined { return this.#paths.get(encodeBase64Url(pathId))?.grant }

  admitted(pathId: Uint8Array, lane: TurnLane): void {
    const path = this.#paths.get(encodeBase64Url(pathId))
    if (path === undefined || lane.laneEpoch <= (path.admission?.lane.laneEpoch ?? 0)) return
    path.admission?.lifetime.abort()
    const admission: Admission = { lane: { ...lane }, lifetime: new AbortController(), readySent: false, accountedBytes: 0 }
    path.admission = admission
    this.#observe(path.grant, 'ready')
    // Readiness identifies this exact lane incarnation. The sender retains the
    // predecessor until matching local admission is still live at promotion.
    this.#track(this.#notifyReady(path, admission))
    this.#track(this.#measure(path, admission))
  }

  detached(pathId: Uint8Array, lane: TurnLane): void {
    const admission = this.#paths.get(encodeBase64Url(pathId))?.admission
    if (admission?.lane.laneId === lane.laneId && admission.lane.laneEpoch === lane.laneEpoch) {
      admission.lifetime.abort()
    }
  }

  recoveryChanged(pathId: Uint8Array, state: PeerRecoveryState): void {
    const path = this.#paths.get(encodeBase64Url(pathId))
    if (path === undefined) return
    switch (state.kind) {
      case 'attempting':
        this.#observe(path.grant, 'connecting')
        break
      case 'waiting-retry':
        this.#observe(path.grant, 'retrying', state.reason)
        break
      case 'admitted':
        this.#observe(path.grant, 'ready')
        break
      case 'quiescent':
      case 'session-exhausted':
      case 'path-stopped':
      case 'session-stopped':
        this.#observe(path.grant, 'unavailable', state.reason)
        break
    }
  }

  unhelpful(pathId: Uint8Array, reason?: string): void {
    const path = this.#paths.get(encodeBase64Url(pathId))
    if (path === undefined) return
    this.#retire(path.grant.pathId, reason ?? path.status.reason ?? 'unhelpful')
    this.#track(this.#send({ ...this.#header(path.grant), kind: 'retire', pathId: path.grant.pathId, reason: TURN_RETIRE.unhelpful }))
  }

  async close(): Promise<void> {
    if (!this.#lifetime.signal.aborted) {
      this.setDemand(false)
      this.#unsubscribe()
      this.#lifetime.abort()
    }
    await Promise.allSettled([...this.#tasks])
  }

  #replaceInventory(message: Inventory): void {
    // Advertisement revisions replace inventory without resetting demand
    // authority. Replays cannot restore services removed by newer settings.
    const previous = this.#available
    if (previous !== undefined && (!equalBytes(message.ownerPathId, previous.ownerPathId) ||
      message.sequence <= previous.sequence)) return
    this.#available = message
    for (const path of this.#paths.values()) {
      if (!message.profiles.includes(path.grant.profile)) this.#retire(path.grant.pathId, 'revoked')
    }
    this.#options.profilesChanged?.(message.profiles)
    for (const profile of previous?.profiles ?? []) {
      if (!message.profiles.includes(profile)) this.#emit({ profile, state: 'withdrawn' })
    }
    for (const profile of message.profiles) {
      if (!previous?.profiles.includes(profile)) this.#emit({ profile, state: 'available' })
    }
    this.#startDemand()
  }

  #startDemand(): void {
    if (!this.#active || this.#demand !== undefined || this.#available === undefined || !this.#options.enabled()) return
    const controller = new AbortController()
    this.#demand = controller
    this.#activationSequence = this.#sequence + 1n
    this.#track((async () => {
      while (!controller.signal.aborted) {
        await this.#sendDemand(TURN_DEMAND_LIFETIME_MS, controller.signal).catch(() => undefined)
        await this.#options.clock.sleep(RENEW_DEMAND_MS, controller.signal)
      }
    })())
  }

  #grant(grant: TurnGrant): void {
    if (!this.#active || !this.#options.enabled() || grant.sequence < this.#activationSequence ||
      grant.sequence > this.#sequence || !this.#available?.profiles.includes(grant.profile)) return
    const key = encodeBase64Url(grant.pathId)
    if (this.#paths.has(key) || this.#retired.has(key)) return
    const replaces = this.#paths.get(encodeBase64Url(grant.replaces))
    const sameService = [...this.#paths.values()].filter((path) => path.grant.profile === grant.profile)
    const services = new Set([...this.#paths.values()].map((path) => path.grant.profile))
    if (!services.has(grant.profile) && services.size >= MAX_ACTIVE_SERVICES) return
    if (this.#paths.size >= MAX_PATHS || sameService.length > 1 ||
      (sameService.length === 1 && replaces !== sameService[0]) ||
      (replaces !== undefined && (replaces.grant.profile !== grant.profile || replaces.grant.account !== grant.account || replaces.grant.provider !== grant.provider)) ||
      (grant.replaces.some((value) => value !== 0) && replaces === undefined)) return
    const lifetime = new AbortController()
    this.#paths.set(key, { grant, lifetime, reported: 0, sampleSequence: 0n,
      status: { state: 'connecting' } })
    // Activation can synchronously admit or retire a path when its service
    // budget is exhausted. Never overwrite that result with "connecting".
    this.#observe(grant, 'connecting')
    try {
      this.#options.open(grant)
    } catch (error) {
      this.unhelpful(grant.pathId, String(error))
      return
    }
    if (lifetime.signal.aborted) return
    if (grant.credentials.kind === 'expiring') {
      this.#track(this.#options.clock.sleep(grant.credentials.validForMs, lifetime.signal)
        .then(() => this.#retire(grant.pathId, 'expired')))
    }
  }

  #retire(pathId: Uint8Array<ArrayBuffer>, reason = 'idle'): void {
    const key = encodeBase64Url(pathId)
    const path = this.#paths.get(key)
    if (path === undefined) return
    const final = this.#options.measure?.(path.grant.pathId)
    const delta = final === undefined ? 0 : Math.max(0, final.usefulBytes - (path.admission?.accountedBytes ?? 0))
    if (delta > 0) {
      this.#track(this.#send({ ...this.#header(path.grant), kind: 'report', pathId: path.grant.pathId,
        usefulBytes: BigInt(path.reported + delta) }))
    }
    this.#paths.delete(key)
    this.#retired.add(key)
    if (this.#retired.size > MAX_TOMBSTONES) this.#retired.delete(this.#retired.values().next().value!)
    path.lifetime.abort()
    this.#observe(path.grant, 'retired', reason)
    this.#track(this.#options.retire(path.grant.pathId))
  }

  #sendDemand(holdForMs: number, signal = this.#lifetime.signal): Promise<void> {
    const available = this.#available
    if (available === undefined) return Promise.resolve()
    return this.#send({ ...this.#header(available), sequence: ++this.#sequence, kind: 'demand', holdForMs }, signal)
  }
  async #send(message: Parameters<typeof encodeTurnControl>[0], signal = this.#lifetime.signal): Promise<void> {
    const lifetime = new AbortController()
    const bounded = AbortSignal.any([signal, lifetime.signal])
    this.#options.clock.sleep(CONTROL_WRITE_BUDGET_MS, lifetime.signal)
      .then(() => lifetime.abort(new DOMException('TURN control deadline exceeded', 'TimeoutError')))
      .catch(() => undefined)
    try {
      await awaitWithAbort(this.#options.session.sendTurnControl(encodeTurnControl(message), { signal: bounded }), bounded)
    } finally { lifetime.abort() }
  }
  #header(value: TurnHeader): TurnHeader {
    return { sessionId: value.sessionId, ownerPathId: value.ownerPathId, sequence: value.sequence }
  }
  #track(task: Promise<unknown>): void {
    const settled = task.catch(() => undefined).finally(() => this.#tasks.delete(settled))
    this.#tasks.add(settled)
  }
  #observe(grant: TurnGrant, state: TurnObservation['state'], reason?: string): void {
    const path = this.#paths.get(encodeBase64Url(grant.pathId))
    const status = { state, ...(reason === undefined ? {} : { reason }) }
    if (path !== undefined) path.status = status
    this.#emit({ profile: grant.profile, provider: grant.provider, account: grant.account,
      pathId: encodeBase64Url(grant.pathId), ...status })
  }
  #emit(event: TurnObservation): void {
    try { this.#options.observe?.(event) } catch { /* Observations cannot own the path lifetime. */ }
  }
  async #notifyReady(path: Path, admission: Admission): Promise<void> {
    if (admission.readySent || admission.lifetime.signal.aborted || path.admission !== admission) return
    const signal = AbortSignal.any([path.lifetime.signal, admission.lifetime.signal])
    await this.#send({ ...this.#header(path.grant), ...admission.lane, kind: 'ready', pathId: path.grant.pathId }, signal)
    if (!signal.aborted) admission.readySent = true
  }
  async #measure(path: Path, admission: Admission): Promise<void> {
    const signal = AbortSignal.any([path.lifetime.signal, admission.lifetime.signal])
    while (!signal.aborted) {
      await this.#options.clock.sleep(MEASUREMENT_INTERVAL_MS, signal)
      await this.#notifyReady(path, admission).catch(() => undefined)
      if (signal.aborted) return
      const measurement = this.#options.measure?.(path.grant.pathId)
      if (measurement === undefined) continue
      await this.#reportContent(path, admission, measurement, signal)
      if (signal.aborted) return
      await this.#reportQuality(path, admission, measurement, signal)
      if (signal.aborted) return
      if (measurement.unhelpful) this.unhelpful(path.grant.pathId)
    }
  }

  async #reportContent(path: Path, admission: Admission, measurement: TurnMeasurement, signal: AbortSignal): Promise<void> {
    if (measurement.usefulBytes <= admission.accountedBytes) return
    const total = path.reported + measurement.usefulBytes - admission.accountedBytes
    const sent = await this.#send({ ...this.#header(path.grant), kind: 'report', pathId: path.grant.pathId,
      usefulBytes: BigInt(total) }, signal).then(() => true, () => false)
    if (signal.aborted) return
    if (sent) { path.reported = total; admission.accountedBytes = measurement.usefulBytes }
    this.#emit({ profile: path.grant.profile, provider: path.grant.provider, account: path.grant.account,
      pathId: encodeBase64Url(path.grant.pathId), ...path.status, usefulBytes: total })
  }

  async #reportQuality(path: Path, admission: Admission, measurement: TurnMeasurement, signal: AbortSignal): Promise<void> {
    if (measurement.busyMilliseconds <= 0) return
    await this.#send({ ...this.#header(path.grant), ...admission.lane, kind: 'quality', pathId: path.grant.pathId,
      networkGeneration: this.#options.networkGeneration?.() ?? new Uint8Array(16), sampleSequence: ++path.sampleSequence,
      contentBytes: BigInt(measurement.usefulBytes), busyMilliseconds: BigInt(Math.floor(measurement.busyMilliseconds)),
      pendingBytes: BigInt(measurement.pendingBytes) }, signal).catch(() => undefined)
  }
}
