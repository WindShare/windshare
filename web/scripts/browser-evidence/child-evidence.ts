import { appendFile } from 'node:fs/promises'
import { isAbsolute } from 'node:path'

import { AttemptCollector, type LogicalAttempt } from './attempt-collector.ts'
import { parseAttemptEvidence, type AttemptEvidence } from './attempt-evidence.ts'
import { parseCapabilityEvidence, type CapabilityEvidence } from './capability.ts'
import {
  contractError,
  freezeRecord,
  requireBoolean,
  requireCheckoutSha,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
} from './contract/json.ts'
import { parseMainRouteEvidence, type MainRouteEvidence } from './route-evidence.ts'
import { ARTIFACT_KINDS, type ArtifactKind, type NativeInteropEvidence } from './result.ts'
import {
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from './run-policy.ts'
import { readStableRegularFileSnapshot } from './filesystem/snapshot.ts'
import { parseCanonicalJsonText } from './contract/strict-json.ts'
import { requirePortableRelativePath } from './filesystem/portable-path.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  DELIVERY_OUTCOMES,
  type BrowserEngine,
  type BrowserSuite,
  type DeliveryOutcome,
} from './vocabulary.ts'

export const CHILD_EVIDENCE_CONTEXT_ENV = 'WINDSHARE_BROWSER_EVIDENCE_CONTEXT' as const
export const CHILD_EVIDENCE_SCHEMA_VERSION = 2 as const
export const CHILD_EVIDENCE_MAXIMUM_EVENT_BYTES = 1_048_576 as const
export const CHILD_EVIDENCE_MAXIMUM_LOG_BYTES = 16_777_216 as const

const CHILD_EVENT_KINDS = Object.freeze([
  'capability',
  'attempt',
  'delivery',
  'route',
  'native-interop',
  'artifact',
  'page-crash',
  'target-crash',
  'page-error',
  'console',
  'browser-disconnected',
  'infrastructure-failure',
  'lifecycle-completed',
] as const)
const DIAGNOSTIC_MAXIMUM_BYTES = 4_096
const TEXT_ENCODER = new TextEncoder()
const TERMINAL_DELIVERY_OUTCOMES = DELIVERY_OUTCOMES.filter((outcome) => outcome !== 'not-started')
const TERMINAL_NATIVE_OUTCOMES = Object.freeze(['succeeded', 'failed'] as const)

type ChildEventKind = (typeof CHILD_EVENT_KINDS)[number]

export interface ChildEvidenceContext {
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
  readonly topologyProfilePath: string
  readonly topologyResolutionPath: string
  readonly evidencePath: string
  readonly artifactRoot: string
}

export interface ChildArtifactRegistration {
  readonly kind: ArtifactKind
  readonly relativePath: string
  readonly mediaType: string
}

export interface ChildDeliveryTerminal {
  readonly outcome: Exclude<DeliveryOutcome, 'not-started'>
  readonly evidence: unknown
}

export interface ChildNativeInteropTerminal {
  readonly outcome: 'succeeded' | 'failed'
  readonly evidence: NativeInteropEvidence
}

export interface ChildEvidenceCollection {
  readonly capabilityEvidence: CapabilityEvidence | null
  readonly attempts: readonly LogicalAttempt[]
  readonly delivery: ChildDeliveryTerminal | null
  readonly route: MainRouteEvidence | null
  readonly nativeInterop: ChildNativeInteropTerminal | null
  readonly artifactRegistrations: readonly ChildArtifactRegistration[]
  readonly pageCrashed: boolean
  readonly targetCrashed: boolean
  readonly unexpectedBrowserDisconnect: boolean
  readonly infrastructureFailure: boolean
  readonly lifecycleCompleted: boolean
  readonly diagnosticEventCount: number
  readonly integrityViolations: readonly string[]
}

