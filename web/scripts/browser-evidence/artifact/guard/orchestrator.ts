import type { BigIntStats } from 'node:fs'
import { lstat, open, writeFile } from 'node:fs/promises'
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
  ArtifactGuardRecordedError,
  GuardFailure,
  isOwnedGuardFailure,
  type ArtifactGuardScanFaultCut,
  type ArtifactGuardTraceIdentity,
  type GuardArtifactSuiteExecution,
  type GuardArtifactSuiteOptions,
  type GuardArtifactSuiteOutcome,
  type GuardArtifactSuiteResult,
  type ScanSampleArtifactsExecution,
  type ScanSampleArtifactsOptions,
  type ScanState,
} from './contract.ts'
import {
  ArtifactGuardTraceJournal,
  requireCompleteArtifactGuardTrace,
} from './trace-journal.ts'
import {
  assertControlPlaneSecretFree,
  parseExplicitSecrets,
  sameFileIdentity,
  scanArtifact,
} from './archive-scanner.ts'

const EMPTY_SHA256 = '0'.repeat(64)
const MAXIMUM_SCAN_FAULT_REPLACEMENT_BYTES = 65_536
const GUARD_SUITE_FAILURE_MESSAGE = 'artifact guard suite failed'
const GUARD_UPLOAD_FAILURE_MESSAGE = 'guard suite upload sealing failed'
const ARTIFACT_ROOT_CLEANUP_FAILURE_MESSAGE = 'artifact root cleanup failed'
const ARTIFACT_SCANNER_FAILURE_MESSAGE = 'artifact scanner failed'

interface HeldArtifactRoot {
  readonly path: string
  readonly handle: FileHandle
  readonly identity: BigIntStats
}

/**
 * A suite is the upload unit because workflow artifacts are suite-owned. The
 * convenience boundary returns its complete journal; callers cannot accidentally
 * accept an upload while discarding the evidence that authorized it.
 */
export async function guardArtifactSuite(
  options: GuardArtifactSuiteOptions,
): Promise<GuardArtifactSuiteResult> {
  const execution = startGuardArtifactSuite(options)
  try {
    const outcome = await execution.result
    const traces = execution.traces.snapshot()
    requireCompleteArtifactGuardTrace(traces)
    return Object.freeze({ ...outcome, traces })
  } catch (cause) {
    const traces = execution.traces.snapshot()
    throw new ArtifactGuardRecordedError(GUARD_SUITE_FAILURE_MESSAGE, traces, cause)
  }
}

export function startGuardArtifactSuite(
  options: GuardArtifactSuiteOptions,
): GuardArtifactSuiteExecution {
  const journal = new ArtifactGuardTraceJournal()
  return Object.freeze({
    result: runGuardArtifactSuite(options, journal),
    traces: journal.view,
  })
}

export function startScanSampleArtifacts(
  options: ScanSampleArtifactsOptions,
): ScanSampleArtifactsExecution {
  const journal = new ArtifactGuardTraceJournal()
  return Object.freeze({
    result: runSampleArtifactScan(options, journal),
    traces: journal.view,
  })
}

