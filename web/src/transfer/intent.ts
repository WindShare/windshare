import { canonicalizePortableCatalogPath } from '../catalog/path-policy'
import { V2_MAXIMUM_SELECTION_RULE_OVERRIDES } from '../catalog/v2-selection'
import { decodeBase64Url, encodeBase64Url, equalBytes } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import type { V2CanonicalSelectionRule, V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import type { V2SelectionDecision } from '../catalog/v2-selection'

/** Go's TransferIntentV1 contract version. */
export const TRANSFER_INTENT_VERSION = 1 as const
const TRANSFER_INTENT_DOMAIN = new TextEncoder().encode('windshare/transfer-intent/v1\0')
const TEXT_ENCODER = new TextEncoder()
export const MAX_SELECTION_PATH_TARGET_BYTES = 1 << 20
export const MAX_OUTPUT_BACKEND_ID_BYTES = 128

export type TransferSelectionMode = 'catalog-path' | 'node-id'

export interface TransferNodeSelectionRule {
  readonly kind: 'directory' | 'file'
  /** Opaque catalog identity in canonical base64url form. */
  readonly id: string
  readonly selected: boolean
}

export interface TransferNodeSelectionRules {
  readonly mode: 'node-id'
  readonly defaultSelected: boolean
  readonly rules: readonly TransferNodeSelectionRule[]
}

export interface TransferCatalogPathSelectionRules {
  readonly mode: 'catalog-path'
  readonly defaultSelected: false
  /** Canonical relative catalog paths; path targets are kind-agnostic selections in Go. */
  readonly paths: readonly string[]
}

export type TransferSelectionRules = TransferNodeSelectionRules | TransferCatalogPathSelectionRules

export interface TransferOutputLocator {
  /**
   * A backend-issued opaque identity. Browser code deliberately cannot express
   * Go's host-filesystem target kind because it has no authority to canonicalize
   * a native absolute path.
   */
  readonly target: string
  readonly backend: string
  readonly format: 'directory' | 'single-file' | 'zip'
  /** Browser output authorities issue only opaque 32-byte target identities. */
  readonly targetKind: 2
}

export interface TransferIntent {
  readonly version: typeof TRANSFER_INTENT_VERSION
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly selection: TransferSelectionRules
  readonly output: TransferOutputLocator
  readonly digest: string
  readonly canonicalBytes: Uint8Array<ArrayBuffer>
}

export interface TransferIntentDraft {
  readonly state: 'draft'
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly selection: TransferSelectionRules
}

/** Runtime correlation is intentionally disjoint from durable transfer semantics. */
export interface TransferRun {
  readonly transferJobId: string
  readonly outputSessionId: string
}

export interface TransferTraceContext {
  readonly shareInstance: string
  readonly transferJobId: string
  readonly protocolSessionId?: string
  readonly outputSessionId?: string
  readonly directoryId?: string
  readonly generation?: string
  readonly fileId?: string
  readonly decision?: string
  readonly selectionDecision?: V2SelectionDecision
}

export type TransferTraceEventName =
  | 'download-t0'
  | 'intent-draft'
  | 'intent-frozen'
  | 'output-open'
  | 'directory-generation-committed'
  | 'directory-admitted'
  | 'directory-finalized'
  | 'file-enqueued'
  | 'file-started'
  | 'file-written'
  | 'file-completed'
  | 'discovery-complete'
  | 'discovery-failed'
  | 'output-finalized'
  | 'job-paused'
  | 'job-cancelled'
  | 'job-needs-attention'

export interface TransferTraceEvent {
  readonly name: TransferTraceEventName
  readonly atMilliseconds: number
  readonly context: TransferTraceContext
}

export type TransferTraceListener = (event: TransferTraceEvent) => void

export function createTransferJobId(): string {
  return createRuntimeIdentity('transfer-job')
}

export function createOutputSessionId(): string {
  return createRuntimeIdentity('output-session')
}

export function snapshotTransferRunId(value: string): string {
  return encodeBase64Url(identityBytes(value, 'transfer job'))
}

export function snapshotOutputSessionId(value: string): string {
  return encodeBase64Url(identityBytes(value, 'output session'))
}

export function snapshotProtocolSessionId(value: string): string {
  return encodeBase64Url(identityBytes(value, 'protocol session'))
}

export function createTransferIntentDraft(options: {
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly selection: TransferSelectionRules
}): TransferIntentDraft {
  const shareInstance = encodeBase64Url(identityBytes(options.shareInstance, 'share instance'))
  const syntheticRoot = encodeBase64Url(identityBytes(options.syntheticRoot, 'synthetic root'))
  return Object.freeze({
    state: 'draft' as const,
    shareInstance,
    syntheticRoot,
    selection: snapshotSelectionRules(options.selection),
  })
}

export function createTransferRun(): TransferRun {
  const transferJobId = createTransferJobId()
  let outputSessionId = createOutputSessionId()
  while (outputSessionId === transferJobId) outputSessionId = createOutputSessionId()
  return snapshotTransferRun({ transferJobId, outputSessionId })
}

export function snapshotTransferRun(input: TransferRun): TransferRun {
  const transferJobId = snapshotTransferRunId(input.transferJobId)
  const outputSessionId = snapshotOutputSessionId(input.outputSessionId)
  if (transferJobId === outputSessionId) {
    throw new TypeError('transfer job and output session identities must be independent')
  }
  return Object.freeze({ transferJobId, outputSessionId })
}

export function validateTransferIntentDraft(
  input: TransferIntentDraft,
  expected?: TransferIntentDraft,
): TransferIntentDraft {
  if (input.state !== 'draft') throw new TypeError('transfer intent draft state is invalid')
  const draft = createTransferIntentDraft({
    shareInstance: input.shareInstance,
    syntheticRoot: input.syntheticRoot,
    selection: input.selection,
  })
  if (expected !== undefined) {
    if (draft.shareInstance !== expected.shareInstance ||
        draft.syntheticRoot !== expected.syntheticRoot) {
      throw new TypeError('transfer intent draft does not match its share authority')
    }
    const share = identityBytes(draft.shareInstance, 'share instance')
    const root = identityBytes(draft.syntheticRoot, 'synthetic root')
    if (!equalBytes(
      canonicalSelectionBytes(share, root, draft.selection),
      canonicalSelectionBytes(share, root, snapshotSelectionRules(expected.selection)),
    )) throw new TypeError('transfer intent draft selection does not match the frozen job selection')
  }
  return draft
}

/**
 * Finalization is asynchronous only for WebCrypto. The draft itself is created
 * synchronously in the click handler, while this operation runs after the picker
 * confirms a target and before OpenOutput.
 */
export async function freezeTransferIntent(
  draft: TransferIntentDraft,
  output: TransferOutputLocator,
): Promise<TransferIntent> {
  draft = validateTransferIntentDraft(draft)
  const canonicalBytes = canonicalTransferIntentBytes({
    shareInstance: draft.shareInstance,
    syntheticRoot: draft.syntheticRoot,
    selection: draft.selection,
    output,
  })
  const digest = encodeBase64Url(await sha256(canonicalBytes))
  return Object.freeze({
    version: TRANSFER_INTENT_VERSION,
    shareInstance: draft.shareInstance,
    syntheticRoot: draft.syntheticRoot,
    selection: draft.selection,
    output: snapshotOutputLocator(output),
    digest,
    canonicalBytes,
  })
}

/** Reconstructs canonical authority and rejects stale or caller-forged final fields. */
export async function validateFinalTransferIntent(
  input: TransferIntent,
  expected?: TransferIntentDraft,
): Promise<TransferIntent> {
  if (input.version !== TRANSFER_INTENT_VERSION) throw new TypeError('transfer intent version is invalid')
  const selection = snapshotSelectionRules(input.selection)
  const output = snapshotOutputLocator(input.output)
  const canonicalBytes = canonicalTransferIntentBytes({
    shareInstance: input.shareInstance,
    syntheticRoot: input.syntheticRoot,
    selection,
    output,
  })
  if (!(input.canonicalBytes instanceof Uint8Array) || !equalBytes(input.canonicalBytes, canonicalBytes)) {
    throw new TypeError('transfer intent canonical bytes do not match its semantic fields')
  }
  const digest = encodeBase64Url(await sha256(canonicalBytes))
  if (input.digest !== digest) throw new TypeError('transfer intent digest does not match its canonical bytes')
  if (expected !== undefined) {
    if (input.shareInstance !== expected.shareInstance ||
        input.syntheticRoot !== expected.syntheticRoot) {
      throw new TypeError('transfer intent does not match its share authority')
    }
    const share = identityBytes(input.shareInstance, 'share instance')
    const root = identityBytes(input.syntheticRoot, 'synthetic root')
    if (!equalBytes(
      canonicalSelectionBytes(share, root, selection),
      canonicalSelectionBytes(share, root, snapshotSelectionRules(expected.selection)),
    )) throw new TypeError('transfer intent selection does not match the frozen job selection')
  }
  return Object.freeze({
    version: TRANSFER_INTENT_VERSION,
    shareInstance: input.shareInstance,
    syntheticRoot: input.syntheticRoot,
    selection,
    output,
    digest,
    canonicalBytes,
  })
}

/**
 * Canonical bytes match core/transfer/intent.go:
 * domain\0, version, length-prefixed selection request, then length-prefixed
 * target kind/payload, backend, and output mode. Job/session IDs are absent.
 */
export function canonicalTransferIntentBytes(input: {
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly selection: TransferSelectionRules
  readonly output: TransferOutputLocator
}): Uint8Array<ArrayBuffer> {
  const share = identityBytes(input.shareInstance, 'share instance')
  const root = identityBytes(input.syntheticRoot, 'synthetic root')
  const selection = snapshotSelectionRules(input.selection)
  const selectionBytes = canonicalSelectionBytes(share, root, selection)
  const output = snapshotOutputLocator(input.output)
  const targetKind = output.targetKind
  const targetPayload = persistentIdentityBytes(output.target)
  return concat([
    TRANSFER_INTENT_DOMAIN,
    Uint8Array.of(TRANSFER_INTENT_VERSION),
    canonicalField(selectionBytes),
    canonicalField(Uint8Array.of(targetKind)),
    canonicalField(targetPayload),
    canonicalField(new TextEncoder().encode(output.backend)),
    canonicalField(Uint8Array.of(outputMode(output.format))),
  ])
}

export async function transferIntentDigest(
  intent: TransferIntent | TransferIntentDraft,
  output?: TransferOutputLocator,
): Promise<string> {
  if ('digest' in intent) return (await validateFinalTransferIntent(intent)).digest
  if (output === undefined) throw new TypeError('final transfer intent output is required')
  const bytes = canonicalTransferIntentBytes({
    shareInstance: intent.shareInstance,
    syntheticRoot: intent.syntheticRoot,
    selection: intent.selection,
    output,
  })
  return encodeBase64Url(await sha256(bytes))
}

export function selectionRulesFromPolicy(policy: V2FrozenSelectionPolicy): TransferSelectionRules {
  return Object.freeze({
    mode: 'node-id' as const,
    defaultSelected: policy.defaultSelected,
    rules: Object.freeze(policy.canonicalRules.map(canonicalRule)),
  })
}

function canonicalRule(rule: V2CanonicalSelectionRule): TransferNodeSelectionRule {
  return Object.freeze({
    kind: rule.kind,
    id: encodeBase64Url(rule.id),
    selected: rule.selected,
  })
}

function canonicalSelectionBytes(
  share: Uint8Array,
  root: Uint8Array,
  selection: TransferSelectionRules,
): Uint8Array<ArrayBuffer> {
  const mode = selection.mode === 'node-id' ? 1 : 2
  const fields: Uint8Array[] = [
    canonicalField(share),
    canonicalField(root),
    canonicalField(Uint8Array.of(mode)),
    canonicalField(Uint8Array.of(selection.defaultSelected ? 1 : 0)),
  ]
  if (selection.mode === 'catalog-path') {
    fields.push(uint64Bytes(BigInt(selection.paths.length)))
    for (const path of selection.paths) fields.push(canonicalField(TEXT_ENCODER.encode(path)))
    return concat(fields)
  }
  const rules = [...selection.rules].sort((left, right) => {
    const byId = compareBytes(identityBytes(left.id, 'selection rule identity'), identityBytes(right.id, 'selection rule identity'))
    return byId !== 0 ? byId : kindByte(left.kind) - kindByte(right.kind)
  })
  fields.push(uint64Bytes(BigInt(rules.length)))
  for (const rule of rules) {
    fields.push(canonicalField(Uint8Array.of(kindByte(rule.kind))))
    fields.push(canonicalField(identityBytes(rule.id, 'selection rule identity')))
    fields.push(canonicalField(Uint8Array.of(rule.selected ? 1 : 0)))
  }
  return concat(fields)
}

function snapshotSelectionRules(input: TransferSelectionRules): TransferSelectionRules {
  if (input.mode !== 'catalog-path' && input.mode !== 'node-id') {
    throw new TypeError('transfer selection mode is invalid')
  }
  if (typeof input.defaultSelected !== 'boolean') {
    throw new TypeError('transfer selection default must be boolean')
  }
  if (input.mode === 'catalog-path') {
    return snapshotCatalogPathSelection(input)
  }
  return snapshotNodeSelection(input)
}

function snapshotCatalogPathSelection(
  input: TransferCatalogPathSelectionRules,
): TransferCatalogPathSelectionRules {
  if (Object.hasOwn(input, 'rules') || input.defaultSelected !== false ||
      !Array.isArray(input.paths) || input.paths.length === 0 ||
      input.paths.length > V2_MAXIMUM_SELECTION_RULE_OVERRIDES) {
    throw new TypeError('catalog-path selection has an invalid authority shape')
  }
  const canonical = new Set<string>()
  let totalBytes = 0
  for (const path of input.paths) {
    if (typeof path !== 'string') throw new TypeError('catalog-path selection target must be text')
    const target = canonicalizePortableCatalogPath(path)
    if (canonical.has(target)) continue
    totalBytes += TEXT_ENCODER.encode(target).byteLength
    if (totalBytes > MAX_SELECTION_PATH_TARGET_BYTES) {
      throw new RangeError('catalog-path selection targets exceed the protocol byte limit')
    }
    canonical.add(target)
  }
  const paths = [...canonical].sort((left, right) =>
    compareBytes(TEXT_ENCODER.encode(left), TEXT_ENCODER.encode(right)))
  return Object.freeze({
    mode: 'catalog-path' as const,
    defaultSelected: false as const,
    paths: Object.freeze(paths),
  })
}

function snapshotNodeSelection(input: TransferNodeSelectionRules): TransferNodeSelectionRules {
  if (Object.hasOwn(input, 'paths') || !Array.isArray(input.rules) ||
      input.rules.length > V2_MAXIMUM_SELECTION_RULE_OVERRIDES) {
    throw new TypeError('node-id selection has an invalid authority shape')
  }
  const rules = [...input.rules].map((rule) => {
    if (rule.kind !== 'directory' && rule.kind !== 'file') {
      throw new TypeError('transfer selection rule kind is invalid')
    }
    if (typeof rule.selected !== 'boolean') throw new TypeError('transfer selection rule decision must be boolean')
    // Go's catalog IDs are fixed-width authenticated identities. Rejecting
    // malformed IDs prevents a non-interoperable durable namespace.
    identityBytes(rule.id, 'selection rule identity')
    return Object.freeze({
      kind: rule.kind,
      id: requireText(rule.id, 'selection rule ID'),
      selected: rule.selected,
    })
  })
  const seen = new Set<string>()
  for (const rule of rules) {
    const key = rule.id
    if (seen.has(key)) throw new TypeError('node-id selection contains a duplicate rule')
    seen.add(key)
  }
  return Object.freeze({
    mode: 'node-id' as const,
    defaultSelected: input.defaultSelected,
    rules: Object.freeze(rules),
  })
}

function snapshotOutputLocator(input: TransferOutputLocator): TransferOutputLocator {
  if (input.format !== 'directory' && input.format !== 'single-file' && input.format !== 'zip') {
    throw new TypeError('transfer output format is invalid')
  }
  if (input.targetKind !== 2) throw new TypeError('browser transfer output requires an opaque target identity')
  persistentIdentityBytes(input.target)
  return Object.freeze({
    target: requireText(input.target, 'output target'),
    backend: outputBackend(input.backend),
    format: input.format,
    targetKind: input.targetKind,
  })
}

function createRuntimeIdentity(label: 'transfer-job' | 'output-session'): string {
  const cryptoSource = globalThis.crypto
  if (cryptoSource?.getRandomValues === undefined) {
    throw new DOMException(
      `Secure ${label} identity generation is unavailable`,
      'NotSupportedError',
    )
  }
  const identity = new Uint8Array(16)
  cryptoSource.getRandomValues(identity)
  if (identity.every((byte) => byte === 0)) {
    throw new Error(`Generated ${label} identity was all zeroes`)
  }
  return encodeBase64Url(identity)
}

function identityBytes(value: string, label: string): Uint8Array<ArrayBuffer> {
  if (typeof value !== 'string') throw new TypeError(`${label} must be a base64url identity`)
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== 16 || decoded.every((byte) => byte === 0) ||
      encodeBase64Url(decoded) !== value) {
    throw new TypeError(`${label} must be a non-zero 16-byte base64url identity`)
  }
  return Uint8Array.from(decoded)
}