interface ChildEventEnvelope {
  readonly schemaVersion: typeof CHILD_EVIDENCE_SCHEMA_VERSION
  readonly eventSequence: number
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
  readonly kind: ChildEventKind
  readonly payload: unknown
}

interface MutableCollection {
  capabilityEvidence: CapabilityEvidence | null
  delivery: ChildDeliveryTerminal | null
  route: MainRouteEvidence | null
  nativeInterop: ChildNativeInteropTerminal | null
  artifactRegistrations: ChildArtifactRegistration[]
  pageCrashed: boolean
  targetCrashed: boolean
  unexpectedBrowserDisconnect: boolean
  infrastructureFailure: boolean
  lifecycleCompleted: boolean
  diagnosticEventCount: number
  attemptEventCount: number
  violations: string[]
}

export function childEvidenceEnvironment(context: ChildEvidenceContext): Record<string, string> {
  return { [CHILD_EVIDENCE_CONTEXT_ENV]: JSON.stringify(parseChildEvidenceContext(context)) }
}

export function readChildEvidenceContext(
  environment: Readonly<Record<string, string | undefined>> = process.env,
): ChildEvidenceContext {
  const encoded = environment[CHILD_EVIDENCE_CONTEXT_ENV]
  if (encoded === undefined || encoded.length === 0) {
    throw new Error(`${CHILD_EVIDENCE_CONTEXT_ENV} is required for browser evidence emission`)
  }
  return parseChildEvidenceContext(parseCanonicalJsonText(encoded, CHILD_EVIDENCE_CONTEXT_ENV))
}

export function parseChildEvidenceContext(value: unknown): ChildEvidenceContext {
  const context = requireRecord(value, 'child evidence context')
  requireExactKeys(
    context,
    [
      'runId', 'operationId', 'scenario', 'runPolicy', 'suite', 'browser', 'sampleIndex', 'checkoutSha',
      'topologyProfileSha256', 'topologyResolutionSha256',
      'topologyProfilePath', 'topologyResolutionPath', 'evidencePath', 'artifactRoot',
    ],
    [],
    'child evidence context',
  )
  const runPolicy = parseBrowserRunPolicy(context.runPolicy, 'child evidence run policy')
  const sampleIndex = requireSafeInteger(
    context.sampleIndex,
    1,
    runPolicy.sampleCount,
    'child evidence sample index',
  )
  validatePolicySampleIndex(sampleIndex, runPolicy, 'child evidence sample index')
  return freezeRecord({
    runId: requirePortableToken(context.runId, 'child evidence run ID'),
    operationId: requirePortableToken(context.operationId, 'child evidence operation ID'),
    scenario: requirePortableToken(context.scenario, 'child evidence scenario'),
    runPolicy,
    suite: requireEnum(context.suite, BROWSER_SUITES, 'child evidence suite'),
    browser: requireEnum(context.browser, BROWSER_ENGINES, 'child evidence browser'),
    sampleIndex,
    checkoutSha: requireCheckoutSha(context.checkoutSha, 'child evidence checkout SHA'),
    topologyProfileSha256: requireSha256(
      context.topologyProfileSha256,
      'child evidence topology profile SHA-256',
    ),
    topologyResolutionSha256: requireSha256(
      context.topologyResolutionSha256,
      'child evidence topology resolution SHA-256',
    ),
    topologyProfilePath: requireAbsolutePath(context.topologyProfilePath, 'topology profile path'),
    topologyResolutionPath: requireAbsolutePath(context.topologyResolutionPath, 'topology resolution path'),
    evidencePath: requireAbsolutePath(context.evidencePath, 'child evidence path'),
    artifactRoot: requireAbsolutePath(context.artifactRoot, 'child artifact root'),
  })
}

export class ChildEvidenceReporter {
  readonly #context: ChildEvidenceContext
  #eventSequence = 0
  #queue: Promise<void> = Promise.resolve()
  #lifecycleCompleted = false

