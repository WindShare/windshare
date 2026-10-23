export const INITIAL_BLOCK_MILLISECONDS = 250
export const MINIMUM_SAMPLE_MILLISECONDS = 1
const MAXIMUM_ESTIMATE_MILLISECONDS = 60 * 60 * 1000
const RELAY_COST_FACTOR = 1.1
const USER_RELAY_COST_FACTOR = 1.05

/** Evidence only; admission owns capacity, route permission, and transport lifetime. */
export interface CompletionLoad {
  readonly pendingBytes: number
  readonly pendingBlocks: number
  readonly bytesPerSecond: number
}

export function estimateQueueMilliseconds(load: CompletionLoad): number {
  // A new request's size cannot establish the speed of existing reservations.
  const milliseconds = load.bytesPerSecond > 0
    ? load.pendingBytes * 1000 / load.bytesPerSecond
    : load.pendingBlocks * INITIAL_BLOCK_MILLISECONDS
  return boundedEstimate(milliseconds, 0)
}

export function estimateCompletionMilliseconds(load: CompletionLoad, bytes: number): number {
  const service = load.bytesPerSecond > 0
    ? Math.max(bytes, 1) * 1000 / load.bytesPerSecond
    : INITIAL_BLOCK_MILLISECONDS
  return boundedEstimate(estimateQueueMilliseconds(load) + service, MINIMUM_SAMPLE_MILLISECONDS)
}

function boundedEstimate(milliseconds: number, minimum: number): number {
  return Math.min(MAXIMUM_ESTIMATE_MILLISECONDS, Math.max(minimum, milliseconds))
}

export function completionCost(milliseconds: number, route: 'direct' | 'turn' | 'application-relay'): number {
  if (route === 'direct') return milliseconds
  return milliseconds * (route === 'turn' ? USER_RELAY_COST_FACTOR : RELAY_COST_FACTOR)
}
