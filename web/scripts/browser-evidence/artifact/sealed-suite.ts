import { createHash, randomBytes } from 'node:crypto'
import type { BigIntStats } from 'node:fs'
import { lstat, open, type FileHandle } from 'node:fs/promises'
import { basename, dirname, isAbsolute, join, resolve } from 'node:path'

import {
  artifactIdForManifest,
  artifactManifestSha256,
  sha256Bytes,
} from './manifest.ts'
import {
  requireVerifiedProcessSettlementSet,
  type VerifiedProcessSettlementSet,
} from './settlement-receipt.ts'
import { GuardExecutionLease } from '../execution/guard-execution-lease.ts'
import { readStableRegularFileSnapshot } from '../filesystem/snapshot.ts'
import {
  GuardUploadDirectoryPublisherUnsettledError,
  requireGuardUploadDirectoryPublisher,
  type GuardUploadDirectoryPublisher,
} from './directory-publisher.ts'
import type { ArtifactGuardResult } from './guard-result.ts'
import {
  GUARD_MAXIMUM_ARTIFACT_FILES,
  GUARD_MAXIMUM_ARTIFACT_FILE_BYTES,
  GUARD_MAXIMUM_TOTAL_ARTIFACT_BYTES,
  parseArtifactGuardResult,
} from './guard-result.ts'
import {
  freezeRecord,
  requireArray,
  requireCheckoutSha,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
} from '../contract/json.ts'
import {
  comparePortablePaths,
  portablePathCollisionKey,
  requirePortableRelativePath,
} from '../filesystem/portable-path.ts'
import type { ArtifactIndexEntry, BrowserSampleResult } from '../result.ts'
import { ARTIFACT_KINDS } from '../result.ts'
import {
  assertBrowserRunPolicyEqual,
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from '../run-policy.ts'
import { parseCanonicalJsonText } from '../contract/strict-json.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
} from '../test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  type BrowserEngine,
  type BrowserSuite,
} from '../vocabulary.ts'
import type {
  ExistingDirectoryPublisherInventory,
  ExistingDirectoryPublisherResponse,
  ExistingDirectoryPublisherSnapshot,
  PublisherHelperFailureCode,
} from '../../browser-network-matrix/cli/publisher-helper-protocol.ts'

export const GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION = 2 as const
export const GUARD_UPLOAD_MANIFEST_FILENAME = 'manifest.json' as const
export const GUARD_UPLOAD_RESULT_FILENAME = 'result.json' as const
export const GUARD_UPLOAD_GUARD_FILENAME = 'guard.json' as const
export const GUARD_UPLOAD_SAMPLES_DIRECTORY = 'samples' as const
export const GUARD_UPLOAD_ATTACHMENTS_DIRECTORY = 'attachments' as const
export const GUARD_UPLOAD_TOPOLOGY_DIRECTORY = 'topology' as const
export const GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH = 'topology/profile.json' as const
export const GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH = 'topology/resolution.json' as const
export const GUARD_UPLOAD_OUTPUT_NAME = 'sealed' as const

const PRIVATE_UPLOAD_PREFIX = '.browser-evidence-upload-'
const MAXIMUM_UPLOAD_MANIFEST_BYTES = 8 * 1_024 * 1_024
const MAXIMUM_SAMPLE_RESULT_BYTES = 16 * 1_024 * 1_024
const MAXIMUM_GUARD_RESULT_BYTES = 1 * 1_024 * 1_024
const MAXIMUM_TOPOLOGY_BYTES = 1 * 1_024 * 1_024
const COPY_BUFFER_BYTES = 64 * 1_024
const STAGING_NAME_PATTERN = /^\.browser-evidence-upload-[a-f0-9]{32}$/u

export interface GuardUploadFileAuthority {
  readonly relativePath: string
  readonly byteLength: string
  readonly sha256: string
}

export interface GuardUploadTopologyManifest {
  readonly profile: GuardUploadFileAuthority & {
    readonly relativePath: typeof GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH
  }
  readonly resolution: GuardUploadFileAuthority & {
    readonly relativePath: typeof GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH
  }
}

export interface GuardUploadArtifactManifest {
  readonly artifactId: string
  readonly kind: ArtifactIndexEntry['kind']
  readonly relativePath: string
  readonly mediaType: string
  readonly byteLength: string
  readonly sha256: string
}

export interface GuardUploadManifest {
  readonly schemaVersion: typeof GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly checkoutSha: string
  readonly topology: GuardUploadTopologyManifest
  readonly samples: readonly GuardUploadSampleManifest[]
}

export interface GuardUploadSampleManifest {
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly sampleResultByteLength: string
  readonly sampleResultSha256: string
  readonly guardResultByteLength: string
  readonly guardResultSha256: string
  readonly artifactManifestSha256: string
  readonly artifacts: readonly GuardUploadArtifactManifest[]
}

export interface GuardUploadSelection {
  readonly uploadDirectory: string
  readonly manifestSha256: string
  readonly manifestByteLength: string
  readonly manifest: GuardUploadManifest
  readonly topologySnapshots: GuardUploadTopologySnapshots
  readonly guards: readonly ArtifactGuardResult[]
  readonly sampleSnapshots: readonly GuardUploadSampleSnapshot[]
}

export interface GuardUploadTopologySnapshots {
  readonly profileBytes: Uint8Array
  readonly resolutionBytes: Uint8Array
}

export interface GuardUploadSampleSnapshot {
  readonly manifest: GuardUploadSampleManifest
  readonly resultBytes: Uint8Array
  readonly guardBytes: Uint8Array
  readonly guard: ArtifactGuardResult
}

export interface GuardUploadSampleContractPaths {
  readonly sampleDirectory: string
  readonly resultPath: string
  readonly guardPath: string
  readonly attachmentsDirectory: string
}

export interface GuardUploadSampleInput {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly guard: ArtifactGuardResult
  readonly commandSha256: string
}

export interface SealGuardUploadSuiteOptions {
  readonly uploadParent: string
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly checkoutSha: string
  readonly samples: readonly GuardUploadSampleInput[]
  readonly topology: GuardUploadTopologySnapshots
  readonly settlement: VerifiedProcessSettlementSet
  readonly settlementInvocationId: string
  readonly directoryPublisher: GuardUploadDirectoryPublisher
  readonly executionLease?: GuardExecutionLease
  readonly hooks?: GuardUploadHooks
}

export interface GuardUploadHooks {
  readonly beforeArtifactCopy?: (
    sample: BrowserSampleResult,
    artifact: ArtifactIndexEntry,
    sourcePath: string,
    destinationPath: string,
  ) => void | Promise<void>
  readonly beforeSeal?: (privateStagingRoot: string) => void | Promise<void>
}