  constructor(context: ChildEvidenceContext = readChildEvidenceContext()) {
    this.#context = parseChildEvidenceContext(context)
  }

  get context(): ChildEvidenceContext {
    return this.#context
  }

  recordCapability(evidence: CapabilityEvidence): void {
    this.#emit('capability', parseCapabilityEvidence(evidence))
  }

  recordAttempt(evidence: AttemptEvidence): void {
    this.#emit('attempt', parseAttemptEvidence(evidence))
  }

  recordDelivery(outcome: Exclude<DeliveryOutcome, 'not-started'>, evidence: unknown): void {
    this.#emit('delivery', freezeRecord({
      outcome: requireEnum(outcome, TERMINAL_DELIVERY_OUTCOMES, 'child delivery outcome'),
      evidence,
    }))
  }

  recordRoute(evidence: MainRouteEvidence): void {
    const parsed = parseMainRouteEvidence(evidence)
    if (parsed === null) throw new Error('child route event cannot contain null evidence')
    this.#emit('route', parsed)
  }

  recordNativeInterop(outcome: 'succeeded' | 'failed', evidence: NativeInteropEvidence): void {
    this.#emit('native-interop', freezeRecord({
      outcome: requireEnum(outcome, TERMINAL_NATIVE_OUTCOMES, 'child native interop outcome'),
      evidence,
    }))
  }

  recordArtifact(registration: ChildArtifactRegistration): void {
    this.#emit('artifact', parseArtifactRegistration(registration))
  }

  recordPageCrash(message: unknown = 'Playwright page crash event'): void {
    this.#emitDiagnostic('page-crash', message)
  }

  recordTargetCrash(message: unknown): void {
    this.#emitDiagnostic('target-crash', message)
  }

  recordPageError(error: unknown): void {
    this.#emitDiagnostic('page-error', error)
  }

  recordConsole(level: string, message: unknown): void {
    this.#emit('console', freezeRecord({
      level: requirePortableToken(level, 'browser console level'),
      message: diagnosticMessage(message),
    }))
  }

  recordBrowserDisconnected(expected: boolean, message: unknown = 'browser disconnected'): void {
    this.#emit('browser-disconnected', freezeRecord({
      unexpected: !expected,
      message: diagnosticMessage(message),
    }))
  }

  recordInfrastructureFailure(message: unknown): void {
    this.#emitDiagnostic('infrastructure-failure', message)
  }

  completeLifecycle(): void {
    if (this.#lifecycleCompleted) throw new Error('child evidence lifecycle can only complete once')
    this.#lifecycleCompleted = true
    this.#emit('lifecycle-completed', Object.freeze({}))
  }

  async flush(): Promise<void> {
    await this.#queue
  }

  #emitDiagnostic(kind: Extract<ChildEventKind,
  'page-crash' | 'target-crash' | 'page-error' | 'infrastructure-failure'>, message: unknown): void {
    this.#emit(kind, freezeRecord({ message: diagnosticMessage(message) }))
  }

  #emit(kind: ChildEventKind, payload: unknown): void {
    if (this.#lifecycleCompleted && kind !== 'lifecycle-completed') {
      throw new Error('child evidence cannot be emitted after lifecycle completion')
    }
    const eventSequence = this.#eventSequence + 1
    const encoded = `${JSON.stringify({
      schemaVersion: CHILD_EVIDENCE_SCHEMA_VERSION,
      eventSequence,
      runId: this.#context.runId,
      operationId: this.#context.operationId,
      scenario: this.#context.scenario,
      suite: this.#context.suite,
      browser: this.#context.browser,
      sampleIndex: this.#context.sampleIndex,
      checkoutSha: this.#context.checkoutSha,
      topologyProfileSha256: this.#context.topologyProfileSha256,
      topologyResolutionSha256: this.#context.topologyResolutionSha256,
      kind,
      payload,
    })}\n`
    if (TEXT_ENCODER.encode(encoded).byteLength > CHILD_EVIDENCE_MAXIMUM_EVENT_BYTES) {
      throw new Error('child evidence event exceeds the frozen byte limit')
    }
    this.#eventSequence = eventSequence
    this.#queue = this.#queue.then(async () => appendFile(this.#context.evidencePath, encoded, 'utf8'))
  }
}