function persistentIdentityBytes(value: string): Uint8Array<ArrayBuffer> {
  if (typeof value !== 'string') throw new TypeError('opaque output target must be a base64url identity')
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== 32 || decoded.every((byte) => byte === 0) ||
      encodeBase64Url(decoded) !== value) {
    throw new TypeError('opaque output target must be a non-zero 32-byte base64url identity')
  }
  return Uint8Array.from(decoded)
}

function outputMode(format: TransferOutputLocator['format']): number {
  if (format === 'directory') return 1
  if (format === 'single-file') return 2
  return 3
}

function kindByte(kind: TransferNodeSelectionRule['kind']): number {
  return kind === 'directory' ? 1 : 2
}

function canonicalField(value: Uint8Array): Uint8Array<ArrayBuffer> {
  return concat([uint64Bytes(BigInt(value.byteLength)), value])
}

function uint64Bytes(value: bigint): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(8)
  new DataView(bytes.buffer).setBigUint64(0, value, false)
  return bytes
}

function concat(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const length = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = (left[index] ?? 0) - (right[index] ?? 0)
    if (difference !== 0) return difference
  }
  return left.byteLength - right.byteLength
}

function requireText(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new TypeError(`${label} must not be empty`)
  return value
}

function outputBackend(value: string): string {
  if (typeof value !== 'string' || value.length === 0 || !isWellFormedUnicode(value) ||
      TEXT_ENCODER.encode(value).byteLength > MAX_OUTPUT_BACKEND_ID_BYTES ||
      /^\p{White_Space}|\p{White_Space}$/u.test(value)) {
    throw new TypeError('output backend is not a canonical backend identifier')
  }
  return value
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) return false
  }
  return true
}
