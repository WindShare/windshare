import { decodeBase64Url, encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  canonicalTransferIntentBytes,
  validateFinalTransferIntent,
  validateTransferIntentDraft,
  type TransferIntent,
  type TransferIntentDraft,
} from '../../transfer/intent'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
} from '../capability/contract'
import {
  durableCheckpointNamespaceIdentity,
  durableCheckpointNamespaceKey,
  type DurableCheckpointNamespaceIdentity,
} from '../persistence/namespace'

export const PAUSED_TASK_DESCRIPTOR_SCHEMA_VERSION = 1 as const
export const ROOT_CAPABILITY_REFERENCE_BYTES = 32

export interface PausedTaskDescriptorV1 {
  readonly schemaVersion: typeof PAUSED_TASK_DESCRIPTOR_SCHEMA_VERSION
  readonly intent: TransferIntent
  readonly rootCapabilityRef: string
}

export class PausedTaskShareAuthorityError extends Error {
  constructor() {
    super('The current share authority does not match the paused transfer intent')
    this.name = 'PausedTaskShareAuthorityError'
  }
}

export async function pausedTaskDescriptorV1(input: {
  readonly intent: TransferIntent
  readonly rootCapabilityRef: string
}): Promise<PausedTaskDescriptorV1> {
  const intent = await validateFinalTransferIntent(input.intent)
  assertDurablePausedIntent(intent)
  return Object.freeze({
    schemaVersion: PAUSED_TASK_DESCRIPTOR_SCHEMA_VERSION,
    intent,
    rootCapabilityRef: snapshotRootCapabilityRef(input.rootCapabilityRef),
  })
}

/**
 * IndexedDB records are treated as untrusted structured data. Exact keys keep
 * reconstruction metadata from becoming a covert store for runtime authority.
 */
export async function validatePausedTaskDescriptorV1(
  input: unknown,
): Promise<PausedTaskDescriptorV1> {
  const descriptor = requireExactRecord(
    input,
    ['schemaVersion', 'intent', 'rootCapabilityRef'],
    'paused task descriptor',
  )
  if (descriptor.schemaVersion !== PAUSED_TASK_DESCRIPTOR_SCHEMA_VERSION) {
    throw new TypeError('paused task descriptor schema version is invalid')
  }
  assertStrictIntentShape(descriptor.intent)
  if (typeof descriptor.rootCapabilityRef !== 'string') {
    throw new TypeError('paused task root capability reference must be text')
  }
  return pausedTaskDescriptorV1({
    intent: descriptor.intent as unknown as TransferIntent,
    rootCapabilityRef: descriptor.rootCapabilityRef,
  })
}

export function createRootCapabilityRef(): string {
  const cryptoSource = globalThis.crypto
  if (cryptoSource?.getRandomValues === undefined) {
    throw new DOMException('Secure root capability reference generation is unavailable', 'NotSupportedError')
  }
  const bytes = new Uint8Array(ROOT_CAPABILITY_REFERENCE_BYTES)
  cryptoSource.getRandomValues(bytes)
  return snapshotRootCapabilityRef(encodeBase64Url(bytes))
}

export function snapshotRootCapabilityRef(value: string): string {
  const decoded = typeof value === 'string' ? decodeBase64Url(value) : undefined
  if (decoded === undefined ||
      decoded.byteLength !== ROOT_CAPABILITY_REFERENCE_BYTES ||
      decoded.every((byte) => byte === 0) ||
      encodeBase64Url(decoded) !== value) {
    throw new TypeError('root capability reference must be a canonical non-zero 32-byte identity')
  }
  return value
}

export function pausedTaskDescriptorNamespace(
  descriptor: PausedTaskDescriptorV1,
): DurableCheckpointNamespaceIdentity {
  return durableCheckpointNamespaceIdentity({
    backend: descriptor.intent.output.backend,
    transferIntentDigest: descriptor.intent.digest,
    rootIdentity: descriptor.intent.output.target,
  })
}

export function pausedTaskDescriptorKey(descriptor: PausedTaskDescriptorV1): string {
  return durableCheckpointNamespaceKey(pausedTaskDescriptorNamespace(descriptor))
}

export function assertPausedTaskCurrentShare(
  descriptor: PausedTaskDescriptorV1,
  currentShare: TransferIntentDraft,
): TransferIntentDraft {
  const authority = validateTransferIntentDraft(currentShare)
  const expected = canonicalTransferIntentBytes({
    shareInstance: authority.shareInstance,
    syntheticRoot: authority.syntheticRoot,
    selection: authority.selection,
    output: descriptor.intent.output,
  })
  if (!equalBytes(expected, descriptor.intent.canonicalBytes)) {
    throw new PausedTaskShareAuthorityError()
  }
  return authority
}

