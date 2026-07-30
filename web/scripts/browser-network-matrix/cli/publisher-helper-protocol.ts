import { createHash } from 'node:crypto'

import {
  requireArray,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSha256,
  requireString,
} from '../../browser-evidence/contract/json.ts'
import { parseCanonicalJsonText } from '../../browser-evidence/contract/strict-json.ts'

export const PUBLISHER_HELPER_SCHEMA_VERSION =
  'windshare.artifact-publisher/v2' as const
export const PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES = 48 * 1024 * 1024

export type PublisherHelperOperation = 'directory' | 'file'
export type PublisherHelperFailureCode =
  | 'protocol-invalid'
  | 'destination-exists'
  | 'publication-unsafe'
  | 'response-failed'

export interface PublisherHelperArtifact {
  readonly name: string
  readonly bytes: Uint8Array
  readonly sha256: string
}

export interface PublisherHelperRequest {
  readonly operation: PublisherHelperOperation
  readonly parentPath: string
  readonly outputName: string
  readonly stagingName: string
  readonly artifacts: readonly PublisherHelperArtifact[]
}

export type ExistingDirectoryPublisherOperation =
  | 'prepare-existing-directory'
  | 'publish-existing-directory'
  | 'verify-existing-directory'
  | 'cleanup-existing-directory'

export interface ExistingDirectoryPublisherFile {
  readonly relativePath: string
  readonly byteLength: string
  readonly sha256: string
}

export interface ExistingDirectoryPublisherInventory {
  readonly directories: readonly string[]
  readonly files: readonly ExistingDirectoryPublisherFile[]
}

interface ExistingDirectoryPublisherRequestBase {
  readonly parentPath: string
  readonly outputName: 'sealed'
  readonly inventory: ExistingDirectoryPublisherInventory
  readonly manifestPath: 'manifest.json'
  readonly expectedManifestSha256: string
}

export interface PrepareExistingDirectoryPublisherRequest
  extends ExistingDirectoryPublisherRequestBase {
  readonly operation: 'prepare-existing-directory'
  readonly stagingName: string
}

export interface PublishExistingDirectoryPublisherRequest
  extends ExistingDirectoryPublisherRequestBase {
  readonly operation: 'publish-existing-directory'
  readonly stagingName: string
  readonly stagingReceipt: string
  readonly snapshotPaths: readonly string[]
}

export interface VerifyExistingDirectoryPublisherRequest
  extends ExistingDirectoryPublisherRequestBase {
  readonly operation: 'verify-existing-directory'
  readonly stagingName: ''
  readonly snapshotPaths: readonly string[]
}

export interface CleanupExistingDirectoryPublisherRequest
  extends ExistingDirectoryPublisherRequestBase {
  readonly operation: 'cleanup-existing-directory'
  readonly stagingName: string
  readonly stagingReceipt: string
}

export type ExistingDirectoryPublisherRequest =
  | PrepareExistingDirectoryPublisherRequest
  | PublishExistingDirectoryPublisherRequest
  | VerifyExistingDirectoryPublisherRequest
  | CleanupExistingDirectoryPublisherRequest

export interface ExistingDirectoryPublisherSnapshot {
  readonly relativePath: string
  readonly byteLength: string
  readonly bytes: Uint8Array
  readonly sha256: string
}

export type ExistingDirectoryCleanupOutcome = 'absent' | 'completed' | 'ambiguous'

export type ExistingDirectoryPublisherCompletedResponse =
  | {
      readonly outcome: 'completed'
      readonly operation: 'prepare-existing-directory'
      readonly stagingReceipt: string
    }
  | {
      readonly outcome: 'completed'
      readonly operation: 'publish-existing-directory' | 'verify-existing-directory'
      readonly manifestSha256: string
      readonly snapshots: readonly ExistingDirectoryPublisherSnapshot[]
    }
  | {
      readonly outcome: 'completed'
      readonly operation: 'cleanup-existing-directory'
      readonly cleanupOutcome: ExistingDirectoryCleanupOutcome
    }

