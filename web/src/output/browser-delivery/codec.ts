import { encodeBase64Url } from '../../crypto/bytes'
import { checkpointSha256Sync } from '../persistence/checkpoint-codec'
import type { DurableCheckpointNamespaceIdentity } from '../persistence/namespace'

const ENCODER = new TextEncoder()
export const BROWSER_DELIVERY_MAX_REASON_LENGTH = 1024

export function deliveryDigest(domain: string, fields: readonly unknown[]): string {
  return encodeBase64Url(checkpointSha256Sync(ENCODER.encode(JSON.stringify([domain, ...fields]))))
}

export function namespaceFields(namespace: DurableCheckpointNamespaceIdentity): readonly unknown[] {
  return [namespace.operationId, namespace.receiveIntentDigest, namespace.materializationBindingDigest,
    namespace.materializerKind, namespace.authorityRef]
}

export function deliveryReason(value: string): string {
  if (typeof value !== 'string' || value.length === 0 || value.length > BROWSER_DELIVERY_MAX_REASON_LENGTH) {
    throw new TypeError('Browser delivery reason must be bounded nonempty text')
  }
  return value
}