async function runGuardArtifactSuite(
  options: GuardArtifactSuiteOptions,
  journal: ArtifactGuardTraceJournal,
): Promise<GuardArtifactSuiteOutcome> {
  const identity = suiteTraceIdentity(options)
  journal.start(identity, { sampleCount: options.samples.length })
  let terminalOutcome: 'succeeded' | 'failed' | 'blocked' = 'failed'
  let cleanupOutcome: 'completed' | 'failed' | 'not-required' = 'not-required'
  let lastMilestone = 'suite-started'
  let outcome: GuardArtifactSuiteOutcome | undefined
  let failure: unknown
  try {
    const executionLease = options.executionLease ?? GuardExecutionLease.start()
    executionLease.throwIfPrimaryExpired('artifact guard suite')
    validateSuiteInputs(options)
    lastMilestone = 'suite-input-validated'
    journal.progress(identity, lastMilestone, 'succeeded', { sampleCount: options.samples.length })
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
    lastMilestone = 'suite-settlement-verified'
    journal.progress(identity, lastMilestone, 'succeeded', { sampleCount: options.samples.length })
    cleanupOutcome = 'completed'
    const guards: ArtifactGuardResult[] = []
    for (const input of options.samples) {
      const faultCut = scanFaultForSample(options, input.sample)
      const execution = startScanSampleArtifacts({
        sample: input.sample,
        sampleResultBytes: input.sampleResultBytes,
        artifactRoot: input.artifactRoot,
        explicitSecrets: options.explicitSecrets,
        ...(faultCut === undefined ? {} : { faultCut }),
        executionLease,
      })
      let guard: ArtifactGuardResult
      try {
        guard = await execution.result
      } finally {
        const snapshot = execution.traces.snapshot()
        journal.replay(snapshot)
        if (scanCleanupFailed(snapshot)) cleanupOutcome = 'failed'
      }
      guards.push(guard)
    }
    const frozenGuards = Object.freeze(guards)
    lastMilestone = 'suite-samples-scanned'
    journal.progress(identity, lastMilestone, 'succeeded', { sampleCount: frozenGuards.length })
    if (frozenGuards.some(({ guardOutcome }) => guardOutcome !== 'passed')) {
      lastMilestone = 'suite-upload-blocked'
      terminalOutcome = 'blocked'
      journal.progress(identity, lastMilestone, 'blocked', {
        sampleCount: frozenGuards.length,
        nonPassedGuardCount: frozenGuards.filter(({ guardOutcome }) => guardOutcome !== 'passed').length,
      })
      outcome = Object.freeze({ guards: frozenGuards, upload: null })
    } else {
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
          ...(options.uploadFaultCuts === undefined ? {} : { faultCuts: options.uploadFaultCuts }),
        })
        lastMilestone = 'suite-upload-sealed'
        terminalOutcome = 'succeeded'
        journal.progress(identity, lastMilestone, 'succeeded', {
          sampleCount: frozenGuards.length,
          uploadManifestSha256: upload.manifestSha256,
        })
        outcome = Object.freeze({ guards: frozenGuards, upload })
      } catch {
        const failed = Object.freeze(frozenGuards.map(failedGuardAfterSuiteSeal))
        lastMilestone = 'suite-upload-failed'
        terminalOutcome = 'failed'
        journal.progress(identity, lastMilestone, 'failed', {
          sampleCount: failed.length,
          failure: GUARD_UPLOAD_FAILURE_MESSAGE,
        })
        outcome = Object.freeze({ guards: failed, upload: null })
      }
    }
  } catch (cause) {
    failure = cause
  }
  if (cleanupOutcome === 'failed') terminalOutcome = 'failed'
  journal.terminal(identity, terminalOutcome, cleanupOutcome, lastMilestone, {
    sampleCount: options.samples.length,
  })
  journal.finish()
  if (journal.failure() !== undefined) {
    throw new Error('artifact guard suite trace settlement failed', {
      cause: failure ?? journal.failure(),
    })
  }
  if (failure !== undefined) throw failure
  if (outcome === undefined) throw new Error('artifact guard suite settled without an outcome')
  return outcome
}