export interface ExistingDirectoryPublisherFailedResponse extends PublisherHelperFailedResponse {
  readonly operation: ExistingDirectoryPublisherOperation
  /** Only prepare may return a receipt after native handle-settlement ambiguity. */
  readonly stagingReceipt: string | null
  /** Cleanup reports its native tri-state even when settlement itself failed. */
  readonly cleanupOutcome: ExistingDirectoryCleanupOutcome | null
}

export type ExistingDirectoryPublisherResponse =
  | ExistingDirectoryPublisherCompletedResponse
  | ExistingDirectoryPublisherFailedResponse

export type ExistingDirectoryPublisherResponseFor<
  Operation extends ExistingDirectoryPublisherOperation,
> =
  | (Operation extends 'prepare-existing-directory'
      ? Extract<ExistingDirectoryPublisherCompletedResponse, {
          readonly operation: 'prepare-existing-directory'
        }>
      : Operation extends 'cleanup-existing-directory'
        ? Extract<ExistingDirectoryPublisherCompletedResponse, {
            readonly operation: 'cleanup-existing-directory'
          }>
        : Extract<ExistingDirectoryPublisherCompletedResponse, {
            readonly manifestSha256: string
          }> & { readonly operation: Operation })
  | (Omit<ExistingDirectoryPublisherFailedResponse, 'operation'> & {
      readonly operation: Operation
    })

export interface PublisherHelperCompletedResponse {
  readonly outcome: 'completed'
  readonly artifacts: readonly PublisherHelperArtifact[]
}

export interface PublisherHelperFailedResponse {
  readonly outcome: 'failed'
  readonly failureCode: PublisherHelperFailureCode
}

export type PublisherHelperResponse =
  | PublisherHelperCompletedResponse
  | PublisherHelperFailedResponse

const RESPONSE_KEYS = Object.freeze(['schemaVersion', 'outcome', 'failureCode', 'artifacts'])
const ARTIFACT_KEYS = Object.freeze(['name', 'bytesBase64', 'sha256'])
const OUTCOMES = Object.freeze(['completed', 'failed'] as const)
const FAILURE_CODES = Object.freeze([
  'protocol-invalid',
  'destination-exists',
  'publication-unsafe',
  'response-failed',
] as const)
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true })
const EXISTING_DIRECTORY_OPERATIONS = Object.freeze([
  'prepare-existing-directory',
  'publish-existing-directory',
  'verify-existing-directory',
  'cleanup-existing-directory',
] as const)
const EXISTING_RESPONSE_BASE_KEYS = Object.freeze([
  'schemaVersion', 'outcome', 'failureCode', 'artifacts',
])
const EXISTING_SNAPSHOT_KEYS = Object.freeze([
  'relativePath', 'byteLength', 'bytesBase64', 'sha256',
])
const EXISTING_CLEANUP_OUTCOMES = Object.freeze(['absent', 'completed', 'ambiguous'] as const)
const MAXIMUM_EXISTING_SNAPSHOT_BYTES = 16 * 1024 * 1024
const MAXIMUM_EXISTING_SNAPSHOT_TOTAL_BYTES = 32 * 1024 * 1024

export function publisherHelperArtifact(name: string, bytes: Uint8Array): PublisherHelperArtifact {
  const owned = Uint8Array.from(bytes)
  return Object.freeze({ name, bytes: owned, sha256: sha256(owned) })
}

export function encodePublisherHelperRequest(request: PublisherHelperRequest): Uint8Array {
  const encoded = Buffer.from(encodeGoCanonicalJson({
    schemaVersion: PUBLISHER_HELPER_SCHEMA_VERSION,
    operation: request.operation,
    parentPath: request.parentPath,
    outputName: request.outputName,
    stagingName: request.stagingName,
    artifacts: request.artifacts.map((artifact) => ({
      name: artifact.name,
      bytesBase64: Buffer.from(artifact.bytes).toString('base64'),
      sha256: artifact.sha256,
    })),
  }), 'utf8')
  if (encoded.byteLength < 1 || encoded.byteLength > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES) {
    throw new Error('browser network matrix publisher request exceeds its byte authority')
  }
  return encoded
}

