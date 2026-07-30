import { createHash } from 'node:crypto'
import type { BigIntStats } from 'node:fs'
import { lstat, open } from 'node:fs/promises'
import type { FileHandle } from 'node:fs/promises'
import { basename, extname, isAbsolute, join, resolve } from 'node:path'

import {
  scanTrustedZip,
  TrustedZipFailure,
  type ArchiveByteSource,
  type TrustedZipEntry,
} from '../archive/trusted-zip.ts'
import { artifactAbsolutePath } from './index.ts'
import { artifactManifestSha256, sha256Bytes } from './manifest.ts'
import {
  sealGuardUploadSuite,
  type GuardUploadHooks,
  type GuardUploadSelection,
  type GuardUploadTopologySnapshots,
} from './sealed-suite.ts'
import {
  requireGuardUploadDirectoryPublisher,
  type GuardUploadDirectoryPublisher,
} from './directory-publisher.ts'
import {
  requireVerifiedProcessSettlementSet,
  verifyProcessSettlementAttestations,
  type ProcessSettlementTrustAnchor,
} from './settlement-receipt.ts'
import { GuardExecutionLease } from '../execution/guard-execution-lease.ts'
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
} from './guard-result.ts'
import type { ArtifactIndexEntry, BrowserSampleResult } from '../result.ts'
import { assertBrowserRunPolicyEqual, parseBrowserRunPolicy } from '../run-policy.ts'
import { requirePortableRelativePath } from '../filesystem/portable-path.ts'
import { parseCanonicalJsonText } from '../contract/strict-json.ts'
import { BROWSER_ENGINES } from '../vocabulary.ts'

const GITHUB_TOKEN_PATTERN = /(?:^|\W)(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_\w{20,255})(?:$|\W)/u
const TOKEN_SCAN_TAIL_BYTES = 512
const ZIP_PREFIX_BYTES = 4
const ZIP_END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES = 65_557
export interface ExplicitGuardSecret {
  readonly value: string
}

export interface ArtifactGuardScanHooks {
  readonly beforeArtifactScan?: (artifact: ArtifactIndexEntry) => void | Promise<void>
}

export interface GuardArtifactSuiteSample {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly commandSha256: string
  readonly settlementAttestation: unknown
}

export interface GuardArtifactSuiteOptions {
  readonly runId: string
  readonly runPolicy: BrowserSampleResult['runPolicy']
  readonly suite: BrowserSampleResult['suite']
  readonly checkoutSha: string
  readonly samples: readonly GuardArtifactSuiteSample[]
  readonly uploadParent: string
  readonly topology: GuardUploadTopologySnapshots
  readonly settlementTrust: ProcessSettlementTrustAnchor
  readonly directoryPublisher: GuardUploadDirectoryPublisher
  readonly executionLease?: GuardExecutionLease
  readonly explicitSecrets: readonly ExplicitGuardSecret[]
  readonly hooks?: ArtifactGuardSuiteHooks
  readonly trace?: ArtifactGuardTraceSink
}

export interface ArtifactGuardSuiteHooks {
  readonly beforeArtifactScan?: (
    sample: BrowserSampleResult,
    artifact: ArtifactIndexEntry,
  ) => void | Promise<void>
  readonly upload?: GuardUploadHooks
}

export interface GuardArtifactSuiteResult {
  readonly guards: readonly ArtifactGuardResult[]
  readonly upload: GuardUploadSelection | null
}

interface ScanSampleArtifactsOptions extends ArtifactGuardScanHooks {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly explicitSecrets: readonly ExplicitGuardSecret[]
  readonly trace?: ArtifactGuardTraceSink
  readonly executionLease?: GuardExecutionLease
}

export interface ArtifactGuardTraceEvent {
  readonly component: 'artifact-guard'
  readonly operationId: string
  readonly milestone: string
  readonly context: Readonly<Record<string, string | number | boolean | null>>
}

export type ArtifactGuardTraceSink = (event: ArtifactGuardTraceEvent) => void

interface ScanState {
  scannedFileCount: number
  scannedArchiveEntryCount: number
  observedArchiveBytes: number
  expandedArchiveBytes: number
  observedMaximumArchiveDepth: number
  readonly matches: GuardMatchEvidence[]
}

interface ScannedFileBytes {
  readonly prefix: Uint8Array
  readonly tail: Uint8Array
  readonly byteLength: number
  readonly sha256: string
}

interface HeldArtifactRoot {
  readonly path: string
  readonly handle: FileHandle
  readonly identity: BigIntStats
}

type GuardFailureCode = NonNullable<ArtifactGuardResult['failureCode']>

class GuardFailure extends Error {
  readonly code: GuardFailureCode