interface SamplePayload {
  readonly input: GuardUploadSampleInput
  readonly guardBytes: Uint8Array
  readonly manifest: GuardUploadSampleManifest
}

interface CanonicalTopology {
  readonly profileBytes: Uint8Array
  readonly resolutionBytes: Uint8Array
  readonly manifest: GuardUploadTopologyManifest
}

class NativePublisherFailure extends Error {
  readonly failureCode: PublisherHelperFailureCode

  constructor(failureCode: PublisherHelperFailureCode, operation: string) {
    super(`native artifact publisher ${operation} failed: ${failureCode}`)
    this.name = 'NativePublisherFailure'
    this.failureCode = failureCode
  }
}

/**
 * Native prepare owns namespace privacy and identity; Node is only a byte
 * producer for files that already exist. The receipt is required again at
 * publish/cleanup, so a same-name staging replacement never gains authority.
 */
export async function sealGuardUploadSuite(
  options: SealGuardUploadSuiteOptions,
): Promise<GuardUploadSelection> {
  const executionLease = options.executionLease ?? GuardExecutionLease.start()
  executionLease.throwIfPrimaryExpired('guard upload sealing')
  requireGuardUploadDirectoryPublisher(options.directoryPublisher)
  const uploadParent = requireCanonicalAbsolutePath(options.uploadParent, 'guard upload parent')
  const samples = canonicalSuiteInputs(options)
  const settlementSamples = samples.map((input) => Object.freeze({
    sample: input.sample,
    resultBytes: input.sampleResultBytes,
    commandSha256: input.commandSha256,
  }))
  requireVerifiedProcessSettlementSet(options.settlement, {
    invocationId: requirePortableToken(
      options.settlementInvocationId,
      'process settlement invocation ID',
    ),
    samples: settlementSamples,
  })
  const topology = canonicalTopology(options.topology, samples)
  const samplePayloads = canonicalSamplePayloads(options, samples)
  const manifest = freezeRecord({
    schemaVersion: GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION,
    runId: requirePortableToken(options.runId, 'guard upload run ID'),
    runPolicy: parseBrowserRunPolicy(options.runPolicy, 'guard upload run policy'),
    suite: requireEnum(options.suite, BROWSER_SUITES, 'guard upload suite'),
    checkoutSha: requireCheckoutSha(options.checkoutSha, 'guard upload checkout SHA'),
    topology: topology.manifest,
    samples: Object.freeze(samplePayloads.map(({ manifest: sample }) => sample)),
  }) satisfies GuardUploadManifest
  const manifestBytes = Buffer.from(JSON.stringify(manifest), 'utf8')
  if (manifestBytes.byteLength < 1 || manifestBytes.byteLength > MAXIMUM_UPLOAD_MANIFEST_BYTES) {
    throw new Error('guard upload manifest exceeds its byte authority')
  }
  const manifestSha256 = sha256Bytes(manifestBytes)
  const inventory = buildInventory(manifest, manifestBytes.byteLength, manifestSha256)
  const snapshotPaths = snapshotPathsForManifest(manifest)
  const stagingName = `${PRIVATE_UPLOAD_PREFIX}${randomBytes(16).toString('hex')}`
  if (!STAGING_NAME_PATTERN.test(stagingName)) throw new Error('guard staging name is not canonical')

  const prepared = await options.directoryPublisher.invoke({
    operation: 'prepare-existing-directory',
    parentPath: uploadParent,
    outputName: GUARD_UPLOAD_OUTPUT_NAME,
    stagingName,
    inventory,
    manifestPath: GUARD_UPLOAD_MANIFEST_FILENAME,
    expectedManifestSha256: manifestSha256,
  }, executionLease.primaryWindow('native guard staging prepare'))
  if (prepared.outcome === 'failed') {
    const failure = new NativePublisherFailure(prepared.failureCode, 'prepare')
    if (prepared.stagingReceipt === null) throw failure
    return cleanupAfterFailure(
      options.directoryPublisher,
      uploadParent,
      stagingName,
      prepared.stagingReceipt,
      inventory,
      manifestSha256,
      failure,
      executionLease,
    )
  }
  const receipt = prepared.stagingReceipt
  const stagePath = join(uploadParent, stagingName)
  try {
    await executionLease.runPrimary('guard upload materialization', async (signal) => {
      await materializePreparedTree(
        stagePath,
        manifestBytes,
        topology,
        samplePayloads,
        options.hooks,
        signal,
      )
    })
    await executionLease.runPrimary('guard upload pre-seal hook', async (signal) => {
      await options.hooks?.beforeSeal?.(stagePath)
      signal.throwIfAborted()
    })
    const completed = await publishOrRecover(
      options.directoryPublisher,
      uploadParent,
      stagingName,
      receipt,
      inventory,
      manifestSha256,
      snapshotPaths,
      executionLease,
    )
    return selectionFromNativeSnapshots(
      join(uploadParent, GUARD_UPLOAD_OUTPUT_NAME),
      String(manifestBytes.byteLength),
      manifestSha256,
      completed,
      manifestBytes,
    )
  } catch (cause) {
    return cleanupAfterFailure(
      options.directoryPublisher,
      uploadParent,
      stagingName,
      receipt,
      inventory,
      manifestSha256,
      cause,
      executionLease,
    )
  }
}

export async function resolveGuardUpload(options: {
  readonly suite?: BrowserSuite
  readonly uploadDirectory: string
  readonly manifestSha256: string
  readonly manifestByteLength: string
  readonly directoryPublisher: GuardUploadDirectoryPublisher
  readonly executionLease?: GuardExecutionLease
}): Promise<GuardUploadSelection> {
  const executionLease = options.executionLease ?? GuardExecutionLease.start()
  executionLease.throwIfPrimaryExpired('sealed guard upload resolution')
  requireGuardUploadDirectoryPublisher(options.directoryPublisher)
  const uploadDirectory = requireCanonicalAbsolutePath(
    options.uploadDirectory,
    'sealed guard upload directory',
  )
  if (basename(uploadDirectory) !== GUARD_UPLOAD_OUTPUT_NAME ||
      uploadDirectory !== join(dirname(uploadDirectory), GUARD_UPLOAD_OUTPUT_NAME)) {
    throw new Error('sealed guard upload must use the deterministic output name')
  }
  const manifestSha256 = requireSha256(options.manifestSha256, 'guard upload manifest SHA-256')
  const manifestByteLength = requireDecimal(
    options.manifestByteLength,
    MAXIMUM_UPLOAD_MANIFEST_BYTES,
    'guard upload manifest byte length',
  )
  const manifestSnapshot = await readStableRegularFileSnapshot(
    join(uploadDirectory, GUARD_UPLOAD_MANIFEST_FILENAME),
    Number(manifestByteLength),
    'guard upload manifest',
  )
  if (manifestSnapshot.sha256 !== manifestSha256 ||
      String(manifestSnapshot.bytes.byteLength) !== manifestByteLength) {
    throw new Error('guard upload manifest differs from its exact external authority')
  }
  const manifest = parseGuardUploadManifest(decodeUtf8(manifestSnapshot.bytes, 'guard upload manifest'))
  if (options.suite !== undefined && manifest.suite !== options.suite) {
    throw new Error('guard upload suite differs from its external ledger slot')
  }
  const inventory = buildInventory(manifest, Number(manifestByteLength), manifestSha256)
  const response = await options.directoryPublisher.invoke({
    operation: 'verify-existing-directory',
    parentPath: dirname(uploadDirectory),
    outputName: GUARD_UPLOAD_OUTPUT_NAME,
    stagingName: '',
    inventory,
    manifestPath: GUARD_UPLOAD_MANIFEST_FILENAME,
    expectedManifestSha256: manifestSha256,
    snapshotPaths: snapshotPathsForManifest(manifest),
  }, executionLease.primaryWindow('native sealed guard verification'))
  if (response.outcome === 'failed') {
    throw new NativePublisherFailure(response.failureCode, 'verify')
  }
  return selectionFromNativeSnapshots(
    uploadDirectory,
    manifestByteLength,
    manifestSha256,
    response,
    manifestSnapshot.bytes,
  )
}

