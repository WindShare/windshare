import type { BigIntStats } from 'node:fs'
import { lstat, open } from 'node:fs/promises'
import type { FileHandle } from 'node:fs/promises'
import { isAbsolute, join, resolve } from 'node:path'

import { artifactManifestSha256, sha256Bytes } from '../manifest.ts'
import {
  sealGuardUploadSuite,
} from '../sealed-suite.ts'
import {
  requireGuardUploadDirectoryPublisher,
} from '../directory-publisher.ts'
import {
  requireVerifiedProcessSettlementSet,
  verifyProcessSettlementAttestations,
} from '../settlement-receipt.ts'
import { GuardExecutionLease } from '../../execution/guard-execution-lease.ts'
import {
  ARTIFACT_GUARD_SCHEMA_VERSION,
  GUARD_MAXIMUM_ARTIFACT_FILES,
  GUARD_MAXIMUM_ARTIFACT_FILE_BYTES,
  GUARD_MAXIMUM_TOTAL_ARTIFACT_BYTES,
  GUARD_MAXIMUM_ARCHIVE_BYTES,
  GUARD_MAXIMUM_ARCHIVE_ENTRIES,
  GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH,
  GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES,
  parseArtifactGuardResult,
  validateArtifactGuardForSample,
  type ArtifactGuardResult,
  type GuardMatchEvidence,
} from '../guard-result.ts'
import type { ArtifactIndexEntry, BrowserSampleResult } from '../../result.ts'
import { assertBrowserRunPolicyEqual, parseBrowserRunPolicy } from '../../run-policy.ts'
import { parseCanonicalJsonText } from '../../contract/strict-json.ts'
import { BROWSER_ENGINES } from '../../vocabulary.ts'
import {
  GuardFailure,
  type ArtifactGuardTraceEvent,
  type ArtifactGuardTraceSink,
  type GuardArtifactSuiteOptions,
  type GuardArtifactSuiteResult,
  type ScanSampleArtifactsOptions,
  type ScanState,
} from './contract.ts'
import {
  assertControlPlaneSecretFree,
  parseExplicitSecrets,
  sameFileIdentity,
  scanArtifact,
} from './archive-scanner.ts'

interface HeldArtifactRoot {
  readonly path: string
  readonly handle: FileHandle
  readonly identity: BigIntStats
}

/**
 * A suite is the upload unit because workflow artifacts are suite-owned. Guard
 * failures stay sample-specific, while the upload authority is all-or-nothing
 * so a partial browser/sample set can never reach the verdict job.
 */
