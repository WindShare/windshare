import { randomBytes } from 'node:crypto'
import { basename, dirname, join } from 'node:path'

import { artifactManifestSha256, sha256Bytes } from '../manifest.ts'
import {
  requireVerifiedProcessSettlementSet,
} from '../settlement-receipt.ts'
import { GuardExecutionLease } from '../../execution/guard-execution-lease.ts'
import { readStableRegularFileSnapshot } from '../../filesystem/snapshot.ts'
import {
  GuardUploadDirectoryPublisherUnsettledError,
  requireGuardUploadDirectoryPublisher,
  type GuardUploadDirectoryPublisher,
} from '../directory-publisher.ts'
import type { ArtifactGuardResult } from '../guard-result.ts'
import { parseArtifactGuardResult } from '../guard-result.ts'
import {
  freezeRecord,
  requireCheckoutSha,
  requireEnum,
  requireSha256,
} from '../../contract/json.ts'
import {
  comparePortablePaths,
  requirePortableRelativePath,
} from '../../filesystem/portable-path.ts'
import {
  assertBrowserRunPolicyEqual,
  parseBrowserRunPolicy,
} from '../../run-policy.ts'
import { parseCanonicalJsonText } from '../../contract/strict-json.ts'
import {
  BROWSER_SUITES,
  type BrowserSuite,
} from '../../vocabulary.ts'
import type {
  ExistingDirectoryPublisherInventory,
  ExistingDirectoryPublisherResponse,
  PublisherHelperFailureCode,
} from '../../../browser-network-matrix/cli/publisher-helper-protocol.ts'
import {
  GUARD_UPLOAD_ATTACHMENTS_DIRECTORY,
  GUARD_UPLOAD_GUARD_FILENAME,
  GUARD_UPLOAD_MANIFEST_FILENAME,
  GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION,
  GUARD_UPLOAD_OUTPUT_NAME,
  GUARD_UPLOAD_RESULT_FILENAME,
  GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH,
  GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH,
  MAXIMUM_UPLOAD_MANIFEST_BYTES,
  type GuardUploadHooks,
  type GuardUploadManifest,
  type GuardUploadSampleInput,
  type GuardUploadSampleManifest,
  type GuardUploadSampleSnapshot,
  type GuardUploadSelection,
  type SealGuardUploadSuiteOptions,
} from './contract.ts'
import {
  artifactPathSegments,
  relativeSampleUploadRoot,
  requireCanonicalAbsolutePath,
  sampleUploadRoot,
} from './layout.ts'
import {
  assertGuardMatchesSampleManifest,
  assertPortableInventoryCollisionFree,
  canonicalArtifactManifests,
  compareSampleInputs,
  parseGuardUploadManifest,
  requireDecimal,
  requireExactSampleSlots,
  requirePortableToken,
  snapshotPathsForManifest,
} from './manifest-codec.ts'
import {
  assertDirectoryAncestry,
  assertSampleResultSnapshot,
  copyVerifiedArtifact,
  decodeUtf8,
  requireSnapshot,
  requireSnapshotAuthority,
  snapshotMap,
  writePreparedFile,
} from './prepared-tree-io.ts'
import {
  canonicalTopology,
  validateTopologyBytes,
  type CanonicalTopology,
} from './topology.ts'

const PRIVATE_UPLOAD_PREFIX = '.browser-evidence-upload-'
const STAGING_NAME_PATTERN = /^\.browser-evidence-upload-[a-f0-9]{32}$/u

interface SamplePayload {
  readonly input: GuardUploadSampleInput
  readonly guardBytes: Uint8Array
  readonly manifest: GuardUploadSampleManifest
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