export function guardUploadSampleContractPaths(
  uploadDirectory: string,
  sample: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): GuardUploadSampleContractPaths {
  const sampleDirectory = sampleUploadRoot(
    requireCanonicalAbsolutePath(uploadDirectory, 'sealed guard upload directory'),
    sample,
  )
  return freezeRecord({
    sampleDirectory,
    resultPath: join(sampleDirectory, GUARD_UPLOAD_RESULT_FILENAME),
    guardPath: join(sampleDirectory, GUARD_UPLOAD_GUARD_FILENAME),
    attachmentsDirectory: join(sampleDirectory, GUARD_UPLOAD_ATTACHMENTS_DIRECTORY),
  })
}

export function parseGuardUploadManifest(encoded: string): GuardUploadManifest {
  const record = requireRecord(
    parseCanonicalJsonText(encoded, 'guard upload manifest'),
    'guard upload manifest',
  )
  requireExactKeys(record, [
    'schemaVersion', 'runId', 'runPolicy', 'suite', 'checkoutSha', 'topology', 'samples',
  ], [], 'guard upload manifest')
  const runPolicy = parseBrowserRunPolicy(record.runPolicy, 'guard upload run policy')
  const manifest = freezeRecord({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION,
      'guard upload manifest schema version',
    ),
    runId: requirePortableToken(record.runId, 'guard upload run ID'),
    runPolicy,
    suite: requireEnum(record.suite, BROWSER_SUITES, 'guard upload suite'),
    checkoutSha: requireCheckoutSha(record.checkoutSha, 'guard upload checkout SHA'),
    topology: parseTopologyManifest(record.topology),
    samples: parseSampleManifests(record.samples, runPolicy),
  })
  requireExactSampleSlots(manifest.samples, runPolicy)
  if (JSON.stringify(manifest) !== encoded) {
    throw new Error('guard upload manifest is not canonical JSON')
  }
  return manifest
}

async function publishOrRecover(
  directoryPublisher: GuardUploadDirectoryPublisher,
  parentPath: string,
  stagingName: string,
  stagingReceipt: string,
  inventory: ExistingDirectoryPublisherInventory,
  manifestSha256: string,
  snapshotPaths: readonly string[],
  executionLease: GuardExecutionLease,
): Promise<Extract<ExistingDirectoryPublisherResponse, { readonly outcome: 'completed' }> & {
  readonly operation: 'publish-existing-directory' | 'verify-existing-directory'
}> {
  let primaryFailure: unknown
  let mayHaveCommitted = true
  try {
    const response = await directoryPublisher.invoke({
      operation: 'publish-existing-directory',
      parentPath,
      outputName: GUARD_UPLOAD_OUTPUT_NAME,
      stagingName,
      stagingReceipt,
      inventory,
      manifestPath: GUARD_UPLOAD_MANIFEST_FILENAME,
      expectedManifestSha256: manifestSha256,
      snapshotPaths,
    }, executionLease.primaryWindow('native guard publication'))
    if (response.outcome === 'completed') return response
    primaryFailure = new NativePublisherFailure(response.failureCode, 'publish')
    mayHaveCommitted = response.failureCode === 'publication-unsafe' ||
      response.failureCode === 'response-failed'
  } catch (cause) {
    if (containsNativePublisherUnsettledFailure(cause)) throw cause
    primaryFailure = cause
  }
  if (mayHaveCommitted) {
    try {
      const recovered = await directoryPublisher.invoke({
        operation: 'verify-existing-directory',
        parentPath,
        outputName: GUARD_UPLOAD_OUTPUT_NAME,
        stagingName: '',
        inventory,
        manifestPath: GUARD_UPLOAD_MANIFEST_FILENAME,
        expectedManifestSha256: manifestSha256,
        snapshotPaths,
      }, executionLease.cleanupWindow('native guard post-commit verification'))
      if (recovered.outcome === 'completed') return recovered
      throw new NativePublisherFailure(recovered.failureCode, 'post-commit verify')
    } catch (recoveryFailure) {
      throw new AggregateError(
        [primaryFailure, recoveryFailure],
        'native artifact publication failed and exact final recovery was not authorized',
        { cause: recoveryFailure },
      )
    }
  }
  throw primaryFailure
}

async function cleanupAfterFailure(
  directoryPublisher: GuardUploadDirectoryPublisher,
  parentPath: string,
  stagingName: string,
  stagingReceipt: string,
  inventory: ExistingDirectoryPublisherInventory,
  manifestSha256: string,
  primaryFailure: unknown,
  executionLease: GuardExecutionLease,
): Promise<never> {
  if (containsNativePublisherUnsettledFailure(primaryFailure)) {
    throw new AggregateError(
      [primaryFailure],
      'guard upload failed while a native publisher remained active; cleanup was not started concurrently',
      { cause: primaryFailure },
    )
  }
  try {
    executionLease.assertCleanupSafe('guard staging cleanup')
  } catch (unsettledFailure) {
    throw new AggregateError(
      [primaryFailure, unsettledFailure],
      'guard upload failed while a primary operation remained active; cleanup was not started concurrently',
      { cause: unsettledFailure },
    )
  }
  let cleanupFailure: unknown
  try {
    const response = await directoryPublisher.invoke({
      operation: 'cleanup-existing-directory',
      parentPath,
      outputName: GUARD_UPLOAD_OUTPUT_NAME,
      stagingName,
      stagingReceipt,
      inventory,
      manifestPath: GUARD_UPLOAD_MANIFEST_FILENAME,
      expectedManifestSha256: manifestSha256,
    }, executionLease.cleanupWindow('native guard staging cleanup'))
    if (response.outcome === 'failed') {
      throw new NativePublisherFailure(response.failureCode, 'cleanup')
    }
    if (response.cleanupOutcome === 'ambiguous') {
      throw new Error('native artifact staging cleanup remained ambiguous')
    }
  } catch (cause) {
    cleanupFailure = cause
  }
  if (cleanupFailure !== undefined) {
    throw new AggregateError(
      [primaryFailure, cleanupFailure],
      'guard upload failed and receipt-bound native cleanup did not settle',
      { cause: cleanupFailure },
    )
  }
  throw primaryFailure
}

