import {
  artifactIdForManifest,
  artifactManifestSha256,
  sha256Bytes,
} from '../manifest.ts'
import {
  GUARD_MAXIMUM_ARTIFACT_FILES,
  GUARD_MAXIMUM_ARTIFACT_FILE_BYTES,
  GUARD_MAXIMUM_TOTAL_ARTIFACT_BYTES,
  type ArtifactGuardResult,
} from '../guard-result.ts'
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
} from '../../contract/json.ts'
import { parseCanonicalJsonText } from '../../contract/strict-json.ts'
import {
  comparePortablePaths,
  portablePathCollisionKey,
  requirePortableRelativePath,
} from '../../filesystem/portable-path.ts'
import { ARTIFACT_KINDS, type ArtifactIndexEntry } from '../../result.ts'
import {
  assertBrowserRunPolicyEqual,
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from '../../run-policy.ts'
import { BROWSER_ENGINES, BROWSER_SUITES } from '../../vocabulary.ts'
import {
  GUARD_UPLOAD_GUARD_FILENAME,
  GUARD_UPLOAD_MANIFEST_FILENAME,
  GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION,
  GUARD_UPLOAD_RESULT_FILENAME,
  GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH,
  GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH,
  MAXIMUM_GUARD_RESULT_BYTES,
  MAXIMUM_SAMPLE_RESULT_BYTES,
  MAXIMUM_TOPOLOGY_BYTES,
  type GuardUploadArtifactManifest,
  type GuardUploadFileAuthority,
  type GuardUploadManifest,
  type GuardUploadSampleInput,
  type GuardUploadSampleManifest,
  type GuardUploadTopologyManifest,
} from './contract.ts'
import { relativeSampleUploadRoot } from './layout.ts'

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

export function snapshotPathsForManifest(manifest: GuardUploadManifest): readonly string[] {
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

export function canonicalArtifactManifests(
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

export function assertGuardMatchesSampleManifest(
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

export function fileAuthority(relativePath: string, bytes: Uint8Array): GuardUploadFileAuthority {
  return freezeRecord({
    relativePath,
    byteLength: String(bytes.byteLength),
    sha256: sha256Bytes(bytes),
  })
}

export function assertPortableInventoryCollisionFree(
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

export function requireExactSampleSlots(
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

export function requirePortableToken(value: unknown, label: string): string {
  const token = requireString(value, label, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(token)) throw new Error(`${label} contains non-portable characters`)
  return token
}

export function requireDecimal(value: unknown, maximum: number, label: string): string {
  const encoded = requireString(value, label, 32)
  if (!/^(?:0|[1-9]\d*)$/u.test(encoded)) throw new Error(`${label} is not canonical unsigned decimal`)
  const numeric = Number(encoded)
  if (!Number.isSafeInteger(numeric) || numeric > maximum) throw new Error(`${label} exceeds its byte authority`)
  return encoded
}

function compareArtifactManifests(
  left: GuardUploadArtifactManifest,
  right: GuardUploadArtifactManifest,
): number {
  return comparePortablePaths(left.relativePath, right.relativePath) ||
    compareStrings(left.artifactId, right.artifactId)
}

export function compareSampleInputs(left: GuardUploadSampleInput, right: GuardUploadSampleInput): number {
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

export function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

function sameOrderedStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