  constructor(code: GuardFailureCode, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'GuardFailure'
    this.code = code
  }
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

function assertControlPlaneSecretFree(
  sampleResultBytes: Uint8Array,
  artifacts: readonly ArtifactIndexEntry[],
  explicitSecrets: readonly Uint8Array[],
): void {
  const scanner = new StreamSecretScanner(explicitSecrets)
  scanner.scan(sampleResultBytes)
  scanner.scan(Buffer.from(JSON.stringify(artifacts), 'utf8'))
  if (scanner.detectors.size > 0) {
    throw new GuardFailure('contract', 'browser evidence control-plane bytes contain a protected secret')
  }
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

async function scanArtifact(
  artifact: ArtifactIndexEntry,
  artifactRoot: string,
  explicitSecrets: readonly Uint8Array[],
  state: ScanState,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  const path = artifactAbsolutePath(artifactRoot, artifact.relativePath)
  state.scannedFileCount += 1
  let reader: NodeFileReader | undefined
  try {
    reader = await NodeFileReader.open(path, signal)
    const scanner = new StreamSecretScanner(explicitSecrets)
    if (reader.byteLength !== artifact.byteLength) {
      throw new GuardFailure('contract', `artifact ${artifact.relativePath} length changed before its guard scan`)
    }
    const scanned = await reader.scan(scanner, artifact.byteLength)
    if (scanned.byteLength !== artifact.byteLength || scanned.sha256 !== artifact.sha256) {
      throw new GuardFailure('contract', `artifact ${artifact.relativePath} changed before its guard scan`)
    }
    recordMatches(state, artifact.artifactId, 'file', null, scanner.detectors)
    if (!isZipArtifact(artifact.relativePath, scanned.prefix, scanned.tail)) return
    state.observedMaximumArchiveDepth = Math.max(state.observedMaximumArchiveDepth, 1)
    state.observedArchiveBytes += artifact.byteLength
    if (state.observedArchiveBytes > GUARD_MAXIMUM_ARCHIVE_BYTES) {
      throw new GuardFailure('archive-byte-limit', 'archive bytes exceed the frozen guard limit')
    }
    await scanZip(reader, artifact.relativePath, artifact.artifactId, explicitSecrets, state, signal)
    await reader.assertStable()
  } catch (cause) {
    if (cause instanceof GuardFailure) throw cause
    throw new GuardFailure('scanner-crashed', `artifact scanner crashed for ${artifact.relativePath}`, {
      cause,
    })
  } finally {
    await reader?.close().catch(() => undefined)
  }
}

async function scanZip(
  reader: NodeFileReader,
  relativePath: string,
  artifactId: string,
  explicitSecrets: readonly Uint8Array[],
  state: ScanState,
  signal: AbortSignal,
): Promise<void> {
  const entryPaths = new Set<string>()
  const initialEntryCount = state.scannedArchiveEntryCount
  const initialExpandedBytes = state.expandedArchiveBytes
  let active: {
    readonly entry: TrustedZipEntry
    readonly entryPath: string
    readonly scanner: StreamSecretScanner | null
    readonly prefix: number[]
    readonly tail: ByteTail
  } | null = null
  try {
    await scanTrustedZip(reader, {
      maximumEntries: GUARD_MAXIMUM_ARCHIVE_ENTRIES - initialEntryCount,
      maximumExpandedBytes: GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES - initialExpandedBytes,
      maximumPathBytes: 1_024,
    }, {
      start(entry) {
        signal.throwIfAborted()
        if (active !== null) throw new Error('trusted ZIP visitor received overlapping entries')
        state.scannedArchiveEntryCount += 1
        const entryPath = normalizedArchivePath(entry.path, entry.directory)
        if (entryPaths.has(entryPath)) {
          throw new GuardFailure('invalid-archive', 'ZIP contains duplicate entry paths')
        }
        entryPaths.add(entryPath)
        active = {
          entry,
          entryPath,
          scanner: entry.directory ? null : new StreamSecretScanner(explicitSecrets),
          prefix: [],
          tail: new ByteTail(ZIP_END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES),
        }
      },
      chunk(entry, chunk) {
        signal.throwIfAborted()
        if (active === null || active.entry !== entry || active.scanner === null) {
          throw new Error('trusted ZIP visitor received bytes outside an active file entry')
        }
        state.expandedArchiveBytes += chunk.byteLength
        for (const byte of chunk.subarray(0, ZIP_PREFIX_BYTES - active.prefix.length)) {
          active.prefix.push(byte)
        }
        active.tail.push(chunk)
        active.scanner.scan(chunk)
      },
      end(entry) {
        signal.throwIfAborted()
        if (active === null || active.entry !== entry) {
          throw new Error('trusted ZIP visitor ended a different entry')
        }
        if (active.scanner !== null) {
          if (isZipByteSequence(Uint8Array.from(active.prefix), active.tail.bytes())) {
            state.observedMaximumArchiveDepth = GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH + 1
            throw new GuardFailure(
              'archive-nesting-limit',
              'nested ZIP archive exceeds the frozen depth limit',
            )
          }
          recordMatches(
            state,
            artifactId,
            'archive-entry',
            active.entryPath,
            active.scanner.detectors,
          )
        }
        active = null
      },
    })
  } catch (cause) {
    if (cause instanceof GuardFailure) throw cause
    if (cause instanceof TrustedZipFailure) {
      if (cause.kind === 'archive-entry-limit') {
        state.scannedArchiveEntryCount = Math.max(
          state.scannedArchiveEntryCount,
          initialEntryCount + (cause.observedEntryCount ?? 0),
        )
      }
      if (cause.kind === 'archive-expanded-byte-limit') {
        state.expandedArchiveBytes = Math.max(
          state.expandedArchiveBytes,
          initialExpandedBytes + (cause.observedExpandedBytes ?? 0),
        )
      }
      if (cause.kind !== 'invalid-archive') {
        throw new GuardFailure(cause.kind, cause.message, { cause })
      }
      throw new GuardFailure(
        'invalid-archive',
        `ZIP archive is malformed: ${basename(relativePath)} (${cause.message})`,
        { cause },
      )
    }
    throw cause
  }
}

class StreamSecretScanner {
  readonly #secrets: readonly Uint8Array[]
  readonly detectors = new Set<'explicit-secret' | 'github-token-pattern'>()
  #tail = Buffer.alloc(0)
  readonly #tailBytes: number

  constructor(secrets: readonly Uint8Array[]) {
    this.#secrets = secrets
    this.#tailBytes = Math.max(TOKEN_SCAN_TAIL_BYTES, ...secrets.map((secret) => secret.byteLength))
  }

  scan(chunk: Uint8Array): void {
    const combined = Buffer.concat([this.#tail, Buffer.from(chunk)])
    if (this.#secrets.some((secret) => combined.indexOf(secret) >= 0)) {
      this.detectors.add('explicit-secret')
    }
    if (GITHUB_TOKEN_PATTERN.test(combined.toString('latin1'))) {
      this.detectors.add('github-token-pattern')
    }
    const retained = Math.min(this.#tailBytes, combined.byteLength)
    this.#tail = combined.subarray(combined.byteLength - retained)
  }
}

class ByteTail {
  readonly #maximumBytes: number
  #value = Buffer.alloc(0)

  constructor(maximumBytes: number) {
    this.#maximumBytes = maximumBytes
  }

  push(chunk: Uint8Array): void {
    const combined = Buffer.concat([this.#value, Buffer.from(chunk)])
    this.#value = combined.subarray(Math.max(0, combined.byteLength - this.#maximumBytes))
  }

  bytes(): Uint8Array {
    return Uint8Array.from(this.#value)
  }
}

class NodeFileReader implements ArchiveByteSource {
  readonly #path: string
  readonly #handle: FileHandle
  readonly #pathBefore: BigIntStats
  readonly #openedBefore: BigIntStats
  readonly byteLength: number
  readonly #signal: AbortSignal

  private constructor(
    path: string,
    handle: FileHandle,
    pathBefore: BigIntStats,
    openedBefore: BigIntStats,
    signal: AbortSignal,
  ) {
    this.#path = path
    this.#handle = handle
    this.#pathBefore = pathBefore
    this.#openedBefore = openedBefore
    this.byteLength = Number(openedBefore.size)
    this.#signal = signal
  }

  static async open(path: string, signal: AbortSignal): Promise<NodeFileReader> {
    signal.throwIfAborted()
    const pathBefore = await lstat(path, { bigint: true })
    if (!pathBefore.isFile() || pathBefore.isSymbolicLink()) {
      throw new GuardFailure('contract', 'indexed artifact is not a regular file')
    }
    const handle = await open(path, 'r')
    try {
      const openedBefore = await handle.stat({ bigint: true })
      if (
        !sameFileIdentity(pathBefore, openedBefore) ||
        openedBefore.size > BigInt(GUARD_MAXIMUM_ARTIFACT_FILE_BYTES)
      ) {
        throw new GuardFailure('contract', 'indexed artifact changed while it was opened')
      }
      return new NodeFileReader(path, handle, pathBefore, openedBefore, signal)
    } catch (cause) {
      await handle.close().catch(() => undefined)
      throw cause
    }
  }

  async scan(scanner: StreamSecretScanner, maximumBytes: number): Promise<ScannedFileBytes> {
    const prefix: number[] = []
    const tail = new ByteTail(ZIP_END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES)
    const digest = createHash('sha256')
    let byteLength = 0
    for await (const value of this.#handle.createReadStream({
      autoClose: false,
      start: 0,
      signal: this.#signal,
    })) {
      this.#signal.throwIfAborted()
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value)
      for (const byte of chunk.subarray(0, ZIP_PREFIX_BYTES - prefix.length)) prefix.push(byte)
      tail.push(chunk)
      scanner.scan(chunk)
      digest.update(chunk)
      byteLength += chunk.byteLength
      if (byteLength > maximumBytes) {
        throw new GuardFailure('contract', 'indexed artifact grew beyond its byte authority during guard scan')
      }
    }
    await this.assertStable(byteLength)
    return Object.freeze({
      prefix: Uint8Array.from(prefix),
      tail: tail.bytes(),
      byteLength,
      sha256: digest.digest('hex'),
    })
  }

  async readExactly(index: number, length: number): Promise<Uint8Array> {
    this.#signal.throwIfAborted()
    const buffer = Buffer.allocUnsafe(length)
    let offset = 0
    while (offset < length) {
      this.#signal.throwIfAborted()
      const { bytesRead } = await this.#handle.read(buffer, offset, length - offset, index + offset)
      if (bytesRead === 0) break
      offset += bytesRead
    }
    return Uint8Array.from(buffer.subarray(0, offset))
  }

  async assertStable(observedBytes = Number(this.#openedBefore.size)): Promise<void> {
    this.#signal.throwIfAborted()
    const openedAfter = await this.#handle.stat({ bigint: true })
    const pathAfter = await lstat(this.#path, { bigint: true })
    if (
      !sameFileIdentity(this.#pathBefore, openedAfter) ||
      !sameFileIdentity(openedAfter, pathAfter) ||
      this.#openedBefore.size !== openedAfter.size ||
      this.#openedBefore.mtimeNs !== openedAfter.mtimeNs ||
      this.#openedBefore.ctimeNs !== openedAfter.ctimeNs ||
      openedAfter.size !== BigInt(observedBytes)
    ) {
      throw new GuardFailure('contract', 'indexed artifact changed during its guard scan')
    }
  }

  async close(): Promise<void> {
    await this.#handle.close()
  }
}

function sameFileIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
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

function recordMatches(
  state: ScanState,
  artifactId: string,
  location: GuardMatchEvidence['location'],
  archiveEntryPath: string | null,
  detectors: ReadonlySet<GuardMatchEvidence['detector']>,
): void {
  for (const detector of detectors) {
    state.matches.push(Object.freeze({ artifactId, location, archiveEntryPath, detector }))
  }
}

function parseExplicitSecrets(secrets: readonly ExplicitGuardSecret[]): readonly Uint8Array[] {
  const values = new Map<string, Uint8Array>()
  for (const secret of secrets) {
    if (secret.value.length === 0) throw new GuardFailure('contract', 'explicit guard secrets must be non-empty')
    const bytes = Buffer.from(secret.value, 'utf8')
    values.set(bytes.toString('base64'), bytes)
  }
  return Object.freeze([...values.values()])
}

function normalizedArchivePath(filename: string, directory: boolean): string {
  const value = directory && filename.endsWith('/') ? filename.slice(0, -1) : filename
  try {
    requirePortableRelativePath(value, 'ZIP entry path')
  } catch (cause) {
    throw new GuardFailure('archive-path', 'ZIP entry path is not portable and root-confined', { cause })
  }
  if (Buffer.byteLength(value, 'utf8') > 1_024) {
    throw new GuardFailure('archive-path', 'ZIP entry path exceeds the guard limit')
  }
  return value
}

function isZipArtifact(relativePath: string, prefix: Uint8Array, tail: Uint8Array): boolean {
  return extname(relativePath).toLowerCase() === '.zip' || isZipByteSequence(prefix, tail)
}

function isZipByteSequence(prefix: Uint8Array, tail: Uint8Array): boolean {
  return isZipPrefix(prefix) || containsZipEndOfCentralDirectory(tail)
}

function isZipPrefix(prefix: Uint8Array): boolean {
  if (prefix.length < ZIP_PREFIX_BYTES) return false
  return prefix[0] === 0x50 && prefix[1] === 0x4b && (
    (prefix[2] === 0x03 && prefix[3] === 0x04) ||
    (prefix[2] === 0x05 && prefix[3] === 0x06) ||
    (prefix[2] === 0x07 && prefix[3] === 0x08)
  )
}

function containsZipEndOfCentralDirectory(tail: Uint8Array): boolean {
  for (let index = 0; index <= tail.length - ZIP_PREFIX_BYTES; index += 1) {
    if (
      tail[index] === 0x50 && tail[index + 1] === 0x4b &&
      tail[index + 2] === 0x05 && tail[index + 3] === 0x06
    ) return true
  }
  return false
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