export function encodeExistingDirectoryPublisherRequest(
  request: ExistingDirectoryPublisherRequest,
): Uint8Array {
  requireEnum(request.operation, EXISTING_DIRECTORY_OPERATIONS, 'artifact publisher operation')
  const wire: Record<string, unknown> = {
    schemaVersion: PUBLISHER_HELPER_SCHEMA_VERSION,
    operation: request.operation,
    parentPath: request.parentPath,
    outputName: request.outputName,
    stagingName: request.stagingName,
    artifacts: [],
    inventory: {
      directories: [...request.inventory.directories],
      files: request.inventory.files.map((file) => ({
        relativePath: file.relativePath,
        byteLength: requireCanonicalUnsignedDecimal(file.byteLength, 'artifact publisher file length'),
        sha256: requireSha256(file.sha256, 'artifact publisher file digest'),
      })),
    },
    manifestPath: request.manifestPath,
    expectedManifestSha256: requireSha256(
      request.expectedManifestSha256,
      'artifact publisher expected manifest digest',
    ),
  }
  if (
    request.operation === 'publish-existing-directory' ||
    request.operation === 'verify-existing-directory'
  ) {
    if (request.snapshotPaths.length > 0) wire.snapshotPaths = [...request.snapshotPaths]
    if (request.operation === 'publish-existing-directory') {
      wire.stagingReceipt = requireStagingReceipt(
        request.stagingReceipt,
        'artifact publisher staging receipt',
      )
    }
  } else if (request.operation === 'cleanup-existing-directory') {
    wire.stagingReceipt = requireStagingReceipt(
      request.stagingReceipt,
      'artifact publisher staging receipt',
    )
  }
  const encoded = Buffer.from(encodeGoCanonicalJson(wire), 'utf8')
  if (encoded.byteLength < 1 || encoded.byteLength > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES) {
    throw new Error('artifact publisher existing-directory request exceeds its byte authority')
  }
  return encoded
}

export function parsePublisherHelperResponse(encoded: Uint8Array): PublisherHelperResponse {
  if (encoded.byteLength < 1 || encoded.byteLength > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES) {
    throw new Error('browser network matrix publisher response exceeds its byte authority')
  }
  let text: string
  try {
    text = UTF8_DECODER.decode(encoded)
  } catch {
    throw new Error('browser network matrix publisher response is not valid UTF-8')
  }
  const parsed = requireRecord(
    parseCanonicalJsonText(text, 'browser network matrix publisher response'),
    'browser network matrix publisher response',
  )
  requireExactKeys(parsed, RESPONSE_KEYS, [], 'browser network matrix publisher response')
  requireKeyOrder(parsed, RESPONSE_KEYS, 'browser network matrix publisher response')
  if (text !== encodeGoCanonicalJsonLine(parsed)) {
    throw new Error('browser network matrix publisher response is not canonical JSON')
  }
  requireLiteral(
    parsed.schemaVersion,
    PUBLISHER_HELPER_SCHEMA_VERSION,
    'browser network matrix publisher schema version',
  )
  const outcome = requireEnum(
    parsed.outcome,
    OUTCOMES,
    'browser network matrix publisher outcome',
  )
  const artifacts = requireArray(parsed.artifacts, 'browser network matrix publisher artifacts')
  if (outcome === 'failed') {
    if (artifacts.length !== 0) {
      throw new Error('failed browser network matrix publication returned artifacts')
    }
    return Object.freeze({
      outcome,
      failureCode: requireEnum(
        parsed.failureCode,
        FAILURE_CODES,
        'browser network matrix publisher failure code',
      ),
    })
  }
  if (parsed.failureCode !== null || artifacts.length < 1 || artifacts.length > 8) {
    throw new Error('completed browser network matrix publication has contradictory fields')
  }
  return Object.freeze({
    outcome,
    artifacts: Object.freeze(artifacts.map((artifact, index) =>
      parseArtifact(artifact, index))),
  })
}