async function runSampleArtifactScan(
  options: ScanSampleArtifactsOptions,
  journal: ArtifactGuardTraceJournal,
): Promise<ArtifactGuardResult> {
  const identity = scanTraceIdentity(options.sample)
  let checked: readonly ArtifactIndexEntry[] = Object.freeze([])
  let sampleResultSha256 = EMPTY_SHA256
  let manifestSha256 = EMPTY_SHA256
  const state = emptyScanState()
  const claimedArtifactCount = Array.isArray(options.sample.artifacts)
    ? options.sample.artifacts.length
    : 0
  journal.start(identity, { artifactCount: claimedArtifactCount })
  let cleanupOutcome: 'completed' | 'failed' | 'not-required' = 'not-required'
  let lastMilestone = 'scan-started'
  let result: ArtifactGuardResult | undefined
  let scanFailure: unknown
  let artifactRoot: HeldArtifactRoot | undefined
  try {
    const executionLease = options.executionLease ?? GuardExecutionLease.start()
    const scanSignal = executionLease.primarySignal('artifact guard scan')
    scanSignal.throwIfAborted()
    checked = Object.freeze([...options.sample.artifacts].sort(compareArtifacts))
    sampleResultSha256 = sha256Bytes(options.sampleResultBytes)
    manifestSha256 = artifactManifestSha256(checked)
    validateSampleResultSnapshot(options.sampleResultBytes, options.sample)
    validateArtifactSizeAuthority(checked)
    const faultCut = validateScanFaultCut(options.faultCut, checked)
    const explicitSecretBytes = parseExplicitSecrets(options.explicitSecrets)
    assertControlPlaneSecretFree(options.sampleResultBytes, checked, explicitSecretBytes)
    lastMilestone = 'scan-input-validated'
    journal.progress(identity, lastMilestone, 'succeeded', {
      artifactCount: checked.length,
      manifestSha256,
    })
    artifactRoot = await holdArtifactRoot(options.artifactRoot, scanSignal)
    cleanupOutcome = 'completed'
    lastMilestone = 'scan-authority-acquired'
    journal.progress(identity, lastMilestone, 'succeeded', { artifactCount: checked.length })
    for (const artifact of checked) {
      scanSignal.throwIfAborted()
      await assertHeldArtifactRoot(artifactRoot)
      await assertArtifactAncestry(artifactRoot.path, artifact.relativePath)
      if (faultCut?.relativePath === artifact.relativePath) {
        lastMilestone = 'scan-fault-cut-reached'
        journal.progress(identity, lastMilestone, 'succeeded', { action: faultCut.action })
        await applyScanFaultCut(faultCut, artifactRoot.path)
      }
      await scanArtifact(artifact, artifactRoot.path, explicitSecretBytes, state, scanSignal)
      await assertArtifactAncestry(artifactRoot.path, artifact.relativePath)
    }
    await assertHeldArtifactRoot(artifactRoot)
    lastMilestone = 'scan-artifacts-processed'
    journal.progress(identity, lastMilestone, 'succeeded', {
      scannedFileCount: state.scannedFileCount,
      scannedArchiveEntryCount: state.scannedArchiveEntryCount,
    })
    result = completedGuardResult(
      options.sample,
      sampleResultSha256,
      manifestSha256,
      checked,
      state,
      uniqueMatchedArtifactIds(state.matches),
    )
  } catch (cause) {
    scanFailure = cause
  }
  if (artifactRoot !== undefined) {
    try {
      await artifactRoot.handle.close()
      cleanupOutcome = 'completed'
    } catch (cause) {
      cleanupOutcome = 'failed'
      scanFailure = new GuardFailure(
        'scanner-crashed',
        ARTIFACT_ROOT_CLEANUP_FAILURE_MESSAGE,
        { cause: scanFailure ?? cause },
      )
    }
  }
  let parsed: ArtifactGuardResult
  try {
    if (scanFailure !== undefined || result === undefined) {
      result = failedGuardResult(
        options.sample,
        sampleResultSha256,
        manifestSha256,
        checked,
        state,
        guardFailure(scanFailure ?? new Error('artifact scan settled without a result')),
      )
    }
    parsed = parseArtifactGuardResult(result)
    if (parsed.guardOutcome === 'passed') {
      validateArtifactGuardForSample(parsed, options.sample, sampleResultSha256)
    }
  } catch (cause) {
    journal.terminal(identity, 'failed', cleanupOutcome, lastMilestone, {
      guardOutcome: 'failed',
      scannedFileCount: state.scannedFileCount,
      scannedArchiveEntryCount: state.scannedArchiveEntryCount,
      observedArchiveBytes: state.observedArchiveBytes,
      expandedArchiveBytes: state.expandedArchiveBytes,
      quarantinedArtifactCount: checked.length,
      failureCode: 'contract',
    })
    journal.finish()
    if (journal.failure() !== undefined) {
      throw new Error('artifact scan trace settlement failed', { cause })
    }
    throw cause
  }
  const terminalOutcome = parsed.guardOutcome === 'passed'
    ? 'succeeded'
    : parsed.guardOutcome === 'quarantined' ? 'blocked' : 'failed'
  journal.terminal(identity, terminalOutcome, cleanupOutcome, lastMilestone, {
    guardOutcome: parsed.guardOutcome,
    scannedFileCount: parsed.scanEvidence.scannedFileCount,
    scannedArchiveEntryCount: parsed.scanEvidence.scannedArchiveEntryCount,
    observedArchiveBytes: parsed.scanEvidence.observedArchiveBytes,
    expandedArchiveBytes: parsed.scanEvidence.expandedArchiveBytes,
    quarantinedArtifactCount: parsed.quarantinedArtifactIds.length,
    failureCode: parsed.failureCode ?? null,
  })
  journal.finish()
  if (journal.failure() !== undefined) {
    throw new Error('artifact scan trace settlement failed', { cause: journal.failure() })
  }
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
  if (Object.getOwnPropertyDescriptor(options, 'hooks') !== undefined) {
    throw new Error('artifact guard suite rejects executable lifecycle hooks')
  }
  const faultSlots = new Set<string>()
  for (const candidate of options.scanFaultCuts ?? []) {
    requirePlainEnumerableRecord(candidate, 'artifact guard suite scan fault')
    if (Object.keys(candidate).sort(compareStrings).join(',') !== 'browser,fault,sampleIndex') {
      throw new Error('artifact guard suite scan fault shape is invalid')
    }
    const slot = guardSlot(candidate)
    if (faultSlots.has(slot)) throw new Error('artifact guard suite repeats a scan fault slot')
    const input = options.samples.find(({ sample }) => guardSlot(sample) === slot)
    if (input === undefined) throw new Error('artifact guard suite scan fault targets an absent sample')
    validateScanFaultCut(candidate.fault, input.sample.artifacts)
    faultSlots.add(slot)
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
    failureMessage: boundedFrameworkMessage(failure.message),
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

function suiteTraceIdentity(options: GuardArtifactSuiteOptions): ArtifactGuardTraceIdentity {
  return Object.freeze({
    operationId: `${options.runId}/${options.suite}/guard-suite`,
    runId: options.runId,
    scenario: 'guard-suite',
    suite: options.suite,
  })
}

function scanTraceIdentity(sample: BrowserSampleResult): ArtifactGuardTraceIdentity {
  return Object.freeze({
    operationId: `${sample.runId}/${sample.suite}/${sample.browser}/${sample.sampleIndex}/guard-scan`,
    runId: sample.runId,
    scenario: 'artifact-scan',
    suite: sample.suite,
    browser: sample.browser,
    sampleIndex: sample.sampleIndex,
  })
}

function scanFaultForSample(
  options: GuardArtifactSuiteOptions,
  sample: BrowserSampleResult,
): ArtifactGuardScanFaultCut | undefined {
  return options.scanFaultCuts?.find((candidate) =>
    candidate.browser === sample.browser && candidate.sampleIndex === sample.sampleIndex)?.fault
}

function scanCleanupFailed(snapshot: ReturnType<ScanSampleArtifactsExecution['traces']['snapshot']>): boolean {
  const terminal = snapshot.events.findLast(({ milestone }) => milestone === 'scan-terminal')
  return terminal?.context.cleanupOutcome === 'failed'
}

function validateScanFaultCut(
  candidate: ArtifactGuardScanFaultCut | undefined,
  artifacts: readonly ArtifactIndexEntry[],
): ArtifactGuardScanFaultCut | undefined {
  if (candidate === undefined) return undefined
  requirePlainEnumerableRecord(candidate, 'artifact guard scan fault cut')
  const keys = Object.keys(candidate).sort(compareStrings)
  if (candidate.action === 'fail-before-artifact-scan') {
    if (keys.join(',') !== 'action,relativePath') {
      throw new GuardFailure('contract', 'artifact guard failure cut shape is invalid')
    }
  } else if (candidate.action === 'replace-artifact-before-scan') {
    if (keys.join(',') !== 'action,relativePath,replacementUtf8') {
      throw new GuardFailure('contract', 'artifact guard replacement cut shape is invalid')
    }
    if (
      typeof candidate.replacementUtf8 !== 'string' ||
      Buffer.byteLength(candidate.replacementUtf8, 'utf8') > MAXIMUM_SCAN_FAULT_REPLACEMENT_BYTES
    ) throw new GuardFailure('contract', 'artifact guard replacement cut exceeds its byte authority')
  } else {
    throw new GuardFailure('contract', 'artifact guard scan fault action is invalid')
  }
  if (
    typeof candidate.relativePath !== 'string' ||
    !artifacts.some(({ relativePath }) => relativePath === candidate.relativePath)
  ) throw new GuardFailure('contract', 'artifact guard scan fault target is not indexed')
  return Object.freeze(
    candidate.action === 'fail-before-artifact-scan'
      ? { action: candidate.action, relativePath: candidate.relativePath }
      : {
          action: candidate.action,
          relativePath: candidate.relativePath,
          replacementUtf8: candidate.replacementUtf8,
        },
  )
}

async function applyScanFaultCut(
  fault: ArtifactGuardScanFaultCut,
  artifactRoot: string,
): Promise<void> {
  if (fault.action === 'fail-before-artifact-scan') {
    throw new GuardFailure(
      'scanner-crashed',
      `declarative artifact scan failure cut reached for ${fault.relativePath}`,
    )
  }
  await writeFile(
    join(artifactRoot, ...fault.relativePath.split('/')),
    fault.replacementUtf8,
    { encoding: 'utf8', flag: 'r+' },
  )
}

function requirePlainEnumerableRecord(value: object, label: string): void {
  if (Object.getPrototypeOf(value) !== Object.prototype) throw new Error(`${label} must be a plain object`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (
    Reflect.ownKeys(value).some((key) => typeof key !== 'string') ||
    Object.values(descriptors).some((descriptor) => !descriptor.enumerable || !('value' in descriptor))
  ) throw new Error(`${label} contains hidden or executable properties`)
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
    failureMessage: GUARD_UPLOAD_FAILURE_MESSAGE,
  })
}

function guardSlot(value: Pick<BrowserSampleResult, 'browser' | 'sampleIndex'>): string {
  return `${value.browser}/${value.sampleIndex}`
}

function guardFailure(cause: unknown): GuardFailure {
  if (isOwnedGuardFailure(cause)) return cause
  return new GuardFailure('scanner-crashed', ARTIFACT_SCANNER_FAILURE_MESSAGE, { cause })
}

function boundedFrameworkMessage(source: string): string {
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