export interface PublicBrowserDiagnosticSink {
  readonly pageCrashed: (message?: unknown) => void
  readonly targetCrashed: (message: unknown) => void
  readonly pageError: (error: unknown) => void
  readonly console: (level: string, message: unknown) => void
  readonly browserDisconnected: (expected: boolean, message?: unknown) => void
  readonly infrastructureFailure: (message: unknown) => void
}

export function publicBrowserDiagnosticSink(reporter: ChildEvidenceReporter): PublicBrowserDiagnosticSink {
  return Object.freeze({
    pageCrashed: (message?: unknown) => reporter.recordPageCrash(message),
    targetCrashed: (message: unknown) => reporter.recordTargetCrash(message),
    pageError: (error: unknown) => reporter.recordPageError(error),
    console: (level: string, message: unknown) => reporter.recordConsole(level, message),
    browserDisconnected: (expected: boolean, message?: unknown) =>
      reporter.recordBrowserDisconnected(expected, message),
    infrastructureFailure: (message: unknown) => reporter.recordInfrastructureFailure(message),
  })
}

export async function collectChildEvidence(
  path: string,
  expectedContext: ChildEvidenceContext,
): Promise<ChildEvidenceCollection> {
  const context = parseChildEvidenceContext(expectedContext)
  const state = emptyCollection()
  const attemptCollector = new AttemptCollector()
  let encoded: Uint8Array
  try {
    encoded = (await readStableRegularFileSnapshot(
      path,
      CHILD_EVIDENCE_MAXIMUM_LOG_BYTES,
      'child evidence log',
    )).bytes
  } catch (cause) {
    state.violations.push(`child evidence log cannot be read: ${errorMessage(cause)}`)
    return finalizedCollection(state, attemptCollector)
  }
  const text = decodeEvidence(encoded, state.violations)
  if (text === null) return finalizedCollection(state, attemptCollector)
  const completeLines = completeEvidenceLines(text, state.violations)
  let expectedSequence = 1
  for (const line of completeLines) {
    try {
      if (TEXT_ENCODER.encode(line).byteLength > CHILD_EVIDENCE_MAXIMUM_EVENT_BYTES) {
        contractError('child evidence event exceeds the frozen byte limit')
      }
      const event = parseChildEvent(line, context, expectedSequence)
      if (state.lifecycleCompleted) contractError('child evidence follows lifecycle completion')
      applyChildEvent(state, attemptCollector, event, context.suite)
      expectedSequence += 1
    } catch (cause) {
      state.violations.push(errorMessage(cause))
      break
    }
  }
  return finalizedCollection(state, attemptCollector)
}