function containsNativePublisherUnsettledFailure(value: unknown): boolean {
  if (value instanceof GuardUploadDirectoryPublisherUnsettledError) return true
  if (value instanceof AggregateError) {
    return value.errors.some(containsNativePublisherUnsettledFailure)
  }
  return value instanceof Error && value.cause !== undefined
    ? containsNativePublisherUnsettledFailure(value.cause)
    : false
}

async function materializePreparedTree(
  stagePath: string,
  manifestBytes: Uint8Array,
  topology: CanonicalTopology,
  samples: readonly SamplePayload[],
  hooks: GuardUploadHooks | undefined,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  await writePreparedFile(
    join(stagePath, GUARD_UPLOAD_MANIFEST_FILENAME),
    manifestBytes,
    'guard upload manifest',
    signal,
  )
  await writePreparedFile(
    join(stagePath, ...GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH.split('/')),
    topology.profileBytes,
    'guard topology profile',
    signal,
  )
  await writePreparedFile(
    join(stagePath, ...GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH.split('/')),
    topology.resolutionBytes,
    'guard topology resolution',
    signal,
  )
  for (const payload of samples) {
    const sampleRoot = sampleUploadRoot(stagePath, payload.input.sample)
    await writePreparedFile(
      join(sampleRoot, GUARD_UPLOAD_RESULT_FILENAME),
      payload.input.sampleResultBytes,
      'guard upload sample result',
      signal,
    )
    await writePreparedFile(
      join(sampleRoot, GUARD_UPLOAD_GUARD_FILENAME),
      payload.guardBytes,
      'guard upload guard result',
      signal,
    )
    const sourceRoot = requireCanonicalAbsolutePath(payload.input.artifactRoot, 'guard artifact root')
    const destinationRoot = join(sampleRoot, GUARD_UPLOAD_ATTACHMENTS_DIRECTORY)
    for (const artifact of payload.input.sample.artifacts) {
      const segments = artifactPathSegments(artifact.relativePath)
      const sourcePath = join(sourceRoot, ...segments)
      const destinationPath = join(destinationRoot, ...segments)
      await hooks?.beforeArtifactCopy?.(
        payload.input.sample,
        artifact,
        sourcePath,
        destinationPath,
      )
      signal.throwIfAborted()
      await assertDirectoryAncestry(sourceRoot, segments.slice(0, -1), 'guard artifact source', signal)
      await copyVerifiedArtifact(sourcePath, destinationPath, artifact, signal)
      await assertDirectoryAncestry(sourceRoot, segments.slice(0, -1), 'guard artifact source', signal)
    }
  }
}

function selectionFromNativeSnapshots(
  uploadDirectory: string,
  manifestByteLength: string,
  manifestSha256: string,
  response: Extract<ExistingDirectoryPublisherResponse, { readonly outcome: 'completed' }> & {
    readonly operation: 'publish-existing-directory' | 'verify-existing-directory'
  },
  expectedManifestBytes: Uint8Array,
): GuardUploadSelection {
  if (response.manifestSha256 !== manifestSha256) {
    throw new Error('native publisher returned the wrong manifest digest')
  }
  const snapshots = snapshotMap(response.snapshots)
  const manifestSnapshot = requireSnapshot(
    snapshots,
    GUARD_UPLOAD_MANIFEST_FILENAME,
    manifestByteLength,
    manifestSha256,
  )
  if (!Buffer.from(manifestSnapshot.bytes).equals(Buffer.from(expectedManifestBytes))) {
    throw new Error('native manifest snapshot differs from the previously authenticated bytes')
  }
  const manifest = parseGuardUploadManifest(decodeUtf8(manifestSnapshot.bytes, 'guard upload manifest'))
  const expectedPaths = snapshotPathsForManifest(manifest)
  if (snapshots.size !== expectedPaths.length || expectedPaths.some((path) => !snapshots.has(path))) {
    throw new Error('native publisher did not return the exact requested snapshot inventory')
  }
  const profile = requireSnapshotAuthority(snapshots, manifest.topology.profile)
  const resolution = requireSnapshotAuthority(snapshots, manifest.topology.resolution)
  validateTopologyBytes(profile.bytes, resolution.bytes)
  const guards: ArtifactGuardResult[] = []
  const sampleSnapshots: GuardUploadSampleSnapshot[] = []
  for (const sample of manifest.samples) {
    const relativeRoot = relativeSampleUploadRoot(sample)
    const result = requireSnapshot(
      snapshots,
      `${relativeRoot}/${GUARD_UPLOAD_RESULT_FILENAME}`,
      sample.sampleResultByteLength,
      sample.sampleResultSha256,
    )
    const guardSnapshot = requireSnapshot(
      snapshots,
      `${relativeRoot}/${GUARD_UPLOAD_GUARD_FILENAME}`,
      sample.guardResultByteLength,
      sample.guardResultSha256,
    )
    const guard = parseArtifactGuardResult(parseCanonicalJsonText(
      decodeUtf8(guardSnapshot.bytes, 'guard upload guard result'),
      'guard upload guard result',
    ))
    assertGuardMatchesSampleManifest(guard, manifest, sample)
    guards.push(guard)
    sampleSnapshots.push(Object.freeze({
      manifest: sample,
      resultBytes: Uint8Array.from(result.bytes),
      guardBytes: Uint8Array.from(guardSnapshot.bytes),
      guard,
    }))
  }
  return freezeRecord({
    uploadDirectory,
    manifestSha256,
    manifestByteLength,
    manifest,
    topologySnapshots: Object.freeze({
      profileBytes: Uint8Array.from(profile.bytes),
      resolutionBytes: Uint8Array.from(resolution.bytes),
    }),
    guards: Object.freeze(guards),
    sampleSnapshots: Object.freeze(sampleSnapshots),
  })
}

