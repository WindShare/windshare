import {
  createHash,
  generateKeyPairSync,
  randomBytes,
  sign,
  type KeyObject,
} from 'node:crypto'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  TextReader,
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipWriter,
} from '@zip.js/zip.js'

import {
  requireCompleteArtifactGuardTrace,
  startScanSampleArtifacts,
  type ArtifactGuardScanFaultCut,
} from '../../scripts/browser-evidence/artifact/guard.ts'
import {
  startBrowserSample,
  type BrowserSampleRunExecution,
  type BrowserSampleRunOutcome,
} from '../../scripts/browser-evidence/sample-runner.ts'
import type {
  BrowserSampleContainmentBackend,
} from '../../scripts/browser-evidence/process/containment.ts'
import type { BrowserSampleStagingFaultCut } from '../../scripts/browser-evidence/process/attachment-staging.ts'
import { createDeterministicTestContainmentBackend } from './deterministic-containment.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  canonicalTestIceTopologyResolutionJson,
  testIceTopologyResolutionSha256,
  testIceTopologySha256,
  verifyTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from '../../scripts/browser-evidence/test-ice-topology.ts'
import type { ArtifactGuardResult } from '../../scripts/browser-evidence/artifact/guard-result.ts'
import type {
  GuardUploadTopologySnapshots,
} from '../../scripts/browser-evidence/artifact/sealed-suite.ts'
import type { GuardUploadDirectoryPublisher } from '../../scripts/browser-evidence/artifact/directory-publisher.ts'
import {
  PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS,
  PROCESS_SETTLEMENT_SCHEMA_VERSION,
  canonicalProcessSettlementPayloadBytes,
  processSettlementPublicKeyFingerprint,
  processSettlementSampleId,
  type ProcessSettlementAttestation,
  type ProcessSettlementEvidence,
  type ProcessSettlementPayload,
  type ProcessSettlementTrustAnchor,
} from '../../scripts/browser-evidence/artifact/settlement-receipt.ts'
import {
  browserRunPolicy,
  type BrowserRunPolicy,
} from '../../scripts/browser-evidence/run-policy.ts'
import type { BrowserEngine, BrowserSuite } from '../../scripts/browser-evidence/vocabulary.ts'
import { createDeterministicDirectoryPublisher } from './deterministic-directory-publisher.ts'

export const FRAMEWORK_CHECKOUT_SHA = 'a'.repeat(40)
export const FRAMEWORK_RUN_ID = 'framework-run'
export const FRAMEWORK_COMMAND_SHA256 = 'b'.repeat(64)
export const SYNTHETIC_CHILD_PATH = fileURLToPath(
  new URL('./fixtures/synthetic-child.mjs', import.meta.url),
)
export interface FrameworkTopology {
  readonly lock: VerifiedTestIceTopologyLock
  readonly profilePath: string
  readonly resolutionPath: string
}

export interface SyntheticSampleOptions {
  readonly workspace: string
  readonly runId?: string
  readonly runPolicy?: BrowserRunPolicy
  readonly topology: FrameworkTopology
  readonly suite: BrowserSuite
  readonly browser?: BrowserEngine
  readonly sampleIndex?: number
  readonly mode: string
  readonly environment?: Readonly<Record<string, string>>
  readonly maximumCapturedStreamBytes?: number
  readonly processDeadlineMs?: number
  readonly delayMs?: number
  readonly containmentBackend?: BrowserSampleContainmentBackend
  readonly stagingFaultCut?: BrowserSampleStagingFaultCut
  readonly readOnlyInputRoots?: readonly string[]
}

export interface FrameworkGuardAuthority {
  readonly topology: GuardUploadTopologySnapshots
  readonly directoryPublisher: GuardUploadDirectoryPublisher
  readonly settlementTrust: ProcessSettlementTrustAnchor
  readonly commandSha256: string
  settlementAttestation(
    outcome: BrowserSampleRunOutcome,
    resultBytes: Uint8Array,
  ): ProcessSettlementAttestation
}

export async function loadFrameworkTopology(): Promise<FrameworkTopology> {
  const profilePath = fileURLToPath(new URL(
    '../../../testdata/test-ice-topology/pr-same-host-kernel-route-ipv4.json',
    import.meta.url,
  ))
  const resolutionPath = fileURLToPath(new URL(
    '../../../testdata/test-ice-topology/pr-same-host-kernel-route-ipv4-resolution.json',
    import.meta.url,
  ))
  const profile = parseTestIceTopologyJson(await readFile(profilePath, 'utf8'))
  const profileSha256 = await testIceTopologySha256(profile)
  const resolution = parseTestIceTopologyResolutionJson(
    await readFile(resolutionPath, 'utf8'),
    profile,
    profileSha256,
  )
  const resolutionSha256 = await testIceTopologyResolutionSha256(
    resolution,
    profile,
    profileSha256,
  )
  return Object.freeze({
    lock: await verifyTestIceTopologyLock(profile, resolution, profileSha256, resolutionSha256),
    profilePath,
    resolutionPath,
  })
}

