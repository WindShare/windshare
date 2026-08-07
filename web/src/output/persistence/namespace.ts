import { decodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
} from './checkpoint'

export interface DurableCheckpointNamespaceIdentity {
  readonly backend: string
  readonly transferIntentDigest: string
  readonly rootIdentity: string
}

export function durableCheckpointNamespaceIdentity(
  input: DurableCheckpointNamespaceIdentity,
): DurableCheckpointNamespaceIdentity {
  const backend = requireNamespacePart(input.backend, 'backend')
  return Object.freeze({
    backend,
    transferIntentDigest: requireDigest(input.transferIntentDigest, 'transfer intent digest'),
    rootIdentity: requireDigest(input.rootIdentity, 'root identity'),
  })
}

/** The same key is used by storage and Web Locks, so run IDs cannot split ownership. */
export function durableCheckpointNamespaceKey(input: DurableCheckpointNamespaceIdentity): string {
  const identity = durableCheckpointNamespaceIdentity(input)
  return `${FILE_CHECKPOINT_OWNERSHIP_MARKER}\0${FILE_CHECKPOINT_NAMESPACE}\0${identity.backend}\0${identity.transferIntentDigest}\0${identity.rootIdentity}`
}

function requireNamespacePart(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0 || value.includes('\0')) {
    throw new TypeError(`checkpoint namespace ${label} is invalid`)
  }
  return value
}

function requireDigest(value: string, label: string): string {
  const decoded = typeof value === 'string' ? decodeBase64Url(value) : undefined
  if (decoded === undefined || decoded.byteLength !== 32 || decoded.every((byte) => byte === 0)) {
    throw new TypeError(`checkpoint namespace ${label} is not a non-zero SHA-256 identity`)
  }
  return value
}