function canonicalTopology(
  value: GuardUploadTopologySnapshots,
  samples: readonly GuardUploadSampleInput[],
): CanonicalTopology {
  const profileBytes = Uint8Array.from(value.profileBytes)
  const resolutionBytes = Uint8Array.from(value.resolutionBytes)
  validateTopologyBytes(profileBytes, resolutionBytes, samples.map(({ sample }) => sample))
  return Object.freeze({
    profileBytes,
    resolutionBytes,
    manifest: freezeRecord({
      profile: fileAuthority(
        GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH,
        profileBytes,
      ) as GuardUploadTopologyManifest['profile'],
      resolution: fileAuthority(
        GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH,
        resolutionBytes,
      ) as GuardUploadTopologyManifest['resolution'],
    }),
  })
}

function validateTopologyBytes(
  profileBytes: Uint8Array,
  resolutionBytes: Uint8Array,
  samples: readonly Pick<BrowserSampleResult, 'topologyProfileSha256' | 'topologyResolutionSha256'>[] = [],
): void {
  if (profileBytes.byteLength < 1 || profileBytes.byteLength > MAXIMUM_TOPOLOGY_BYTES ||
      resolutionBytes.byteLength < 1 || resolutionBytes.byteLength > MAXIMUM_TOPOLOGY_BYTES) {
    throw new Error('guard topology snapshot exceeds its byte authority')
  }
  const profileSha256 = sha256Bytes(profileBytes)
  const resolutionSha256 = sha256Bytes(resolutionBytes)
  const profile = parseTestIceTopologyJson(decodeUtf8(profileBytes, 'guard topology profile'))
  parseTestIceTopologyResolutionJson(
    decodeUtf8(resolutionBytes, 'guard topology resolution'),
    profile,
    profileSha256,
  )
  if (samples.some((sample) =>
    sample.topologyProfileSha256 !== profileSha256 ||
    sample.topologyResolutionSha256 !== resolutionSha256)) {
    throw new Error('guard topology snapshots do not bind every sample result')
  }
}

function canonicalSamplePayloads(
  options: SealGuardUploadSuiteOptions,
  samples: readonly GuardUploadSampleInput[],
): readonly SamplePayload[] {
  return Object.freeze(samples.map((input) => {
    const guardBytes = Buffer.from(JSON.stringify(input.guard), 'utf8')
    const artifacts = canonicalArtifactManifests(input.sample.artifacts)
    const sampleManifest = freezeRecord({
      browser: input.sample.browser,
      sampleIndex: input.sample.sampleIndex,
      sampleResultByteLength: String(input.sampleResultBytes.byteLength),
      sampleResultSha256: sha256Bytes(input.sampleResultBytes),
      guardResultByteLength: String(guardBytes.byteLength),
      guardResultSha256: sha256Bytes(guardBytes),
      artifactManifestSha256: artifactManifestSha256(input.sample.artifacts),
      artifacts,
    })
    assertGuardMatchesSampleManifest(input.guard, options, sampleManifest)
    return Object.freeze({ input, guardBytes: Uint8Array.from(guardBytes), manifest: sampleManifest })
  }))
}

function canonicalSuiteInputs(
  options: SealGuardUploadSuiteOptions,
): readonly GuardUploadSampleInput[] {
  const runId = requirePortableToken(options.runId, 'guard upload run ID')
  const checkoutSha = requireCheckoutSha(options.checkoutSha, 'guard upload checkout SHA')
  const suite = requireEnum(options.suite, BROWSER_SUITES, 'guard upload suite')
  const runPolicy = parseBrowserRunPolicy(options.runPolicy, 'guard upload run policy')
  const inputs = options.samples.map((input) => {
    assertSampleResultSnapshot(input.sampleResultBytes, input.sample)
    if (
      input.sample.resultStatus === 'provisional' || input.sample.runId !== runId ||
      input.sample.suite !== suite || input.sample.checkoutSha !== checkoutSha
    ) throw new Error('guard upload sample does not match its suite authority')
    assertBrowserRunPolicyEqual(input.sample.runPolicy, runPolicy, 'guard upload run policy')
    if (input.guard.browser !== input.sample.browser ||
        input.guard.sampleIndex !== input.sample.sampleIndex) {
      throw new Error('guard upload sample and guard occupy different suite slots')
    }
    requireCanonicalAbsolutePath(input.artifactRoot, 'guard artifact root')
    requireSha256(input.commandSha256, 'guard sample command digest')
    return input
  }).sort(compareSampleInputs)
  requireExactSampleSlots(inputs.map(({ sample }) => sample), runPolicy)
  return Object.freeze(inputs)
}

function buildInventory(
  manifest: GuardUploadManifest,
  manifestByteLength: number,
  manifestSha256: string,
): ExistingDirectoryPublisherInventory {
  const directories = new Set<string>()
  const files = new Map<string, { readonly byteLength: string; readonly sha256: string }>()
  const addFile = (relativePath: string, byteLength: string, sha256: string): void => {
    requirePortableRelativePath(relativePath, 'guard upload inventory path')
    if (files.has(relativePath)) throw new Error('guard upload inventory repeats a file path')
    files.set(relativePath, { byteLength, sha256 })
    const segments = relativePath.split('/')
    for (let index = 1; index < segments.length; index += 1) {
      directories.add(segments.slice(0, index).join('/'))
    }
  }
  addFile(GUARD_UPLOAD_MANIFEST_FILENAME, String(manifestByteLength), manifestSha256)
  addFile(
    manifest.topology.profile.relativePath,
    manifest.topology.profile.byteLength,
    manifest.topology.profile.sha256,
  )
  addFile(
    manifest.topology.resolution.relativePath,
    manifest.topology.resolution.byteLength,
    manifest.topology.resolution.sha256,
  )
  for (const sample of manifest.samples) {
    const sampleRoot = relativeSampleUploadRoot(sample)
    addFile(
      `${sampleRoot}/${GUARD_UPLOAD_RESULT_FILENAME}`,
      sample.sampleResultByteLength,
      sample.sampleResultSha256,
    )
    addFile(
      `${sampleRoot}/${GUARD_UPLOAD_GUARD_FILENAME}`,
      sample.guardResultByteLength,
      sample.guardResultSha256,
    )
    directories.add(`${sampleRoot}/${GUARD_UPLOAD_ATTACHMENTS_DIRECTORY}`)
    for (const artifact of sample.artifacts) {
      addFile(
        `${sampleRoot}/${GUARD_UPLOAD_ATTACHMENTS_DIRECTORY}/${artifact.relativePath}`,
        artifact.byteLength,
        artifact.sha256,
      )
    }
  }
  const orderedDirectories = [...directories].sort(comparePortablePaths)
  const orderedFiles = [...files.entries()]
    .sort(([left], [right]) => comparePortablePaths(left, right))
    .map(([relativePath, authority]) => freezeRecord({ relativePath, ...authority }))
  assertPortableInventoryCollisionFree(orderedDirectories, orderedFiles)
  return freezeRecord({
    directories: Object.freeze(orderedDirectories),
    files: Object.freeze(orderedFiles),
  })
}