function parseChildEvent(
  encoded: string,
  context: ChildEvidenceContext,
  expectedSequence: number,
): ChildEventEnvelope {
  const event = requireRecord(parseCanonicalJsonText(encoded, 'child evidence event'), 'child evidence event')
  requireExactKeys(
    event,
    [
      'schemaVersion', 'eventSequence', 'runId', 'operationId', 'scenario', 'suite', 'browser', 'sampleIndex',
      'checkoutSha', 'topologyProfileSha256', 'topologyResolutionSha256', 'kind', 'payload',
    ],
    [],
    'child evidence event',
  )
  const parsed = freezeRecord({
    schemaVersion: requireLiteral(
      event.schemaVersion,
      CHILD_EVIDENCE_SCHEMA_VERSION,
      'child evidence schema version',
    ),
    eventSequence: requireSafeInteger(
      event.eventSequence,
      expectedSequence,
      expectedSequence,
      'child evidence event sequence',
    ),
    runId: requirePortableToken(event.runId, 'child evidence event run ID'),
    operationId: requirePortableToken(event.operationId, 'child evidence event operation ID'),
    scenario: requirePortableToken(event.scenario, 'child evidence event scenario'),
    suite: requireEnum(event.suite, BROWSER_SUITES, 'child evidence event suite'),
    browser: requireEnum(event.browser, BROWSER_ENGINES, 'child evidence event browser'),
    sampleIndex: validatePolicySampleIndex(requireSafeInteger(
      event.sampleIndex,
      1,
      context.runPolicy.sampleCount,
      'child evidence event sample index',
    ), context.runPolicy, 'child evidence event sample index'),
    checkoutSha: requireCheckoutSha(event.checkoutSha, 'child evidence event checkout SHA'),
    topologyProfileSha256: requireSha256(
      event.topologyProfileSha256,
      'child evidence event topology profile SHA-256',
    ),
    topologyResolutionSha256: requireSha256(
      event.topologyResolutionSha256,
      'child evidence event topology resolution SHA-256',
    ),
    kind: requireEnum(event.kind, CHILD_EVENT_KINDS, 'child evidence event kind'),
    payload: event.payload,
  })
  validateEventIdentity(parsed, context)
  return parsed
}

function applyChildEvent(
  state: MutableCollection,
  attemptCollector: AttemptCollector,
  event: ChildEventEnvelope,
  suite: BrowserSuite,
): void {
  if (event.kind === 'capability') {
    if (state.capabilityEvidence !== null) contractError('child capability evidence appears more than once')
    state.capabilityEvidence = parseCapabilityEvidence(event.payload)
  } else if (event.kind === 'artifact') {
    state.artifactRegistrations.push(parseArtifactRegistration(event.payload))
  } else if (event.kind === 'lifecycle-completed') {
    requireEmptyPayload(event.payload, 'child lifecycle completion')
    state.lifecycleCompleted = true
  } else if (suite === 'main' && isMainAuthorityEvent(event.kind)) {
    applyMainAuthorityEvent(state, attemptCollector, event)
  } else if (suite === 'pion' && event.kind === 'native-interop') {
    if (state.nativeInterop !== null) contractError('child native interop terminal appears more than once')
    state.nativeInterop = parseNativeTerminal(event.payload)
  } else if (isSuiteAuthorityEvent(event.kind)) {
    requireSuite(suite, suite === 'main' ? 'pion' : 'main', event.kind)
  } else {
    applyDiagnosticEvent(state, event.kind, event.payload)
  }
}

function applyMainAuthorityEvent(
  state: MutableCollection,
  attemptCollector: AttemptCollector,
  event: ChildEventEnvelope,
): void {
  if (event.kind === 'attempt') {
    attemptCollector.ingest(event.payload)
    state.attemptEventCount += 1
  } else if (event.kind === 'delivery') {
    if (state.delivery !== null) contractError('child delivery terminal appears more than once')
    state.delivery = parseDeliveryTerminal(event.payload)
  } else if (event.kind === 'route') {
    if (state.route !== null) contractError('child route evidence appears more than once')
    state.route = parseRequiredRoute(event.payload)
  }
}

function isMainAuthorityEvent(kind: ChildEventKind): boolean {
  return kind === 'attempt' || kind === 'delivery' || kind === 'route'
}

function isSuiteAuthorityEvent(kind: ChildEventKind): boolean {
  return isMainAuthorityEvent(kind) || kind === 'native-interop'
}

function applyDiagnosticEvent(state: MutableCollection, kind: ChildEventKind, payload: unknown): void {
  state.diagnosticEventCount += 1
  if (kind === 'console') {
    parseConsoleDiagnostic(payload)
    return
  }
  if (kind === 'browser-disconnected') {
    const diagnostic = parseDisconnectDiagnostic(payload)
    state.unexpectedBrowserDisconnect ||= diagnostic.unexpected
    return
  }
  parseMessageDiagnostic(payload, `child ${kind} diagnostic`)
  if (kind === 'page-crash') state.pageCrashed = true
  else if (kind === 'target-crash') state.targetCrashed = true
  else if (kind === 'infrastructure-failure') state.infrastructureFailure = true
}

