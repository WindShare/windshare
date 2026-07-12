import type { DeliveryOrder } from '../contracts/sink'
import {
  DEFAULT_BLOCK_ATTEMPTS,
  DEFAULT_REQUEST_TIMEOUT_MS,
  type ReceiveSessionOptions,
} from './model'

const MAX_CHANNEL_POLL_MS = 250
const MIN_CHANNEL_POLL_MS = 1
const MAX_TIMER_DELAY_MS = 2_147_483_647

export interface NormalizedReceiveOptions {
  readonly deliveryOrder: DeliveryOrder
  readonly maxBlockBytes: number
  readonly requestTimeoutMs: number
  readonly maxBlockAttempts: number
  readonly pollIntervalMs: number
  readonly now: () => number
}

function positiveInteger(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${label} must be a positive safe integer`)
  }
  return value
}

export function normalizeReceiveOptions(
  options: ReceiveSessionOptions,
  deliveryOrder: unknown,
): NormalizedReceiveOptions {
  if (deliveryOrder !== 'any' && deliveryOrder !== 'ascending') {
    throw new TypeError('sink delivery order must be any or ascending')
  }
  const requestTimeoutMs = positiveInteger(
    options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS,
    'request timeout',
  )
  if (requestTimeoutMs > MAX_TIMER_DELAY_MS) {
    throw new RangeError(`request timeout must not exceed ${MAX_TIMER_DELAY_MS}`)
  }
  return {
    deliveryOrder,
    maxBlockBytes: positiveInteger(options.maxBlockBytes, 'maximum block bytes'),
    requestTimeoutMs,
    maxBlockAttempts: positiveInteger(
      options.maxBlockAttempts ?? DEFAULT_BLOCK_ATTEMPTS,
      'maximum block attempts',
    ),
    pollIntervalMs: Math.min(
      Math.max(requestTimeoutMs / 4, MIN_CHANNEL_POLL_MS),
      MAX_CHANNEL_POLL_MS,
    ),
    now: options.now ?? (() => performance.now()),
  }
}
