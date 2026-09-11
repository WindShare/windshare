import { BROWSER_DELIVERY_TRANSITIONS } from '../trace/browser-delivery-payload'
import { canonicalIdentity, decimalUint64, exactKeys, member, type UnknownRecord } from './trace-payload-validation'

const MAXIMUM_DELIVERY_REASON_LENGTH = 256

export function validateBrowserDelivery(payload: UnknownRecord): void {
  exactKeys(payload, ['operation_id', 'file_id', 'transition'], [
    'placement', 'placement_reason', 'received_bytes', 'recoverable_bytes', 'copy_milliseconds', 'failure_name',
    'object_id', 'checkpoint_stage', 'pending_bytes', 'at_milliseconds', 'last_checkpoint_milliseconds', 'checkpoint_milliseconds',
  ], 'browser_delivery payload')
  canonicalIdentity(payload.operation_id, 'browser delivery operation ID')
  canonicalIdentity(payload.file_id, 'browser delivery file ID')
  member(payload.transition, BROWSER_DELIVERY_TRANSITIONS, 'browser delivery transition')
  if (payload.placement !== undefined) member(payload.placement, ['direct', 'staged'], 'browser delivery placement')
  for (const key of ['received_bytes', 'recoverable_bytes', 'pending_bytes']) {
    if (payload[key] !== undefined) decimalUint64(payload[key], `browser delivery ${key}`)
  }
  for (const key of ['copy_milliseconds', 'at_milliseconds', 'last_checkpoint_milliseconds', 'checkpoint_milliseconds']) {
    if (payload[key] !== undefined && (typeof payload[key] !== 'number' || !Number.isFinite(payload[key]) || payload[key] < 0)) {
      throw new TypeError('Browser delivery time must be finite and non-negative')
    }
  }
  if (payload.checkpoint_stage !== undefined) member(payload.checkpoint_stage,
    ['started', 'advanced', 'deferred', 'finished', 'failed'], 'browser delivery checkpoint stage')
  for (const key of ['placement_reason', 'failure_name', 'object_id']) {
    if (payload[key] !== undefined && (typeof payload[key] !== 'string' ||
        payload[key].length === 0 || payload[key].length > MAXIMUM_DELIVERY_REASON_LENGTH)) {
      throw new TypeError('Browser delivery diagnostic text exceeds its bound')
    }
  }
}