export async function createAlternateFrameworkTopology(workspace: string): Promise<FrameworkTopology> {
  const base = await loadFrameworkTopology()
  const resolution = parseTestIceTopologyResolutionJson(JSON.stringify({
    ...base.lock.resolution,
    interface: {
      ...base.lock.resolution.interface,
      index: base.lock.resolution.interface.index + 1,
      name: `${base.lock.resolution.interface.name}-alternate`,
    },
  }), base.lock.profile, base.lock.profileSha256)
  const resolutionSha256 = await testIceTopologyResolutionSha256(
    resolution,
    base.lock.profile,
    base.lock.profileSha256,
  )
  const resolutionPath = join(workspace, 'alternate-topology-resolution.json')
  await writeFile(
    resolutionPath,
    canonicalTestIceTopologyResolutionJson(
      resolution,
      base.lock.profile,
      base.lock.profileSha256,
    ),
    'utf8',
  )
  return Object.freeze({
    lock: await verifyTestIceTopologyLock(
      base.lock.profile,
      resolution,
      base.lock.profileSha256,
      resolutionSha256,
    ),
    profilePath: base.profilePath,
    resolutionPath,
  })
}

export async function createFrameworkGuardAuthority(
  topology: FrameworkTopology,
): Promise<FrameworkGuardAuthority> {
  const profileBytes = Uint8Array.from(await readFile(topology.profilePath))
  const resolutionBytes = Uint8Array.from(await readFile(topology.resolutionPath))
  const { privateKey, publicKey } = generateKeyPairSync('ed25519')
  const publicKeySpkiBase64 = publicKey.export({ format: 'der', type: 'spki' }).toString('base64')
  const settlementTrust = Object.freeze({
    invocationId: `framework-${randomBytes(16).toString('hex')}`,
    publicKeySpkiBase64,
    publicKeySha256: processSettlementPublicKeyFingerprint(publicKeySpkiBase64),
  })
  return Object.freeze({
    topology: Object.freeze({ profileBytes, resolutionBytes }),
    directoryPublisher: createDeterministicDirectoryPublisher(),
    settlementTrust,
    commandSha256: FRAMEWORK_COMMAND_SHA256,
    settlementAttestation: (
      outcome: BrowserSampleRunOutcome,
      resultBytes: Uint8Array,
    ) => signFrameworkSettlement(privateKey, settlementTrust, outcome, resultBytes),
  })
}

function signFrameworkSettlement(
  privateKey: KeyObject,
  trust: ProcessSettlementTrustAnchor,
  outcome: BrowserSampleRunOutcome,
  resultBytes: Uint8Array,
): ProcessSettlementAttestation {
  const issuedAt = Date.now()
  const payload = Object.freeze({
    schemaVersion: PROCESS_SETTLEMENT_SCHEMA_VERSION,
    invocationId: trust.invocationId,
    sampleId: processSettlementSampleId(
      outcome.result.suite,
      outcome.result.browser,
      outcome.result.sampleIndex,
    ),
    runId: outcome.result.runId,
    runPolicy: outcome.result.runPolicy,
    suite: outcome.result.suite,
    browser: outcome.result.browser,
    sampleIndex: outcome.result.sampleIndex,
    checkoutSha: outcome.result.checkoutSha,
    commandSha256: FRAMEWORK_COMMAND_SHA256,
    resultSha256: createHash('sha256').update(resultBytes).digest('hex'),
    resultByteLength: String(resultBytes.byteLength),
    process: frameworkProcessEvidence(outcome),
    treeEmpty: true,
    cleanupOutcome: 'completed',
    input: Object.freeze({ outcome: 'delivered', failureCode: '', failureMessage: '' }),
    ownership: Object.freeze({
      kind: 'test-process-owner',
      backend: 'linux_subreaper',
      terminationReason: 'natural',
    }),
    nonce: randomBytes(32).toString('hex'),
    issuedAtUnixMs: String(issuedAt),
    expiresAtUnixMs: String(issuedAt + PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS),
  }) satisfies ProcessSettlementPayload
  return Object.freeze({
    payload,
    signatureBase64: sign(null, canonicalProcessSettlementPayloadBytes(payload), privateKey)
      .toString('base64'),
  })
}

function frameworkProcessEvidence(outcome: BrowserSampleRunOutcome): ProcessSettlementEvidence {
  const terminal = outcome.result.executionEvidence.runnerProcess
  if (terminal.terminal === 'exited') {
    return Object.freeze({ terminal: 'exited', exitCode: terminal.exitCode })
  }
  if (terminal.terminal === 'signaled') {
    return Object.freeze({ terminal: 'signaled', signal: terminal.signal })
  }
  if (terminal.terminal === 'spawn-failed') {
    return Object.freeze({
      terminal: 'spawn-failed',
      errorCode: terminal.errorCode,
      errorMessage: terminal.errorMessage,
    })
  }
  throw new Error('framework settlement requires a terminal synthetic sample process')
}