function snapshotPathsForManifest(manifest: GuardUploadManifest): readonly string[] {
  const paths = [
    GUARD_UPLOAD_MANIFEST_FILENAME,
    manifest.topology.profile.relativePath,
    manifest.topology.resolution.relativePath,
    ...manifest.samples.flatMap((sample) => {
      const root = relativeSampleUploadRoot(sample)
      return [
        `${root}/${GUARD_UPLOAD_RESULT_FILENAME}`,
        `${root}/${GUARD_UPLOAD_GUARD_FILENAME}`,
      ]
    }),
  ].sort(comparePortablePaths)
  return Object.freeze(paths)
}

function parseTopologyManifest(value: unknown): GuardUploadTopologyManifest {
  const record = requireRecord(value, 'guard upload topology')
  requireExactKeys(record, ['profile', 'resolution'], [], 'guard upload topology')
  return freezeRecord({
    profile: parseFileAuthority(
      record.profile,
      GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH,
      MAXIMUM_TOPOLOGY_BYTES,
      'guard topology profile',
    ) as GuardUploadTopologyManifest['profile'],
    resolution: parseFileAuthority(
      record.resolution,
      GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH,
      MAXIMUM_TOPOLOGY_BYTES,
      'guard topology resolution',
    ) as GuardUploadTopologyManifest['resolution'],
  })
}

function parseSampleManifests(value: unknown, runPolicy: BrowserRunPolicy): readonly GuardUploadSampleManifest[] {
  const samples = requireArray(value, 'guard upload samples').map((item, index) => {
    const record = requireRecord(item, `guard upload sample ${index}`)
    requireExactKeys(record, [
      'browser', 'sampleIndex', 'sampleResultByteLength', 'sampleResultSha256',
      'guardResultByteLength', 'guardResultSha256', 'artifactManifestSha256', 'artifacts',
    ], [], `guard upload sample ${index}`)
    const artifacts = parseArtifactManifests(record.artifacts)
    const sample = freezeRecord({
      browser: requireEnum(record.browser, BROWSER_ENGINES, `guard upload sample ${index} browser`),
      sampleIndex: validatePolicySampleIndex(requireSafeInteger(
        record.sampleIndex,
        1,
        runPolicy.sampleCount,
        `guard upload sample ${index} index`,
      ), runPolicy, `guard upload sample ${index} index`),
      sampleResultByteLength: requireDecimal(
        record.sampleResultByteLength,
        MAXIMUM_SAMPLE_RESULT_BYTES,
        `guard upload sample ${index} result byte length`,
      ),
      sampleResultSha256: requireSha256(
        record.sampleResultSha256,
        `guard upload sample ${index} result SHA-256`,
      ),
      guardResultByteLength: requireDecimal(
        record.guardResultByteLength,
        MAXIMUM_GUARD_RESULT_BYTES,
        `guard upload sample ${index} guard byte length`,
      ),
      guardResultSha256: requireSha256(
        record.guardResultSha256,
        `guard upload sample ${index} guard SHA-256`,
      ),
      artifactManifestSha256: requireSha256(
        record.artifactManifestSha256,
        `guard upload sample ${index} artifact manifest SHA-256`,
      ),
      artifacts,
    })
    if (sample.artifactManifestSha256 !== artifactManifestSha256(
      artifacts.map(numericArtifactManifest),
    )) throw new Error(`guard upload sample ${index} does not bind its exact artifact index`)
    return sample
  })
  const canonical = [...samples].sort(compareSampleManifests)
  if (samples.some((sample, index) => sample !== canonical[index])) {
    throw new Error('guard upload samples are not canonically ordered')
  }
  requireExactSampleSlots(samples, runPolicy)
  return Object.freeze(samples)
}

function parseArtifactManifests(value: unknown): readonly GuardUploadArtifactManifest[] {
  const values = requireArray(value, 'guard upload artifact index')
  if (values.length > GUARD_MAXIMUM_ARTIFACT_FILES) {
    throw new Error('guard upload artifact index exceeds the frozen file-count limit')
  }
  let totalBytes = 0
  const identities = new Set<string>()
  const paths = new Set<string>()
  const portablePaths = new Set<string>()
  const artifacts = values.map((item, index) => {
    const record = requireRecord(item, `guard upload artifact ${index}`)
    requireExactKeys(record, [
      'artifactId', 'kind', 'relativePath', 'mediaType', 'byteLength', 'sha256',
    ], [], `guard upload artifact ${index}`)
    const relativePath = requirePortableRelativePath(
      record.relativePath,
      `guard upload artifact ${index} relative path`,
    )
    const artifact = freezeRecord({
      artifactId: requirePortableToken(record.artifactId, `guard upload artifact ${index} ID`),
      kind: requireEnum(record.kind, ARTIFACT_KINDS, `guard upload artifact ${index} kind`),
      relativePath,
      mediaType: requireString(record.mediaType, `guard upload artifact ${index} media type`, 128),
      byteLength: requireDecimal(
        record.byteLength,
        GUARD_MAXIMUM_ARTIFACT_FILE_BYTES,
        `guard upload artifact ${index} byte length`,
      ),
      sha256: requireSha256(record.sha256, `guard upload artifact ${index} SHA-256`),
    })
    const numeric = numericArtifactManifest(artifact)
    if (artifact.artifactId !== artifactIdForManifest(numeric)) {
      throw new Error(`guard upload artifact ${index} ID does not bind its exact manifest`)
    }
    totalBytes += numeric.byteLength
    if (!Number.isSafeInteger(totalBytes) || totalBytes > GUARD_MAXIMUM_TOTAL_ARTIFACT_BYTES) {
      throw new Error('guard upload artifact index exceeds the frozen total-byte limit')
    }
    const portablePath = portablePathCollisionKey(relativePath)
    if (identities.has(artifact.artifactId) || paths.has(relativePath) || portablePaths.has(portablePath)) {
      throw new Error('guard upload artifact index contains duplicate or colliding authority')
    }
    identities.add(artifact.artifactId)
    paths.add(relativePath)
    portablePaths.add(portablePath)
    return artifact
  })
  const canonical = [...artifacts].sort(compareArtifactManifests)
  if (artifacts.some((artifact, index) => artifact !== canonical[index])) {
    throw new Error('guard upload artifact index is not canonically ordered')
  }
  return Object.freeze(artifacts)
}