function parseDeliveryTerminal(value: unknown): ChildDeliveryTerminal {
  const terminal = requireRecord(value, 'child delivery terminal')
  requireExactKeys(terminal, ['outcome', 'evidence'], [], 'child delivery terminal')
  return freezeRecord({
    outcome: requireEnum(terminal.outcome, TERMINAL_DELIVERY_OUTCOMES, 'child delivery outcome'),
    evidence: terminal.evidence,
  })
}

function parseNativeTerminal(value: unknown): ChildNativeInteropTerminal {
  const terminal = requireRecord(value, 'child native interop terminal')
  requireExactKeys(terminal, ['outcome', 'evidence'], [], 'child native interop terminal')
  return freezeRecord({
    outcome: requireEnum(terminal.outcome, TERMINAL_NATIVE_OUTCOMES, 'child native interop outcome'),
    evidence: terminal.evidence as NativeInteropEvidence,
  })
}

function parseRequiredRoute(value: unknown): MainRouteEvidence {
  const route = parseMainRouteEvidence(value)
  if (route === null) contractError('child route event cannot contain null evidence')
  return route
}

function parseArtifactRegistration(value: unknown): ChildArtifactRegistration {
  const artifact = requireRecord(value, 'child artifact registration')
  requireExactKeys(artifact, ['kind', 'relativePath', 'mediaType'], [], 'child artifact registration')
  return freezeRecord({
    kind: requireEnum(artifact.kind, ARTIFACT_KINDS, 'child artifact kind'),
    relativePath: requireRelativePath(artifact.relativePath, 'child artifact relative path'),
    mediaType: requireString(artifact.mediaType, 'child artifact media type', 128),
  })
}

function parseConsoleDiagnostic(value: unknown): void {
  const diagnostic = requireRecord(value, 'child console diagnostic')
  requireExactKeys(diagnostic, ['level', 'message'], [], 'child console diagnostic')
  requirePortableToken(diagnostic.level, 'child console level')
  requireString(diagnostic.message, 'child console message', DIAGNOSTIC_MAXIMUM_BYTES)
}

function parseDisconnectDiagnostic(value: unknown): { readonly unexpected: boolean } {
  const diagnostic = requireRecord(value, 'child browser disconnect diagnostic')
  requireExactKeys(diagnostic, ['unexpected', 'message'], [], 'child browser disconnect diagnostic')
  requireString(diagnostic.message, 'child browser disconnect message', DIAGNOSTIC_MAXIMUM_BYTES)
  return freezeRecord({
    unexpected: requireBoolean(diagnostic.unexpected, 'child unexpected browser disconnect'),
  })
}

function parseMessageDiagnostic(value: unknown, label: string): void {
  const diagnostic = requireRecord(value, label)
  requireExactKeys(diagnostic, ['message'], [], label)
  requireString(diagnostic.message, `${label} message`, DIAGNOSTIC_MAXIMUM_BYTES)
}

function finalizedCollection(
  state: MutableCollection,
  attemptCollector: AttemptCollector,
): ChildEvidenceCollection {
  const finalizedAttempts = attemptCollector.finalizePreservingCompleted()
  const attempts = finalizedAttempts.attempts
  if (state.attemptEventCount > 0) {
    state.violations.push(...finalizedAttempts.integrityViolations)
  }
  return freezeRecord({
    capabilityEvidence: state.capabilityEvidence,
    attempts,
    delivery: state.delivery,
    route: state.route,
    nativeInterop: state.nativeInterop,
    artifactRegistrations: Object.freeze([...state.artifactRegistrations]),
    pageCrashed: state.pageCrashed,
    targetCrashed: state.targetCrashed,
    unexpectedBrowserDisconnect: state.unexpectedBrowserDisconnect,
    infrastructureFailure: state.infrastructureFailure,
    lifecycleCompleted: state.lifecycleCompleted,
    diagnosticEventCount: state.diagnosticEventCount,
    integrityViolations: Object.freeze([...new Set(state.violations)].sort(compareStrings)),
  })
}