export async function createFrameworkWorkspace(): Promise<string> {
  return mkdtemp(join(tmpdir(), 'windshare-browser-evidence-'))
}

export async function removeFrameworkWorkspace(path: string): Promise<void> {
  await rm(path, { recursive: true, force: true })
}

export async function runSyntheticSample(
  options: SyntheticSampleOptions,
): Promise<BrowserSampleRunOutcome> {
  const execution = startSyntheticSample(options)
  const outcome = await execution.result
  const snapshot = execution.traces.snapshot()
  if (
    !snapshot.completed ||
    snapshot.truncated ||
    snapshot.observedEvents !== snapshot.capturedEvents
  ) throw new Error('synthetic fixture discarded incomplete browser sample trace evidence')
  return outcome
}

export function startSyntheticSample(
  options: SyntheticSampleOptions,
): BrowserSampleRunExecution {
  const browser = options.browser ?? 'chromium'
  const sampleIndex = options.sampleIndex ?? 1
  const sampleDirectory = join(options.workspace, options.suite, browser, `sample-${sampleIndex}`)
  const containmentBackend = options.containmentBackend ?? createDeterministicTestContainmentBackend()
  return startBrowserSample({
    runId: options.runId ?? FRAMEWORK_RUN_ID,
    operationId: `${options.suite}-${browser}-sample-${sampleIndex}`,
    scenario: 'synthetic-browser-evidence',
    runPolicy: options.runPolicy ?? browserRunPolicy('closure'),
    suite: options.suite,
    browser,
    sampleIndex,
    checkoutSha: FRAMEWORK_CHECKOUT_SHA,
    sampleDirectory,
    topologyLock: options.topology.lock,
    topologyProfilePath: options.topology.profilePath,
    topologyResolutionPath: options.topology.resolutionPath,
    command: {
      executable: process.execPath,
      arguments: [SYNTHETIC_CHILD_PATH],
      cwd: resolve(fileURLToPath(new URL('../../', import.meta.url))),
      environment: {
        SYNTHETIC_CHILD_MODE: options.mode,
        // The containment regression must target the parent-owned result
        // explicitly; deriving authority from a writable attachment path would
        // make the fixture validate its own assumption instead of the boundary.
        SYNTHETIC_FINAL_RESULT_PATH: join(sampleDirectory, 'result.json'),
        ...(options.delayMs === undefined ? {} : { SYNTHETIC_CHILD_DELAY_MS: String(options.delayMs) }),
        ...options.environment,
      },
    },
    ...(options.maximumCapturedStreamBytes === undefined
      ? {}
      : { maximumCapturedStreamBytes: options.maximumCapturedStreamBytes }),
    ...(options.processDeadlineMs === undefined ? {} : { processDeadlineMs: options.processDeadlineMs }),
    containmentBackend,
    ...(options.stagingFaultCut === undefined ? {} : { stagingFaultCut: options.stagingFaultCut }),
    readOnlyInputRoots: options.readOnlyInputRoots ?? [options.workspace],
  })
}

export async function guardSyntheticSample(
  outcome: BrowserSampleRunOutcome,
  explicitSecrets: readonly string[] = [],
  faultCut?: ArtifactGuardScanFaultCut,
): Promise<ArtifactGuardResult> {
  const artifactRoot = artifactRootForOutcome(outcome)
  const execution = startScanSampleArtifacts({
    sample: outcome.result,
    sampleResultBytes: await readFile(outcome.resultPath),
    artifactRoot,
    explicitSecrets: explicitSecrets.map((value) => ({ value })),
    ...(faultCut === undefined ? {} : { faultCut }),
  })
  const result = await execution.result
  requireCompleteArtifactGuardTrace(execution.traces.snapshot(), 'synthetic artifact scan trace')
  return result
}

export function artifactRootForOutcome(outcome: BrowserSampleRunOutcome): string {
  return outcome.artifactRoot
}

export async function createZip(
  entries: readonly { readonly name: string; readonly data: string | Uint8Array }[],
): Promise<Uint8Array> {
  const writer = new ZipWriter(new Uint8ArrayWriter(), { useWebWorkers: false })
  for (const entry of entries) {
    await writer.add(
      entry.name,
      typeof entry.data === 'string' ? new TextReader(entry.data) : new Uint8ArrayReader(entry.data),
    )
  }
  return writer.close()
}

export function artifactEnvironment(
  source: string,
  relativePath = 'playwright/diagnostic.bin',
  kind = 'process-log',
  mediaType = 'application/octet-stream',
): Readonly<Record<string, string>> {
  return Object.freeze({
    SYNTHETIC_ARTIFACT_SOURCE: source,
    SYNTHETIC_ARTIFACT_PATH: relativePath,
    SYNTHETIC_ARTIFACT_KIND: kind,
    SYNTHETIC_ARTIFACT_MEDIA_TYPE: mediaType,
  })
}

export function sampleDirectoryFromResultPath(resultPath: string): string {
  return dirname(resultPath)
}