function canonicalArtifactManifests(
  artifacts: readonly ArtifactIndexEntry[],
): readonly GuardUploadArtifactManifest[] {
  return parseArtifactManifests(artifacts.map((artifact) => ({
    ...artifact,
    byteLength: String(artifact.byteLength),
  })))
}

function numericArtifactManifest(artifact: GuardUploadArtifactManifest): ArtifactIndexEntry {
  return freezeRecord({ ...artifact, byteLength: Number(artifact.byteLength) })
}

function assertGuardMatchesSampleManifest(
  guard: ArtifactGuardResult,
  suite: Pick<GuardUploadManifest, 'runId' | 'runPolicy' | 'suite' | 'checkoutSha'>,
  sample: GuardUploadSampleManifest,
): void {
  if (
    guard.guardOutcome !== 'passed' || guard.runId !== suite.runId || guard.suite !== suite.suite ||
    guard.browser !== sample.browser || guard.sampleIndex !== sample.sampleIndex ||
    guard.checkoutSha !== suite.checkoutSha || guard.sampleResultSha256 !== sample.sampleResultSha256 ||
    String(Buffer.byteLength(JSON.stringify(guard), 'utf8')) !== sample.guardResultByteLength ||
    guard.artifactManifestSha256 !== sample.artifactManifestSha256
  ) throw new Error('guard upload manifest does not match its passed guard authority')
  assertBrowserRunPolicyEqual(guard.runPolicy, suite.runPolicy, 'guard upload run policy')
  const artifactIds = sample.artifacts.map(({ artifactId }) => artifactId).sort(compareStrings)
  if (
    !sameOrderedStrings(guard.checkedArtifactIds, artifactIds) ||
    !sameOrderedStrings(guard.uploadableArtifactIds, artifactIds) ||
    guard.quarantinedArtifactIds.length !== 0 || guard.matches.length !== 0
  ) throw new Error('guard upload passed authority does not authorize the exact artifact index')
}

async function writePreparedFile(
  path: string,
  bytes: Uint8Array,
  label: string,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  const namedBefore = await lstat(path, { bigint: true })
  requireRegularFileMetadata(namedBefore, label)
  if (namedBefore.size !== BigInt(bytes.byteLength)) {
    throw new Error(`${label} native-prepared size differs from its byte authority`)
  }
  const handle = await open(path, 'r+')
  try {
    const openedBefore = await handle.stat({ bigint: true })
    if (!sameIdentity(namedBefore, openedBefore) || openedBefore.size !== BigInt(bytes.byteLength)) {
      throw new Error(`${label} changed while its prepared file was opened`)
    }
    await writeEntireBuffer(handle, bytes, 0, signal)
    await handle.sync()
    const [openedAfter, namedAfter] = await Promise.all([
      handle.stat({ bigint: true }),
      lstat(path, { bigint: true }),
    ])
    if (!sameIdentity(openedBefore, openedAfter) || !sameIdentity(openedAfter, namedAfter) ||
        openedAfter.size !== BigInt(bytes.byteLength)) {
      throw new Error(`${label} changed while bytes were materialized`)
    }
  } finally {
    await handle.close()
  }
}

async function copyVerifiedArtifact(
  sourcePath: string,
  destinationPath: string,
  expected: ArtifactIndexEntry,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  const [sourceNamed, destinationNamed] = await Promise.all([
    lstat(sourcePath, { bigint: true }),
    lstat(destinationPath, { bigint: true }),
  ])
  requireRegularFileMetadata(sourceNamed, `artifact ${expected.relativePath}`)
  requireRegularFileMetadata(destinationNamed, `prepared artifact ${expected.relativePath}`)
  if (sourceNamed.size !== BigInt(expected.byteLength) ||
      destinationNamed.size !== BigInt(expected.byteLength)) {
    throw new Error(`artifact ${expected.relativePath} length differs before guard staging`)
  }
  const source = await open(sourcePath, 'r')
  let destination: FileHandle | undefined
  try {
    destination = await open(destinationPath, 'r+')
    const [sourceBefore, destinationBefore] = await Promise.all([
      source.stat({ bigint: true }),
      destination.stat({ bigint: true }),
    ])
    if (
      !sameIdentity(sourceNamed, sourceBefore) || !sameRevision(sourceNamed, sourceBefore) ||
      !sameIdentity(destinationNamed, destinationBefore) ||
      destinationBefore.size !== BigInt(expected.byteLength)
    ) throw new Error(`artifact ${expected.relativePath} changed while opened for guard staging`)
    const digest = createHash('sha256')
    const buffer = Buffer.allocUnsafe(COPY_BUFFER_BYTES)
    let offset = 0
    while (offset < expected.byteLength) {
      signal.throwIfAborted()
      const requested = Math.min(buffer.byteLength, expected.byteLength - offset)
      const { bytesRead } = await source.read(buffer, 0, requested, offset)
      if (bytesRead < 1) throw new Error(`artifact ${expected.relativePath} ended during guard staging`)
      const chunk = buffer.subarray(0, bytesRead)
      await writeEntireBuffer(destination, chunk, offset, signal)
      digest.update(chunk)
      offset += bytesRead
    }
    await destination.sync()
    const [sourceAfter, sourceNamedAfter, destinationAfter, destinationNamedAfter] = await Promise.all([
      source.stat({ bigint: true }),
      lstat(sourcePath, { bigint: true }),
      destination.stat({ bigint: true }),
      lstat(destinationPath, { bigint: true }),
    ])
    if (
      !sameIdentity(sourceBefore, sourceAfter) || !sameIdentity(sourceAfter, sourceNamedAfter) ||
      !sameRevision(sourceBefore, sourceAfter) || !sameRevision(sourceAfter, sourceNamedAfter) ||
      !sameIdentity(destinationBefore, destinationAfter) ||
      !sameIdentity(destinationAfter, destinationNamedAfter) ||
      destinationAfter.size !== BigInt(expected.byteLength) || digest.digest('hex') !== expected.sha256
    ) throw new Error(`artifact ${expected.relativePath} changed while copied into guard staging`)
  } finally {
    await destination?.close().catch(() => undefined)
    await source.close().catch(() => undefined)
  }
}

async function writeEntireBuffer(
  handle: FileHandle,
  bytes: Uint8Array,
  initialOffset = 0,
  signal?: AbortSignal,
): Promise<void> {
  let offset = 0
  while (offset < bytes.byteLength) {
    signal?.throwIfAborted()
    const { bytesWritten } = await handle.write(
      bytes,
      offset,
      bytes.byteLength - offset,
      initialOffset + offset,
    )
    if (bytesWritten < 1) throw new Error('guard upload destination stopped accepting bytes')
    offset += bytesWritten
  }
}