export function parseExistingDirectoryPublisherResponse(
  encoded: Uint8Array,
  operation: ExistingDirectoryPublisherOperation,
): ExistingDirectoryPublisherResponse {
  if (encoded.byteLength < 1 || encoded.byteLength > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES) {
    throw new Error('artifact publisher existing-directory response exceeds its byte authority')
  }
  let encodedText: string
  try {
    encodedText = UTF8_DECODER.decode(encoded)
  } catch {
    throw new Error('artifact publisher existing-directory response is not valid UTF-8')
  }
  const parsed = requireRecord(
    parseCanonicalJsonText(encodedText, 'artifact publisher existing-directory response'),
    'artifact publisher existing-directory response',
  )
  requireLiteral(
    parsed.schemaVersion,
    PUBLISHER_HELPER_SCHEMA_VERSION,
    'artifact publisher existing-directory schema version',
  )
  const outcome = requireEnum(parsed.outcome, OUTCOMES, 'artifact publisher existing-directory outcome')
  const artifacts = requireArray(parsed.artifacts, 'artifact publisher existing-directory artifacts')
  if (artifacts.length !== 0) {
    throw new Error('artifact publisher existing-directory response contains inline artifacts')
  }
  if (outcome === 'failed') {
    return parseExistingDirectoryFailureResponse(parsed, encodedText, operation)
  }
  return parseExistingDirectoryCompletedResponse(parsed, encodedText, operation)
}

function parseExistingDirectoryFailureResponse(
  parsed: Record<string, unknown>,
  encodedText: string,
  operation: ExistingDirectoryPublisherOperation,
): ExistingDirectoryPublisherFailedResponse {
  const hasReceipt = Object.hasOwn(parsed, 'stagingReceipt')
  const hasCleanupOutcome = Object.hasOwn(parsed, 'cleanupOutcome')
  if (
    (hasReceipt && operation !== 'prepare-existing-directory') ||
    (hasCleanupOutcome && operation !== 'cleanup-existing-directory')
  ) {
    throw new Error('failed artifact publisher response contains cross-operation authority')
  }
  const expectedKeys = Object.freeze([
    ...EXISTING_RESPONSE_BASE_KEYS,
    ...(hasReceipt ? ['stagingReceipt'] : []),
    ...(hasCleanupOutcome ? ['cleanupOutcome'] : []),
  ])
  requireExactKeys(parsed, expectedKeys, [], 'artifact publisher failure response')
  requireKeyOrder(parsed, expectedKeys, 'artifact publisher failure response')
  requireCanonicalNativeResponse(encodedText, parsed)
  return Object.freeze({
    outcome: 'failed',
    operation,
    failureCode: requireEnum(
      parsed.failureCode,
      FAILURE_CODES,
      'artifact publisher existing-directory failure code',
    ),
    stagingReceipt: hasReceipt
      ? requireStagingReceipt(parsed.stagingReceipt, 'artifact publisher returned staging receipt')
      : null,
    cleanupOutcome: hasCleanupOutcome
      ? requireEnum(
          parsed.cleanupOutcome,
          EXISTING_CLEANUP_OUTCOMES,
          'artifact publisher cleanup outcome',
        )
      : null,
  })
}

function parseExistingDirectoryCompletedResponse(
  parsed: Record<string, unknown>,
  encodedText: string,
  operation: ExistingDirectoryPublisherOperation,
): ExistingDirectoryPublisherCompletedResponse {
  if (parsed.failureCode !== null) {
    throw new Error('completed artifact publisher response contains a failure code')
  }
  if (operation === 'prepare-existing-directory') {
    return parseExistingDirectoryPreparationResponse(parsed, encodedText)
  }
  if (operation === 'cleanup-existing-directory') {
    return parseExistingDirectoryCleanupResponse(parsed, encodedText)
  }
  return parseExistingDirectorySnapshotResponse(parsed, encodedText, operation)
}

function parseExistingDirectoryPreparationResponse(
  parsed: Record<string, unknown>,
  encodedText: string,
): Extract<ExistingDirectoryPublisherCompletedResponse, { readonly operation: 'prepare-existing-directory' }> {
  const expectedKeys = Object.freeze([...EXISTING_RESPONSE_BASE_KEYS, 'stagingReceipt'])
  requireExactKeys(parsed, expectedKeys, [], 'completed artifact publisher preparation response')
  requireKeyOrder(parsed, expectedKeys, 'completed artifact publisher preparation response')
  requireCanonicalNativeResponse(encodedText, parsed)
  return Object.freeze({
    outcome: 'completed',
    operation: 'prepare-existing-directory',
    stagingReceipt: requireStagingReceipt(
      parsed.stagingReceipt,
      'artifact publisher returned staging receipt',
    ),
  })
}

