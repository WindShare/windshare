import type { V2BlockTransportRoute } from '../content/v2-route-policy'

export const MAXIMUM_DOWNLOAD_INTERVALS = 4_096
import type { DownloadConnectivitySnapshot } from '../diagnostics/trace/transfer-payload'
export type { DownloadConnectivitySnapshot } from '../diagnostics/trace/transfer-payload'
type Interval = { start: bigint; end: bigint }

function positiveOverlap(start: bigint, end: bigint): bigint { return end > start ? end - start : 0n }

function unionIntervals(old: readonly Interval[], start: bigint, end: bigint): {
  unique: bigint; next: Interval[]
} {
  let unique = end - start
  const merged = { start, end }
  const next: Interval[] = []
  for (const part of old) {
    const overlapStart = start > part.start ? start : part.start
    const overlapEnd = end < part.end ? end : part.end
    unique -= positiveOverlap(overlapStart, overlapEnd)
    if (part.end < merged.start || part.start > merged.end) next.push(part)
    else {
      if (part.start < merged.start) merged.start = part.start
      if (part.end > merged.end) merged.end = part.end
    }
  }
  next.push(merged)
  return { unique, next }
}

/** One content activation owns this ledger across protocol generations. */
export class DownloadMetrics {
  readonly #now: () => number
  readonly #id: string
  readonly #started: number
  readonly #ranges = new Map<string, Interval[]>()
  readonly #bytes = { direct: 0n, turn: 0n, 'application-relay': 0n, unknown: 0n }
  #last: number
  #first: number | null
  #direct: boolean
  #fallback = false
  #pending = 0
  #stall = 0
  #intervals = 0
  #incomplete = false
  #final = false

  constructor(id: string, directUsable: boolean, now: () => number = () => performance.now()) {
    this.#id = id
    this.#now = now
    this.#started = this.#last = now()
    this.#direct = directUsable
    this.#first = directUsable ? 0 : null
  }
  availability(direct: boolean): void {
    if (this.#final) return
    this.#tick()
    if (this.#direct && !direct) this.#fallback = true
    this.#direct = direct
    if (direct) {
      this.#fallback = false
      this.#first ??= this.#last - this.#started
    }
  }
  pending(): () => void {
    if (this.#final) return () => undefined
    this.#tick()
    this.#pending += 1
    let released = false
    return () => {
      if (released || this.#final) return
      released = true
      this.#tick()
      this.#pending -= 1
    }
  }
  evidenceLost(): void { if (!this.#final) this.#incomplete = true }
  delivered(revision: string, start: bigint, end: bigint, route?: V2BlockTransportRoute): void {
    if (this.#final) return
    this.#tick()
    if (revision === '' || start < 0n || end <= start) { this.evidenceLost(); return }
    if (route !== undefined) this.#fallback = false
    const old = this.#ranges.get(revision) ?? []
    const { unique, next } = unionIntervals(old, start, end)
    const count = this.#intervals - old.length + next.length
    if (count > MAXIMUM_DOWNLOAD_INTERVALS) { this.evidenceLost(); return }
    this.#intervals = count
    this.#ranges.set(revision, next)
    this.#bytes[route ?? 'unknown'] += unique
    if (route === undefined && unique > 0n) this.evidenceLost()
  }
  snapshot(final = false): DownloadConnectivitySnapshot {
    if (!this.#final) this.#tick()
    if (final) { this.#final = true; this.#ranges.clear(); this.#intervals = 0 }
    const total = Object.values(this.#bytes).reduce((sum, value) => sum + value, 0n)
    return Object.freeze({
      download_id: this.#id, first_direct_elapsed_ms: this.#first,
      direct_bytes: this.#bytes.direct.toString(), turn_bytes: this.#bytes.turn.toString(),
      application_relay_bytes: this.#bytes['application-relay'].toString(), unknown_bytes: this.#bytes.unknown.toString(),
      direct_fraction: total === 0n || this.#incomplete ? null : Number(this.#bytes.direct) / Number(total),
      fallback_stall_ms: this.#stall, incomplete: this.#incomplete, final: this.#final,
    })
  }
  #tick(): void {
    const now = this.#now()
    if (now < this.#last) { this.#incomplete = true; return }
    if (this.#pending > 0 && this.#fallback && !this.#direct) this.#stall += now - this.#last
    this.#last = now
  }
}