export async function guardArtifactSuite(
  options: GuardArtifactSuiteOptions,
): Promise<GuardArtifactSuiteResult> {
  const executionLease = options.executionLease ?? GuardExecutionLease.start()
  executionLease.throwIfPrimaryExpired('artifact guard suite')
  validateSuiteInputs(options)
  const settlementSamples = options.samples.map((input) => Object.freeze({
    sample: input.sample,
    resultBytes: input.sampleResultBytes,
    commandSha256: input.commandSha256,
  }))
  const settlement = verifyProcessSettlementAttestations({
    trust: options.settlementTrust,
    samples: settlementSamples,
    attestations: options.samples.map(({ settlementAttestation }) => settlementAttestation),
  })
  requireVerifiedProcessSettlementSet(settlement, {
    invocationId: options.settlementTrust.invocationId,
    samples: settlementSamples,
  })
  const guards: ArtifactGuardResult[] = []
  for (const input of options.samples) {
    guards.push(await scanSampleArtifacts({
      sample: input.sample,
      sampleResultBytes: input.sampleResultBytes,
      artifactRoot: input.artifactRoot,
      explicitSecrets: options.explicitSecrets,
      ...(options.hooks?.beforeArtifactScan === undefined
        ? {}
        : {
            beforeArtifactScan: (artifact: ArtifactIndexEntry) =>
              options.hooks!.beforeArtifactScan!(input.sample, artifact),
          }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      executionLease,
    }))
  }
  const frozenGuards = Object.freeze(guards)
  if (frozenGuards.some(({ guardOutcome }) => guardOutcome !== 'passed')) {
    emitSuiteTrace(options, 'suite-upload-blocked', {
      sampleCount: frozenGuards.length,
      nonPassedGuardCount: frozenGuards.filter(({ guardOutcome }) => guardOutcome !== 'passed').length,
    })
    return Object.freeze({ guards: frozenGuards, upload: null })
  }
  try {
    const guardsBySlot = new Map(frozenGuards.map((guard) => [guardSlot(guard), guard]))
    const upload = await sealGuardUploadSuite({
      uploadParent: options.uploadParent,
      runId: options.runId,
      runPolicy: options.runPolicy,
      suite: options.suite,
      checkoutSha: options.checkoutSha,
      topology: options.topology,
      settlement,
      settlementInvocationId: options.settlementTrust.invocationId,
      directoryPublisher: options.directoryPublisher,
      executionLease,
      samples: options.samples.map((input) => {
        const guard = guardsBySlot.get(guardSlot(input.sample))
        if (guard === undefined) throw new Error('guard suite lost a sample guard before sealing')
        return Object.freeze({
          sample: input.sample,
          sampleResultBytes: input.sampleResultBytes,
          artifactRoot: input.artifactRoot,
          commandSha256: input.commandSha256,
          guard,
        })
      }),
      ...(options.hooks?.upload === undefined ? {} : { hooks: options.hooks.upload }),
    })
    emitSuiteTrace(options, 'suite-upload-sealed', {
      sampleCount: frozenGuards.length,
      uploadManifestSha256: upload.manifestSha256,
    })
    return Object.freeze({ guards: frozenGuards, upload })
  } catch (cause) {
    const failed = Object.freeze(frozenGuards.map((guard) =>
      failedGuardAfterSuiteSeal(guard, cause)))
    emitSuiteTrace(options, 'suite-upload-failed', {
      sampleCount: failed.length,
      failure: boundedMessage(cause),
    })
    return Object.freeze({ guards: failed, upload: null })
  }
}

export async function scanSampleArtifacts(
  options: ScanSampleArtifactsOptions,
): Promise<ArtifactGuardResult> {
  const executionLease = options.executionLease ?? GuardExecutionLease.start()
  const scanSignal = executionLease.primarySignal('artifact guard scan')
  const checked = [...options.sample.artifacts].sort(compareArtifacts)
  const sampleResultSha256 = sha256Bytes(options.sampleResultBytes)
  const manifestSha256 = artifactManifestSha256(checked)
  const state = emptyScanState()
  emitGuardTrace(options, 'scan-started', { artifactCount: checked.length, manifestSha256 })
  let result: ArtifactGuardResult
  let artifactRoot: HeldArtifactRoot | undefined
  try {
    scanSignal.throwIfAborted()
    validateSampleResultSnapshot(options.sampleResultBytes, options.sample)
    validateArtifactSizeAuthority(checked)
    const explicitSecretBytes = parseExplicitSecrets(options.explicitSecrets)
    assertControlPlaneSecretFree(options.sampleResultBytes, checked, explicitSecretBytes)
    artifactRoot = await holdArtifactRoot(options.artifactRoot, scanSignal)
    for (const artifact of checked) {
      await executionLease.runPrimary('artifact guard pre-scan hook', async (signal) => {
        await options.beforeArtifactScan?.(artifact)
        signal.throwIfAborted()
      })
      scanSignal.throwIfAborted()
      await assertHeldArtifactRoot(artifactRoot)
      await assertArtifactAncestry(artifactRoot.path, artifact.relativePath)
      await scanArtifact(artifact, options.artifactRoot, explicitSecretBytes, state, scanSignal)
      await assertArtifactAncestry(artifactRoot.path, artifact.relativePath)
    }
    await assertHeldArtifactRoot(artifactRoot)
    result = completedGuardResult(
      options.sample,
      sampleResultSha256,
      manifestSha256,
      checked,
      state,
      uniqueMatchedArtifactIds(state.matches),
    )
  } catch (cause) {
    result = failedGuardResult(
      options.sample,
      sampleResultSha256,
      manifestSha256,
      checked,
      state,
      guardFailure(cause),
    )
  } finally {
    await artifactRoot?.handle.close().catch(() => undefined)
  }
  const parsed = parseArtifactGuardResult(result)
  if (parsed.guardOutcome === 'passed') {
    validateArtifactGuardForSample(parsed, options.sample, sampleResultSha256)
  }
  emitGuardTrace(options, parsed.guardOutcome === 'failed' ? 'scan-failed' : 'scan-completed', {
    guardOutcome: parsed.guardOutcome,
    scannedFileCount: parsed.scanEvidence.scannedFileCount,
    scannedArchiveEntryCount: parsed.scanEvidence.scannedArchiveEntryCount,
    observedArchiveBytes: parsed.scanEvidence.observedArchiveBytes,
    expandedArchiveBytes: parsed.scanEvidence.expandedArchiveBytes,
    quarantinedArtifactCount: parsed.quarantinedArtifactIds.length,
    failureCode: parsed.failureCode ?? null,
  })
  return parsed
}

function validateSuiteInputs(options: GuardArtifactSuiteOptions): void {
  requireGuardUploadDirectoryPublisher(options.directoryPublisher)
  const policy = parseBrowserRunPolicy(options.runPolicy, 'guard suite run policy')
  if (!/^[A-Za-z0-9._-]{1,128}$/u.test(options.runId)) {
    throw new Error('guard suite run ID is not portable')
  }
  if (!/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/u.test(options.checkoutSha)) {
    throw new Error('guard suite checkout SHA is not canonical')
  }
  if (options.suite !== 'main' && options.suite !== 'pion') {
    throw new Error('guard suite identity is invalid')
  }
  const slots = new Set<string>()
  for (const input of options.samples) {
    if (
      input.sample.resultStatus === 'provisional' ||
      input.sample.runId !== options.runId ||
      input.sample.suite !== options.suite ||
      input.sample.checkoutSha !== options.checkoutSha
    ) throw new Error('guard suite sample does not match the suite identity')
    assertBrowserRunPolicyEqual(input.sample.runPolicy, policy, 'guard suite run policy')
    const slot = guardSlot(input.sample)
    if (slots.has(slot)) throw new Error('guard suite repeats a browser/sample slot')
    slots.add(slot)
    requireCanonicalArtifactRoot(input.artifactRoot)
  }
  const expected = BROWSER_ENGINES.flatMap((browser) =>
    Array.from({ length: policy.sampleCount }, (_, index) => `${browser}/${index + 1}`))
  if (slots.size !== expected.length || expected.some((slot) => !slots.has(slot))) {
    throw new Error('guard suite does not contain every policy browser/sample slot exactly once')
  }
}

function validateArtifactSizeAuthority(artifacts: readonly ArtifactIndexEntry[]): void {
  if (artifacts.length > GUARD_MAXIMUM_ARTIFACT_FILES) {
    throw new GuardFailure('contract', 'artifact count exceeds the frozen guard limit')
  }
  let totalBytes = 0
  for (const artifact of artifacts) {
    if (artifact.byteLength > GUARD_MAXIMUM_ARTIFACT_FILE_BYTES) {
      throw new GuardFailure(
        'contract',
        `artifact ${artifact.relativePath} exceeds the frozen per-file guard limit`,
      )
    }
    totalBytes += artifact.byteLength
    if (!Number.isSafeInteger(totalBytes) || totalBytes > GUARD_MAXIMUM_TOTAL_ARTIFACT_BYTES) {
      throw new GuardFailure('contract', 'artifact bytes exceed the frozen total guard limit')
    }
  }
}

async function holdArtifactRoot(path: string, signal: AbortSignal): Promise<HeldArtifactRoot> {
  signal.throwIfAborted()
  const canonicalPath = requireCanonicalArtifactRoot(path)
  const namedBefore = await lstat(canonicalPath, { bigint: true })
  requireArtifactDirectory(namedBefore, 'artifact root')
  const handle = await open(canonicalPath, 'r')
  try {
    const identity = await handle.stat({ bigint: true })
    const namedAfter = await lstat(canonicalPath, { bigint: true })
    requireArtifactDirectory(identity, 'artifact root')
    requireArtifactDirectory(namedAfter, 'artifact root')
    if (!sameFileIdentity(namedBefore, identity) || !sameFileIdentity(identity, namedAfter)) {
      throw new GuardFailure('contract', 'artifact root changed while its authority was acquired')
    }
    return Object.freeze({ path: canonicalPath, handle, identity })
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

async function assertHeldArtifactRoot(root: HeldArtifactRoot): Promise<void> {
  const [opened, named] = await Promise.all([
    root.handle.stat({ bigint: true }),
    lstat(root.path, { bigint: true }),
  ])
  requireArtifactDirectory(opened, 'artifact root')
  requireArtifactDirectory(named, 'artifact root')
  if (!sameFileIdentity(root.identity, opened) || !sameFileIdentity(opened, named)) {
    throw new GuardFailure('contract', 'artifact root no longer names its owner-held directory')
  }
}

async function assertArtifactAncestry(root: string, relativePath: string): Promise<void> {
  const segments = relativePath.split('/')
  let current = root
  for (const segment of segments.slice(0, -1)) {
    current = join(current, segment)
    requireArtifactDirectory(await lstat(current, { bigint: true }), `artifact directory ${segment}`)
  }
}

function requireArtifactDirectory(metadata: BigIntStats, label: string): void {
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new GuardFailure('contract', `${label} is not a regular no-follow directory`)
  }
}

function requireCanonicalArtifactRoot(path: string): string {
  if (!isAbsolute(path) || resolve(path) !== path) {
    throw new GuardFailure('contract', 'artifact root must be canonical and absolute')
  }
  return path
}

function validateSampleResultSnapshot(encoded: Uint8Array, sample: BrowserSampleResult): void {
  let decoded: string
  try {
    decoded = new TextDecoder('utf-8', { fatal: true }).decode(encoded)
  } catch (cause) {
    throw new GuardFailure('contract', 'browser sample result snapshot is not valid UTF-8', { cause })
  }
  let parsed: unknown
  try {
    parsed = parseCanonicalJsonText(decoded, 'browser sample result snapshot')
  } catch (cause) {
    throw new GuardFailure('contract', 'browser sample result snapshot violates its canonical JSON contract', { cause })
  }
  if (JSON.stringify(parsed) !== JSON.stringify(sample)) {
    throw new GuardFailure('contract', 'browser sample result object differs from its immutable byte snapshot')
  }
}

function completedGuardResult(
  sample: BrowserSampleResult,
  sampleResultSha256: string,
  manifestSha256: string,
  checked: readonly ArtifactIndexEntry[],
  state: ScanState,
  quarantinedIds: readonly string[],
): ArtifactGuardResult {
  const quarantined = new Set(quarantinedIds)
  const uploadable = checked.map(({ artifactId }) => artifactId).filter((id) => !quarantined.has(id))
  return {
    ...guardIdentity(sample, sampleResultSha256, manifestSha256),
    guardOutcome: quarantinedIds.length === 0 ? 'passed' : 'quarantined',
    scanEvidence: scanEvidence(state, 'completed'),
    checkedArtifactIds: checked.map(({ artifactId }) => artifactId),
    uploadableArtifactIds: uploadable,
    quarantinedArtifactIds: quarantinedIds,
    matches: normalizedMatches(state.matches),
  }
}

function failedGuardResult(
  sample: BrowserSampleResult,
  sampleResultSha256: string,
  manifestSha256: string,
  checked: readonly ArtifactIndexEntry[],
  state: ScanState,
  failure: GuardFailure,
): ArtifactGuardResult {
  const checkedIds = checked.map(({ artifactId }) => artifactId)
  return {
    ...guardIdentity(sample, sampleResultSha256, manifestSha256),
    guardOutcome: 'failed',
    scanEvidence: scanEvidence(state, 'failed'),
    checkedArtifactIds: checkedIds,
    uploadableArtifactIds: [],
    quarantinedArtifactIds: checkedIds,
    matches: normalizedMatches(state.matches),
    failureCode: failure.code,
    failureMessage: boundedMessage(failure.message),
  }
}

function guardIdentity(
  sample: BrowserSampleResult,
  sampleResultSha256: string,
  manifestSha256: string,
) {
  return {
    schemaVersion: ARTIFACT_GUARD_SCHEMA_VERSION,
    runId: sample.runId,
    runPolicy: sample.runPolicy,
    suite: sample.suite,
    browser: sample.browser,
    sampleIndex: sample.sampleIndex,
    checkoutSha: sample.checkoutSha,
    sampleResultSha256,
    artifactManifestSha256: manifestSha256,
  }
}

function scanEvidence(state: ScanState, terminal: 'completed' | 'failed') {
  return {
    terminal,
    scannedFileCount: state.scannedFileCount,
    scannedArchiveEntryCount: state.scannedArchiveEntryCount,
    observedArchiveBytes: state.observedArchiveBytes,
    expandedArchiveBytes: state.expandedArchiveBytes,
    observedMaximumArchiveDepth: state.observedMaximumArchiveDepth,
    maximumArchiveBytes: GUARD_MAXIMUM_ARCHIVE_BYTES,
    maximumArchiveEntries: GUARD_MAXIMUM_ARCHIVE_ENTRIES,
    maximumExpandedArchiveBytes: GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES,
    maximumArchiveNestingDepth: GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH,
  }
}

function guardScanOperationId(sample: BrowserSampleResult): string {
  return `${sample.runId}/${sample.suite}/${sample.browser}/${sample.sampleIndex}/guard-scan`
}

function emitGuardTrace(
  options: {
    readonly sample: BrowserSampleResult
    readonly trace?: ArtifactGuardTraceSink
  },
  milestone: string,
  context: ArtifactGuardTraceEvent['context'],
): void {
  const event = Object.freeze({
    component: 'artifact-guard' as const,
    operationId: guardScanOperationId(options.sample),
    milestone,
    context: Object.freeze({ ...context }),
  })
  try {
    const sink = options.trace ?? defaultGuardTraceSink
    sink(event)
  } catch {
    // Diagnostics must not become authority over whether attachments are safe.
  }
}

function emitSuiteTrace(
  options: GuardArtifactSuiteOptions,
  milestone: string,
  context: ArtifactGuardTraceEvent['context'],
): void {
  const event = Object.freeze({
    component: 'artifact-guard' as const,
    operationId: `${options.runId}/${options.suite}/guard-suite`,
    milestone,
    context: Object.freeze({ ...context }),
  })
  try {
    const sink = options.trace ?? defaultGuardTraceSink
    sink(event)
  } catch {
    // Diagnostics must not become authority over whether attachments are safe.
  }
}

function defaultGuardTraceSink(event: ArtifactGuardTraceEvent): void {
  process.stderr.write(`${JSON.stringify(event)}\n`)
}

function uniqueMatchedArtifactIds(matches: readonly GuardMatchEvidence[]): readonly string[] {
  return Object.freeze([...new Set(matches.map(({ artifactId }) => artifactId))].sort(compareStrings))
}

function normalizedMatches(matches: readonly GuardMatchEvidence[]): readonly GuardMatchEvidence[] {
  return Object.freeze([...new Map(matches.map((match) => [matchKey(match), match])).values()]
    .sort((left, right) => compareStrings(matchKey(left), matchKey(right))))
}

function matchKey(match: GuardMatchEvidence): string {
  return `${match.artifactId}\0${match.location}\0${match.archiveEntryPath ?? ''}\0${match.detector}`
}

function emptyScanState(): ScanState {
  return {
    scannedFileCount: 0,
    scannedArchiveEntryCount: 0,
    observedArchiveBytes: 0,
    expandedArchiveBytes: 0,
    observedMaximumArchiveDepth: 0,
    matches: [],
  }
}

function failedGuardAfterSuiteSeal(
  guard: ArtifactGuardResult,
  cause: unknown,
): ArtifactGuardResult {
  return parseArtifactGuardResult({
    schemaVersion: guard.schemaVersion,
    runId: guard.runId,
    runPolicy: guard.runPolicy,
    suite: guard.suite,
    browser: guard.browser,
    sampleIndex: guard.sampleIndex,
    checkoutSha: guard.checkoutSha,
    sampleResultSha256: guard.sampleResultSha256,
    artifactManifestSha256: guard.artifactManifestSha256,
    guardOutcome: 'failed',
    scanEvidence: { ...guard.scanEvidence, terminal: 'failed' },
    checkedArtifactIds: guard.checkedArtifactIds,
    uploadableArtifactIds: [],
    quarantinedArtifactIds: guard.checkedArtifactIds,
    matches: guard.matches,
    failureCode: 'unexpected',
    failureMessage: boundedMessage(`guard suite upload sealing failed: ${boundedMessage(cause)}`),
  })
}

function guardSlot(value: Pick<BrowserSampleResult, 'browser' | 'sampleIndex'>): string {
  return `${value.browser}/${value.sampleIndex}`
}

function guardFailure(cause: unknown): GuardFailure {
  if (cause instanceof GuardFailure) return cause
  return new GuardFailure('scanner-crashed', boundedMessage(cause), { cause })
}

function boundedMessage(value: unknown): string {
  const source = value instanceof Error ? value.message : String(value)
  const normalized = source.normalize('NFC') || 'artifact guard failed'
  let result = ''
  let bytes = 0
  for (const character of normalized) {
    const width = Buffer.byteLength(character, 'utf8')
    if (bytes + width > 512) break
    result += character
    bytes += width
  }
  return result || 'artifact guard failed'
}

function compareArtifacts(left: ArtifactIndexEntry, right: ArtifactIndexEntry): number {
  return compareStrings(left.artifactId, right.artifactId)
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