function parseExistingDirectoryCleanupResponse(
  parsed: Record<string, unknown>,
  encodedText: string,
): Extract<ExistingDirectoryPublisherCompletedResponse, { readonly operation: 'cleanup-existing-directory' }> {
  const expectedKeys = Object.freeze([...EXISTING_RESPONSE_BASE_KEYS, 'cleanupOutcome'])
  requireExactKeys(parsed, expectedKeys, [], 'completed artifact publisher cleanup response')
  requireKeyOrder(parsed, expectedKeys, 'completed artifact publisher cleanup response')
  requireCanonicalNativeResponse(encodedText, parsed)
  return Object.freeze({
    outcome: 'completed',
    operation: 'cleanup-existing-directory',
    cleanupOutcome: requireEnum(
      parsed.cleanupOutcome,
      EXISTING_CLEANUP_OUTCOMES,
      'artifact publisher cleanup outcome',
    ),
  })
}

function parseExistingDirectorySnapshotResponse(
  parsed: Record<string, unknown>,
  encodedText: string,
  operation: 'publish-existing-directory' | 'verify-existing-directory',
): Extract<ExistingDirectoryPublisherCompletedResponse, { readonly manifestSha256: string }> {
  const hasSnapshots = Object.hasOwn(parsed, 'snapshots')
  const expectedKeys = Object.freeze([
    ...EXISTING_RESPONSE_BASE_KEYS,
    'manifestSha256',
    ...(hasSnapshots ? ['snapshots'] : []),
  ])
  requireExactKeys(parsed, expectedKeys, [], 'completed artifact publisher response')
  requireKeyOrder(parsed, expectedKeys, 'completed artifact publisher response')
  requireCanonicalNativeResponse(encodedText, parsed)
  const snapshots = hasSnapshots
    ? requireArray(parsed.snapshots, 'artifact publisher snapshots').map(parseExistingSnapshot)
    : []
  let totalBytes = 0
  for (const snapshot of snapshots) {
    totalBytes += snapshot.bytes.byteLength
    if (totalBytes > MAXIMUM_EXISTING_SNAPSHOT_TOTAL_BYTES) {
      throw new Error('artifact publisher snapshots exceed their total byte authority')
    }
  }
  return Object.freeze({
    outcome: 'completed',
    operation,
    manifestSha256: requireSha256(
      parsed.manifestSha256,
      'artifact publisher returned manifest digest',
    ),
    snapshots: Object.freeze(snapshots),
  })
}

export function requireExactPublishedArtifacts(
  expected: readonly PublisherHelperArtifact[],
  actual: readonly PublisherHelperArtifact[],
): void {
  if (expected.length !== actual.length) {
    throw new Error('browser network matrix publisher returned the wrong artifact count')
  }
  for (let index = 0; index < expected.length; index += 1) {
    const left = expected[index]
    const right = actual[index]
    if (
      left === undefined || right === undefined || left.name !== right.name ||
      left.sha256 !== right.sha256 || !Buffer.from(left.bytes).equals(Buffer.from(right.bytes))
    ) throw new Error('browser network matrix publisher changed canonical artifact bytes')
  }
}

function parseArtifact(value: unknown, index: number): PublisherHelperArtifact {
  const label = `browser network matrix publisher artifact ${index}`
  const artifact = requireRecord(value, label)
  requireExactKeys(artifact, ARTIFACT_KEYS, [], label)
  requireKeyOrder(artifact, ARTIFACT_KEYS, label)
  const name = requireString(artifact.name, `${label} name`, 255)
  const bytesBase64 = requireString(
    artifact.bytesBase64,
    `${label} bytes`,
    PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES,
  )
  const bytes = decodeCanonicalBase64(bytesBase64, label)
  const digest = requireSha256(artifact.sha256, `${label} digest`)
  if (sha256(bytes) !== digest) throw new Error(`${label} digest does not bind its bytes`)
  return Object.freeze({ name, bytes, sha256: digest })
}