function emptyCollection(): MutableCollection {
  return {
    capabilityEvidence: null,
    delivery: null,
    route: null,
    nativeInterop: null,
    artifactRegistrations: [],
    pageCrashed: false,
    targetCrashed: false,
    unexpectedBrowserDisconnect: false,
    infrastructureFailure: false,
    lifecycleCompleted: false,
    diagnosticEventCount: 0,
    attemptEventCount: 0,
    violations: [],
  }
}

function completeEvidenceLines(encoded: string, violations: string[]): readonly string[] {
  if (encoded.length === 0) return []
  if (!encoded.endsWith('\n')) violations.push('child evidence log ends with a truncated event')
  const finalNewline = encoded.lastIndexOf('\n')
  if (finalNewline < 0) return []
  const lines = encoded.slice(0, finalNewline).split('\n')
  if (lines.some((line) => line.length === 0)) {
    violations.push('child evidence log contains an empty event line')
    return lines.slice(0, lines.findIndex((line) => line.length === 0))
  }
  return lines
}

function decodeEvidence(encoded: Uint8Array, violations: string[]): string | null {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(encoded)
  } catch {
    violations.push('child evidence log is not valid UTF-8')
    return null
  }
}

function validateEventIdentity(event: ChildEventEnvelope, context: ChildEvidenceContext): void {
  if (
    event.runId !== context.runId || event.operationId !== context.operationId ||
    event.scenario !== context.scenario || event.suite !== context.suite ||
    event.browser !== context.browser || event.sampleIndex !== context.sampleIndex ||
    event.checkoutSha !== context.checkoutSha ||
    event.topologyProfileSha256 !== context.topologyProfileSha256 ||
    event.topologyResolutionSha256 !== context.topologyResolutionSha256
  ) contractError('child evidence event identity differs from its parent runner context')
}

function requireSuite(actual: BrowserSuite, expected: BrowserSuite, kind: string): void {
  if (actual !== expected) contractError(`${kind} evidence is not valid for the ${actual} suite`)
}

function requireEmptyPayload(value: unknown, label: string): void {
  const payload = requireRecord(value, label)
  requireExactKeys(payload, [], [], label)
}

function requirePortableToken(value: unknown, label: string): string {
  const token = requireString(value, label, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(token)) contractError(`${label} contains non-portable characters`)
  return token
}

function requireAbsolutePath(value: unknown, label: string): string {
  const path = requireString(value, label, 4_096)
  if (!isAbsolute(path)) contractError(`${label} must be absolute`)
  return path
}

function requireRelativePath(value: unknown, label: string): string {
  try {
    return requirePortableRelativePath(value, label)
  } catch (cause) {
    contractError(cause instanceof Error ? cause.message : String(cause))
  }
}

function diagnosticMessage(value: unknown): string {
  const source = value instanceof Error ? (value.stack ?? value.message) : String(value ?? 'no diagnostic detail')
  const normalized = source.normalize('NFC') || 'no diagnostic detail'
  let result = ''
  let bytes = 0
  for (const character of normalized) {
    const characterBytes = TEXT_ENCODER.encode(character).byteLength
    if (bytes + characterBytes > DIAGNOSTIC_MAXIMUM_BYTES) break
    result += character
    bytes += characterBytes
  }
  return result || 'no diagnostic detail'
}

function errorMessage(cause: unknown): string {
  return diagnosticMessage(cause)
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