async function assertDirectoryAncestry(
  root: string,
  segments: readonly string[],
  label: string,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  await requireRegularDirectory(root, `${label} root`)
  let current = root
  for (const segment of segments) {
    signal.throwIfAborted()
    current = join(current, segment)
    await requireRegularDirectory(current, `${label} directory`)
  }
}

async function requireRegularDirectory(path: string, label: string): Promise<void> {
  const metadata = await lstat(path, { bigint: true })
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || metadata.ino === 0n) {
    throw new Error(`${label} is not a regular identity-bearing directory`)
  }
}

function requireRegularFileMetadata(metadata: BigIntStats, label: string): void {
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.ino === 0n) {
    throw new Error(`${label} is not a regular identity-bearing file`)
  }
}

function snapshotMap(
  values: readonly ExistingDirectoryPublisherSnapshot[],
): Map<string, ExistingDirectoryPublisherSnapshot> {
  const result = new Map<string, ExistingDirectoryPublisherSnapshot>()
  for (const value of values) {
    if (result.has(value.relativePath)) throw new Error('native publisher repeated a snapshot path')
    result.set(value.relativePath, value)
  }
  return result
}

function requireSnapshotAuthority(
  snapshots: ReadonlyMap<string, ExistingDirectoryPublisherSnapshot>,
  authority: GuardUploadFileAuthority,
): ExistingDirectoryPublisherSnapshot {
  return requireSnapshot(snapshots, authority.relativePath, authority.byteLength, authority.sha256)
}

function requireSnapshot(
  snapshots: ReadonlyMap<string, ExistingDirectoryPublisherSnapshot>,
  relativePath: string,
  byteLength: string,
  sha256: string,
): ExistingDirectoryPublisherSnapshot {
  const snapshot = snapshots.get(relativePath)
  if (snapshot === undefined || snapshot.byteLength !== byteLength || snapshot.sha256 !== sha256 ||
      String(snapshot.bytes.byteLength) !== byteLength || sha256Bytes(snapshot.bytes) !== sha256) {
    throw new Error(`native publisher snapshot ${relativePath} differs from its manifest authority`)
  }
  return snapshot
}

function parseFileAuthority(
  value: unknown,
  expectedPath: string,
  maximumBytes: number,
  label: string,
): GuardUploadFileAuthority {
  const record = requireRecord(value, label)
  requireExactKeys(record, ['relativePath', 'byteLength', 'sha256'], [], label)
  const relativePath = requirePortableRelativePath(record.relativePath, `${label} path`)
  if (relativePath !== expectedPath) throw new Error(`${label} path is not canonical`)
  return freezeRecord({
    relativePath,
    byteLength: requireDecimal(record.byteLength, maximumBytes, `${label} byte length`),
    sha256: requireSha256(record.sha256, `${label} SHA-256`),
  })
}

function fileAuthority(relativePath: string, bytes: Uint8Array): GuardUploadFileAuthority {
  return freezeRecord({
    relativePath,
    byteLength: String(bytes.byteLength),
    sha256: sha256Bytes(bytes),
  })
}

function assertPortableInventoryCollisionFree(
  directories: readonly string[],
  files: readonly { readonly relativePath: string }[],
): void {
  const keys = new Set<string>()
  for (const path of [...directories, ...files.map(({ relativePath }) => relativePath)]) {
    const key = portablePathCollisionKey(path)
    if (keys.has(key)) throw new Error('guard upload inventory contains a portable path collision')
    keys.add(key)
  }
}

function requireExactSampleSlots(
  samples: readonly Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>[],
  runPolicy: BrowserRunPolicy,
): void {
  const expected = BROWSER_ENGINES.flatMap((browser) =>
    Array.from({ length: runPolicy.sampleCount }, (_, offset) => `${browser}/${offset + 1}`))
  const observed = samples.map(({ browser, sampleIndex }) => `${browser}/${sampleIndex}`)
  if (!sameOrderedStrings(observed, expected)) {
    throw new Error('guard upload does not contain every canonical browser/sample slot exactly once')
  }
}

function assertSampleResultSnapshot(bytes: Uint8Array, sample: BrowserSampleResult): void {
  const parsed = parseCanonicalJsonText(
    decodeUtf8(bytes, 'guard upload sample result'),
    'guard upload sample result',
  )
  if (JSON.stringify(parsed) !== JSON.stringify(sample)) {
    throw new Error('guard upload sample object differs from its immutable result bytes')
  }
}

function relativeSampleUploadRoot(
  sample: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): string {
  return `${GUARD_UPLOAD_SAMPLES_DIRECTORY}/${sample.browser}/sample-${sample.sampleIndex}`
}

function sampleUploadRoot(
  root: string,
  sample: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): string {
  return join(root, ...relativeSampleUploadRoot(sample).split('/'))
}

function artifactPathSegments(relativePath: string): readonly string[] {
  return requirePortableRelativePath(relativePath, 'guard upload artifact path').split('/')
}

function requirePortableToken(value: unknown, label: string): string {
  const token = requireString(value, label, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(token)) throw new Error(`${label} contains non-portable characters`)
  return token
}

function requireDecimal(value: unknown, maximum: number, label: string): string {
  const encoded = requireString(value, label, 32)
  if (!/^(?:0|[1-9]\d*)$/u.test(encoded)) throw new Error(`${label} is not canonical unsigned decimal`)
  const numeric = Number(encoded)
  if (!Number.isSafeInteger(numeric) || numeric > maximum) throw new Error(`${label} exceeds its byte authority`)
  return encoded
}

function requireCanonicalAbsolutePath(path: string, label: string): string {
  if (!isAbsolute(path) || resolve(path) !== path || path.includes('\0')) {
    throw new Error(`${label} must be canonical and absolute`)
  }
  return path
}

function decodeUtf8(bytes: Uint8Array, label: string): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw new Error(`${label} is not valid UTF-8`)
  }
}

function sameIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

function sameRevision(left: BigIntStats, right: BigIntStats): boolean {
  return left.size === right.size && left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs
}

function sameOrderedStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

function compareArtifactManifests(
  left: GuardUploadArtifactManifest,
  right: GuardUploadArtifactManifest,
): number {
  return comparePortablePaths(left.relativePath, right.relativePath) ||
    compareStrings(left.artifactId, right.artifactId)
}

function compareSampleInputs(left: GuardUploadSampleInput, right: GuardUploadSampleInput): number {
  return compareSampleSlots(left.sample, right.sample)
}

function compareSampleManifests(
  left: GuardUploadSampleManifest,
  right: GuardUploadSampleManifest,
): number {
  return compareSampleSlots(left, right)
}

function compareSampleSlots(
  left: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
  right: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): number {
  return compareStrings(left.browser, right.browser) || left.sampleIndex - right.sampleIndex
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