export function samePausedTaskDescriptor(
  left: PausedTaskDescriptorV1,
  right: PausedTaskDescriptorV1,
): boolean {
  return left.schemaVersion === right.schemaVersion &&
    left.rootCapabilityRef === right.rootCapabilityRef &&
    left.intent.version === right.intent.version &&
    left.intent.shareInstance === right.intent.shareInstance &&
    left.intent.syntheticRoot === right.intent.syntheticRoot &&
    sameSelection(left.intent.selection, right.intent.selection) &&
    left.intent.output.target === right.intent.output.target &&
    left.intent.output.targetKind === right.intent.output.targetKind &&
    left.intent.output.backend === right.intent.output.backend &&
    left.intent.output.format === right.intent.output.format &&
    left.intent.digest === right.intent.digest &&
    equalBytes(left.intent.canonicalBytes, right.intent.canonicalBytes)
}

function sameSelection(
  left: TransferIntent['selection'],
  right: TransferIntent['selection'],
): boolean {
  if (left.mode !== right.mode || left.defaultSelected !== right.defaultSelected) return false
  if (left.mode === 'catalog-path') {
    return right.mode === 'catalog-path' &&
      left.paths.length === right.paths.length &&
      left.paths.every((path, index) => path === right.paths[index])
  }
  return right.mode === 'node-id' &&
    left.rules.length === right.rules.length &&
    left.rules.every((rule, index) => {
      const candidate = right.rules[index]
      return candidate !== undefined &&
        rule.kind === candidate.kind &&
        rule.id === candidate.id &&
        rule.selected === candidate.selected
    })
}

function assertDurablePausedIntent(intent: TransferIntent): void {
  const output = intent.output
  const supported = (output.backend === FILE_SYSTEM_ACCESS_BACKEND && output.format === 'directory') ||
    (output.backend === ORIGIN_PRIVATE_BACKEND && output.format === 'zip')
  if (!supported || output.targetKind !== 2) {
    throw new TypeError('paused task intent does not describe a durable browser output')
  }
  const rootIdentity = decodeBase64Url(output.target)
  if (rootIdentity === undefined ||
      rootIdentity.byteLength !== ROOT_CAPABILITY_REFERENCE_BYTES ||
      rootIdentity.every((byte) => byte === 0) ||
      encodeBase64Url(rootIdentity) !== output.target) {
    throw new TypeError('paused task intent output target is not a canonical root identity')
  }
}

function assertStrictIntentShape(input: unknown): void {
  const intent = requireExactRecord(
    input,
    ['version', 'shareInstance', 'syntheticRoot', 'selection', 'output', 'digest', 'canonicalBytes'],
    'paused task transfer intent',
  )
  requireExactRecord(
    intent.output,
    ['target', 'backend', 'format', 'targetKind'],
    'paused task output locator',
  )
  const selection = requireRecord(intent.selection, 'paused task selection')
  if (selection.mode === 'catalog-path') {
    requireExactRecord(
      selection,
      ['mode', 'defaultSelected', 'paths'],
      'paused task catalog-path selection',
    )
    if (!Array.isArray(selection.paths)) {
      throw new TypeError('paused task catalog-path selection paths are invalid')
    }
    return
  }
  if (selection.mode !== 'node-id') {
    throw new TypeError('paused task selection mode is invalid')
  }
  requireExactRecord(
    selection,
    ['mode', 'defaultSelected', 'rules'],
    'paused task node selection',
  )
  if (!Array.isArray(selection.rules)) {
    throw new TypeError('paused task node selection rules are invalid')
  }
  for (const rule of selection.rules) {
    requireExactRecord(
      rule,
      ['kind', 'id', 'selected'],
      'paused task node selection rule',
    )
  }
}

function requireExactRecord(
  value: unknown,
  keys: readonly string[],
  label: string,
): Record<string, unknown> {
  const record = requireRecord(value, label)
  const actual = Object.keys(record).sort()
  const expected = [...keys].sort()
  if (actual.length !== expected.length ||
      actual.some((key, index) => key !== expected[index])) {
    throw new TypeError(`${label} has an invalid structured shape`)
  }
  return record
}

function requireRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new TypeError(`${label} must be a structured object`)
  }
  return value as Record<string, unknown>
}
