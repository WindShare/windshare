export const INITIAL_BLOCK_MILLISECONDS = 250
export const MINIMUM_SAMPLE_MILLISECONDS = 1
export const EVIDENCE_LIFETIME_MILLISECONDS = 1000
const MAXIMUM_ESTIMATE_MILLISECONDS = 60 * 60 * 1000
const RELAY_COST_FACTOR = 1.1
const USER_RELAY_COST_FACTOR = 1.05
const STALE_EVIDENCE_COST_FACTOR = 2

export type DeliverySample =
  | { readonly kind: 'unknown' }
  | { readonly kind: 'recent' | 'stale'; readonly bytesPerSecond: number; readonly startupMilliseconds: number }

export interface DeliveryLoad {
  readonly remainingBytes: number
  readonly pendingResponses: number
  readonly sample: DeliverySample
}

export interface DeliveryEstimate {
  readonly evidence: DeliverySample['kind']
  readonly milliseconds: number
}

export interface DeliveryProgress {
  readonly remainingBytes: number
  readonly elapsedMilliseconds: number
  readonly silentMilliseconds: number
  readonly remaining: DeliveryEstimate
}

/** A bootstrap timeout is not measured capacity and must never win against recent evidence by its numeric value. */
export function estimateQueueMilliseconds(load: DeliveryLoad): number {
  if (load.remainingBytes === 0) return 0
  return boundedEstimate(load.sample.kind === 'unknown'
    ? load.pendingResponses * INITIAL_BLOCK_MILLISECONDS
    : load.remainingBytes * 1000 / load.sample.bytesPerSecond, 0)
}

export function estimateCompletion(load: DeliveryLoad, bytes: number): DeliveryEstimate {
  const queue = estimateQueueMilliseconds(load)
  // Response latency overlaps queued transmission; charging it for every
  // outstanding block would suppress useful high-latency, high-bandwidth lanes.
  const milliseconds = load.sample.kind === 'unknown'
    ? queue + INITIAL_BLOCK_MILLISECONDS
    : Math.max(queue, load.sample.startupMilliseconds) + Math.max(bytes, 1) * 1000 / load.sample.bytesPerSecond
  return { evidence: load.sample.kind, milliseconds: boundedEstimate(milliseconds, MINIMUM_SAMPLE_MILLISECONDS) }
}

function boundedEstimate(milliseconds: number, minimum: number): number {
  return Math.min(MAXIMUM_ESTIMATE_MILLISECONDS, Math.max(minimum, milliseconds))
}

export function completionCost(milliseconds: number, route: 'direct' | 'turn' | 'application-relay'): number {
  if (route === 'direct') return milliseconds
  return milliseconds * (route === 'turn' ? USER_RELAY_COST_FACTOR : RELAY_COST_FACTOR)
}

/** Old observations can justify one bounded revalidation, never a fresh pipeline. */
export function conservativeDeliveryMilliseconds(estimate: DeliveryEstimate): number {
  return boundedEstimate(estimate.milliseconds * (estimate.evidence === 'stale' ? STALE_EVIDENCE_COST_FACTOR : 1), MINIMUM_SAMPLE_MILLISECONDS)
}

/** Only spare capacity predicted to finish before consumption may take read-ahead. */
export function fitsReadAhead(estimate: DeliveryEstimate, bytes: number, distance: bigint, aggregateRate: number): boolean {
  return distance > 0n && estimate.evidence === 'recent' && aggregateRate > 0 &&
    estimate.milliseconds <= Number(distance) * bytes * 1000 / aggregateRate
}

export function compareDelivery(left: DeliveryEstimate, right: DeliveryEstimate): number {
  return Number(left.evidence === 'unknown') - Number(right.evidence === 'unknown')
}