function parseExistingSnapshot(value: unknown, index: number): ExistingDirectoryPublisherSnapshot {
  const label = `artifact publisher snapshot ${index}`
  const snapshot = requireRecord(value, label)
  requireExactKeys(snapshot, EXISTING_SNAPSHOT_KEYS, [], label)
  requireKeyOrder(snapshot, EXISTING_SNAPSHOT_KEYS, label)
  const relativePath = requireString(snapshot.relativePath, `${label} path`, 4_096)
  const byteLength = requireCanonicalUnsignedDecimal(snapshot.byteLength, `${label} byte length`)
  const numericLength = Number(BigInt(byteLength))
  if (!Number.isSafeInteger(numericLength) || numericLength > MAXIMUM_EXISTING_SNAPSHOT_BYTES) {
    throw new Error(`${label} exceeds its byte authority`)
  }
  const encoded = requireString(
    snapshot.bytesBase64,
    `${label} bytes`,
    Math.ceil(MAXIMUM_EXISTING_SNAPSHOT_BYTES / 3) * 4,
  )
  const bytes = Buffer.from(encoded, 'base64')
  if (bytes.byteLength !== numericLength || bytes.toString('base64') !== encoded) {
    throw new Error(`${label} bytes do not match their canonical length authority`)
  }
  const digest = requireSha256(snapshot.sha256, `${label} digest`)
  if (sha256(bytes) !== digest) throw new Error(`${label} digest does not bind its bytes`)
  return Object.freeze({ relativePath, byteLength, bytes: Uint8Array.from(bytes), sha256: digest })
}

function requireCanonicalUnsignedDecimal(value: unknown, label: string): string {
  const encoded = requireString(value, label, 32)
  if (!/^(?:0|[1-9]\d*)$/u.test(encoded)) {
    throw new Error(`${label} is not canonical unsigned decimal`)
  }
  const parsed = BigInt(encoded)
  if (parsed > 18_446_744_073_709_551_615n) {
    throw new Error(`${label} exceeds uint64 authority`)
  }
  return encoded
}

function requireStagingReceipt(value: unknown, label: string): string {
  const encoded = requireString(value, label, 5_464)
  const decoded = Buffer.from(encoded, 'base64')
  if (
    decoded.byteLength < 1 || decoded.byteLength > 4_096 ||
    decoded.toString('base64') !== encoded
  ) {
    throw new Error(`${label} is not bounded canonical base64`)
  }
  return encoded
}

function requireCanonicalNativeResponse(
  encoded: string,
  parsed: Readonly<Record<string, unknown>>,
): void {
  if (encoded !== encodeGoCanonicalJsonLine(parsed)) {
    throw new Error('artifact publisher existing-directory response is not canonical JSON')
  }
}

function decodeCanonicalBase64(encoded: string, label: string): Uint8Array {
  const decoded = Buffer.from(encoded, 'base64')
  if (decoded.byteLength < 1 || decoded.toString('base64') !== encoded) {
    throw new Error(`${label} bytes are not canonical base64`)
  }
  return Uint8Array.from(decoded)
}

function requireKeyOrder(
  value: Readonly<Record<string, unknown>>,
  expected: readonly string[],
  label: string,
): void {
  if (Object.keys(value).some((key, index) => key !== expected[index])) {
    throw new Error(`${label} fields are not in canonical order`)
  }
}

function encodeGoCanonicalJsonLine(value: unknown): string {
  // The native helper uses encoding/json with HTML escaping enabled. Reconstructing
  // that exact wire form rejects leading/trailing whitespace and multiple values,
  // so only the one response whose bytes were authenticated can carry authority.
  return `${encodeGoCanonicalJson(value)}\n`
}

function encodeGoCanonicalJson(value: unknown): string {
  return JSON.stringify(value).replace(/[<>&\u2028\u2029]/gu, (character) => {
    const codePoint = character.codePointAt(0)
    if (codePoint === undefined) throw new Error('canonical JSON contains an invalid scalar')
    return `\\u${codePoint.toString(16).padStart(4, '0')}`
  })
}

function sha256(value: Uint8Array): string {
  return createHash('sha256').update(value).digest('hex')
}
