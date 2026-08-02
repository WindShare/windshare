import { createHash } from 'node:crypto'
import {
  closeSync,
  constants,
  fstatSync,
  fsyncSync,
  lstatSync,
  mkdirSync,
  openSync,
  readSync,
  readdirSync,
  realpathSync,
  writeSync,
} from 'node:fs'
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  evaluateFinalBrowserSample,
  parseFinalGuardUploadManifest,
} from './generated-semantic/final-semantic-reducer.js'

const VERDICT_SCHEMA_VERSION = 1
const GUARD_SCHEMA_VERSION = 2
const SUITES = Object.freeze(['main', 'pion'])
const BROWSERS = Object.freeze(['chromium', 'firefox', 'webkit'])
const GUARD_KEYS = Object.freeze([
  'schemaVersion',
  'runId',
  'runPolicy',
  'suite',
  'browser',
  'sampleIndex',
  'checkoutSha',
  'sampleResultSha256',
  'artifactManifestSha256',
  'guardOutcome',
  'scanEvidence',
  'checkedArtifactIds',
  'uploadableArtifactIds',
  'quarantinedArtifactIds',
  'matches',
])
const GUARD_SCAN_KEYS = Object.freeze([
  'terminal',
  'scannedFileCount',
  'scannedArchiveEntryCount',
  'observedArchiveBytes',
  'expandedArchiveBytes',
  'observedMaximumArchiveDepth',
  'maximumArchiveBytes',
  'maximumArchiveEntries',
  'maximumExpandedArchiveBytes',
  'maximumArchiveNestingDepth',
])
const SHA256_PATTERN = /^[0-9a-f]{64}$/u
const CHECKOUT_SHA_PATTERN = /^[0-9a-f]{40}$/u
const PORTABLE_TOKEN_PATTERN = /^[A-Za-z0-9._-]+$/u
const MAXIMUM_UPLOAD_MANIFEST_BYTES = 8 * 1_024 * 1_024
const MAXIMUM_SAMPLE_RESULT_BYTES = 16 * 1_024 * 1_024
const MAXIMUM_GUARD_RESULT_BYTES = 1 * 1_024 * 1_024
const MAXIMUM_ARCHIVE_BYTES = 536_870_912
const MAXIMUM_ARCHIVE_ENTRIES = 10_000
const MAXIMUM_EXPANDED_ARCHIVE_BYTES = 2_147_483_648
const MAXIMUM_ARCHIVE_NESTING_DEPTH = 1
const COPY_BUFFER_BYTES = 64 * 1_024

export async function evaluateBrowserGate(options) {
  const violations = []
  const suiteEvidence = {}
  const suiteOutcomes = {}
  if (!PORTABLE_TOKEN_PATTERN.test(options.runId ?? '')) {
    violations.push('browser gate run ID is not a portable token')
  }
  if (!CHECKOUT_SHA_PATTERN.test(options.checkoutSha ?? '')) {
    violations.push('browser gate checkout SHA is not lowercase 40-hex')
  }

  for (const suite of SUITES) {
    const state = options.suites[suite]
    const guardOutcome = canonicalGuardOutcome(state.guardOutcome)
    const manifestSha256 = state.manifestSha256 === '' ? null : state.manifestSha256
    const manifestByteLength = canonicalManifestByteLength(state.manifestByteLength)
    suiteOutcomes[suite] = Object.freeze({
      jobOutcome: state.jobOutcome,
      guardOutcome,
      downloadOutcome: state.downloadOutcome,
      manifestSha256,
      manifestByteLength,
    })
    if (state.jobOutcome !== 'success') {
      violations.push(`${suite} suite job outcome is ${state.jobOutcome || 'missing'}`)
    }
    if (guardOutcome !== 'passed') {
      violations.push(`${suite} guard outcome is ${guardOutcome}`)
    }
    if (state.downloadOutcome !== 'success') {
      violations.push(`${suite} sealed upload download outcome is ${state.downloadOutcome || 'missing'}`)
    }
    if (guardOutcome !== 'passed' || state.downloadOutcome !== 'success') {
      suiteEvidence[suite] = null
      continue
    }
    if (manifestSha256 === null || !SHA256_PATTERN.test(manifestSha256)) {
      violations.push(`${suite} authenticated manifest SHA-256 is missing or invalid`)
      suiteEvidence[suite] = null
      continue
    }
    if (manifestByteLength === null) {
      violations.push(`${suite} authenticated manifest byte length is missing or invalid`)
      suiteEvidence[suite] = null
      continue
    }
    try {
      suiteEvidence[suite] = await verifySuiteUpload({
        suite,
        root: state.root,
        manifestSha256,
        manifestByteLength,
        runId: options.runId,
        checkoutSha: options.checkoutSha,
      })
    } catch (cause) {
      violations.push(`${suite} sealed upload is invalid: ${boundedError(cause)}`)
      suiteEvidence[suite] = null
    }
  }

  const available = SUITES.map((suite) => suiteEvidence[suite]).filter((value) => value !== null)
  const runPolicy = available[0]?.manifest.runPolicy ?? null
  if (available.length === 2 && !sameJson(
    available[0].manifest.runPolicy,
    available[1].manifest.runPolicy,
  )) violations.push('main and Pion uploads bind different run policies')

  const topologyAuthority = buildTopologyAuthority(suiteEvidence, violations)
  correlateSuites(suiteEvidence, violations)
  const samples = summarizeSamples(suiteEvidence, runPolicy)
  const canonicalViolations = Object.freeze([...new Set(violations)].sort(compareStrings))
  return Object.freeze({
    schemaVersion: VERDICT_SCHEMA_VERSION,
    verdictKind: 'browser-gate',
    runId: options.runId,
    checkoutSha: options.checkoutSha,
    browsers: BROWSERS,
    runPolicy,
    suiteOutcomes: Object.freeze(suiteOutcomes),
    topologyAuthority,
    verdict: canonicalViolations.length === 0 ? 'passed' : 'failed',
    violations: canonicalViolations,
    samples,
  })
}

async function verifySuiteUpload({
  suite,
  root,
  manifestSha256,
  manifestByteLength,
  runId,
  checkoutSha,
}) {
  const uploadRoot = requireSealedRoot(root, `${suite} sealed upload root`)
  const manifestSnapshot = readStableFile(
    join(uploadRoot, 'manifest.json'),
    MAXIMUM_UPLOAD_MANIFEST_BYTES,
    `${suite} upload manifest`,
  )
  if (manifestSnapshot.sha256 !== manifestSha256) {
    throw new Error('manifest bytes differ from the workflow-provided digest')
  }
  if (String(manifestSnapshot.bytes.byteLength) !== manifestByteLength) {
    throw new Error('manifest bytes differ from the workflow-provided byte length')
  }
  const manifest = parseManifest(manifestSnapshot.bytes)
  if (
    manifest.runId !== runId || manifest.checkoutSha !== checkoutSha || manifest.suite !== suite
  ) throw new Error('manifest identity differs from the workflow authority')

  const expectedDirectories = new Set(['samples', 'topology'])
  const expectedFiles = new Map([
    ['manifest.json', {
      byteLength: String(manifestSnapshot.bytes.byteLength),
      sha256: manifestSha256,
    }],
    [manifest.topology.profile.relativePath, {
      byteLength: manifest.topology.profile.byteLength,
      sha256: manifest.topology.profile.sha256,
    }],
    [manifest.topology.resolution.relativePath, {
      byteLength: manifest.topology.resolution.byteLength,
      sha256: manifest.topology.resolution.sha256,
    }],
  ])
  for (const sample of manifest.samples) {
    const sampleRoot = `samples/${sample.browser}/sample-${sample.sampleIndex}`
    expectedDirectories.add(`samples/${sample.browser}`)
    expectedDirectories.add(sampleRoot)
    expectedDirectories.add(`${sampleRoot}/attachments`)
    expectedFiles.set(`${sampleRoot}/result.json`, {
      byteLength: sample.sampleResultByteLength,
      sha256: sample.sampleResultSha256,
    })
    expectedFiles.set(`${sampleRoot}/guard.json`, {
      byteLength: sample.guardResultByteLength,
      sha256: sample.guardResultSha256,
    })
    for (const artifact of sample.artifacts) {
      const path = `${sampleRoot}/attachments/${artifact.relativePath}`
      expectedFiles.set(path, { byteLength: artifact.byteLength, sha256: artifact.sha256 })
      const segments = path.split('/')
      for (let index = 1; index < segments.length; index += 1) {
        expectedDirectories.add(segments.slice(0, index).join('/'))
      }
    }
  }
  verifyExactTree(uploadRoot, expectedDirectories, expectedFiles)

  const topologyProfileJson = decodeUtf8(
    readStableFile(
      join(uploadRoot, ...manifest.topology.profile.relativePath.split('/')),
      Number(manifest.topology.profile.byteLength),
      'sealed topology profile',
    ).bytes,
    'sealed topology profile',
  )
  const topologyResolutionJson = decodeUtf8(
    readStableFile(
      join(uploadRoot, ...manifest.topology.resolution.relativePath.split('/')),
      Number(manifest.topology.resolution.byteLength),
      'sealed topology resolution',
    ).bytes,
    'sealed topology resolution',
  )
  const samples = []
  for (const sample of manifest.samples) {
    const sampleRoot = join(
      uploadRoot,
      'samples',
      sample.browser,
      `sample-${sample.sampleIndex}`,
    )
    const resultSnapshot = readStableFile(
      join(sampleRoot, 'result.json'),
      MAXIMUM_SAMPLE_RESULT_BYTES,
      'sealed browser sample result',
    )
    const guardSnapshot = readStableFile(
      join(sampleRoot, 'guard.json'),
      MAXIMUM_GUARD_RESULT_BYTES,
      'sealed browser artifact guard',
    )
    if (
      String(resultSnapshot.byteLength) !== sample.sampleResultByteLength ||
      String(guardSnapshot.byteLength) !== sample.guardResultByteLength ||
      resultSnapshot.sha256 !== sample.sampleResultSha256 ||
      guardSnapshot.sha256 !== sample.guardResultSha256
    ) throw new Error(`sealed contract digest mismatch for ${sample.browser}/${sample.sampleIndex}`)
    const unverifiedResult = parseCanonicalJson(resultSnapshot.bytes, 'browser sample result')
    const guard = parseCanonicalJson(guardSnapshot.bytes, 'browser artifact guard')
    const { result } = await evaluateFinalBrowserSample({
      result: unverifiedResult,
      topologyProfileJson,
      topologyResolutionJson,
      topologyProfileSha256: manifest.topology.profile.sha256,
      topologyResolutionSha256: manifest.topology.resolution.sha256,
    })
    requireSameJson(
      result.artifacts,
      sample.artifacts.map((artifact) => ({
        ...artifact,
        byteLength: Number(artifact.byteLength),
      })),
      'result artifact index',
    )
    validateGuard(guard, manifest, sample)
    samples.push(Object.freeze({
      suite,
      browser: sample.browser,
      sampleIndex: sample.sampleIndex,
      result,
      guard,
    }))
  }
  return Object.freeze({ uploadRoot, manifestSha256, manifest, samples: Object.freeze(samples) })
}

function parseManifest(bytes) {
  const manifest = parseCanonicalJson(bytes, 'guard upload manifest')
  return parseFinalGuardUploadManifest(JSON.stringify(manifest))
}

function validateGuard(guard, manifest, sample) {
  exactKeys(guard, GUARD_KEYS, 'browser artifact guard')
  requireLiteral(guard.schemaVersion, GUARD_SCHEMA_VERSION, 'guard schema version')
  requireLiteral(guard.runId, manifest.runId, 'guard run ID')
  requireLiteral(guard.suite, manifest.suite, 'guard suite')
  requireLiteral(guard.browser, sample.browser, 'guard browser')
  requireLiteral(guard.sampleIndex, sample.sampleIndex, 'guard sample index')
  requireLiteral(guard.checkoutSha, manifest.checkoutSha, 'guard checkout SHA')
  requireSameJson(guard.runPolicy, manifest.runPolicy, 'guard run policy')
  requireLiteral(guard.sampleResultSha256, sample.sampleResultSha256, 'guard result digest')
  requireLiteral(
    guard.artifactManifestSha256,
    sample.artifactManifestSha256,
    'guard artifact manifest digest',
  )
  requireLiteral(guard.guardOutcome, 'passed', 'guard outcome')
  const artifactIds = sample.artifacts.map((artifact) => artifact.artifactId).sort(compareStrings)
  requireOrderedStrings(guard.checkedArtifactIds, artifactIds, 'guard checked artifacts')
  requireOrderedStrings(guard.uploadableArtifactIds, artifactIds, 'guard uploadable artifacts')
  if (!Array.isArray(guard.quarantinedArtifactIds) || guard.quarantinedArtifactIds.length !== 0) {
    throw new Error('passed guard quarantines artifacts')
  }
  if (!Array.isArray(guard.matches) || guard.matches.length !== 0) {
    throw new Error('passed guard carries secret matches')
  }
  exactKeys(guard.scanEvidence, GUARD_SCAN_KEYS, 'guard scan evidence')
  const scan = guard.scanEvidence
  requireLiteral(scan.terminal, 'completed', 'guard scan terminal')
  requireLiteral(scan.scannedFileCount, artifactIds.length, 'guard scanned file count')
  requireSafeInteger(scan.scannedArchiveEntryCount, 0, MAXIMUM_ARCHIVE_ENTRIES, 'archive entry count')
  requireSafeInteger(scan.observedArchiveBytes, 0, MAXIMUM_ARCHIVE_BYTES, 'archive byte count')
  requireSafeInteger(
    scan.expandedArchiveBytes,
    0,
    MAXIMUM_EXPANDED_ARCHIVE_BYTES,
    'expanded archive byte count',
  )
  requireSafeInteger(
    scan.observedMaximumArchiveDepth,
    0,
    MAXIMUM_ARCHIVE_NESTING_DEPTH,
    'archive depth',
  )
  requireLiteral(scan.maximumArchiveBytes, MAXIMUM_ARCHIVE_BYTES, 'guard archive byte limit')
  requireLiteral(scan.maximumArchiveEntries, MAXIMUM_ARCHIVE_ENTRIES, 'guard archive entry limit')
  requireLiteral(
    scan.maximumExpandedArchiveBytes,
    MAXIMUM_EXPANDED_ARCHIVE_BYTES,
    'guard expanded archive limit',
  )
  requireLiteral(
    scan.maximumArchiveNestingDepth,
    MAXIMUM_ARCHIVE_NESTING_DEPTH,
    'guard archive nesting limit',
  )
}

function buildTopologyAuthority(suiteEvidence, violations) {
  const authority = {}
  for (const suite of SUITES) {
    const evidence = suiteEvidence[suite]
    if (evidence === null) {
      authority[suite] = null
      continue
    }
    const first = evidence.samples[0]?.result
    if (first === undefined) {
      violations.push(`${suite} upload has no topology authority`)
      authority[suite] = null
      continue
    }
    const value = Object.freeze({
      topologyId: first.topologyId,
      topologyProfileSha256: first.topologyProfileSha256,
      topologyResolutionSha256: first.topologyResolutionSha256,
    })
    if (evidence.samples.some(({ result }) =>
      result.topologyId !== value.topologyId ||
      result.topologyProfileSha256 !== value.topologyProfileSha256 ||
      result.topologyResolutionSha256 !== value.topologyResolutionSha256)) {
      violations.push(`${suite} samples do not share one topology lock`)
    }
    authority[suite] = value
  }
  if (
    authority.main !== null && authority.pion !== null &&
    (authority.main.topologyId !== authority.pion.topologyId ||
      authority.main.topologyProfileSha256 !== authority.pion.topologyProfileSha256)
  ) violations.push('main and Pion samples do not share one topology profile authority')
  return Object.freeze(authority)
}

function correlateSuites(suiteEvidence, violations) {
  if (suiteEvidence.main === null || suiteEvidence.pion === null) return
  const main = new Map(suiteEvidence.main.samples.map((sample) => [sampleKey(sample), sample.result]))
  for (const pionSample of suiteEvidence.pion.samples) {
    const key = sampleKey(pionSample)
    const mainResult = main.get(key)
    if (mainResult === undefined) {
      violations.push(`main result is missing for correlated slot ${key}`)
      continue
    }
    const pionResult = pionSample.result
    if (
      mainResult.rtcCapability !== pionResult.rtcCapability ||
      mainResult.capabilityEvidence.apiPresence !== pionResult.capabilityEvidence.apiPresence
    ) violations.push(`RTC capability differs across suites for ${key}`)
    if (
      pionResult.rtcCapability === 'unavailable' &&
      (mainResult.peerAttemptOutcome !== 'not-started' ||
        mainResult.routeEvidence?.mode !== 'relay-only')
    ) violations.push(`Pion N/A lacks main relay fallback for ${key}`)
  }
}

function summarizeSamples(suiteEvidence, runPolicy) {
  if (runPolicy === null) return Object.freeze([])
  const summaries = []
  for (const suite of SUITES) {
    const evidence = suiteEvidence[suite]
    if (evidence === null || !sameJson(evidence.manifest.runPolicy, runPolicy)) {
      for (const browser of BROWSERS) {
        for (let sampleIndex = 1; sampleIndex <= runPolicy.sampleCount; sampleIndex += 1) {
          summaries.push(Object.freeze({
            suite,
            browser,
            sampleIndex,
            summaryKind: 'infrastructure-unavailable',
          }))
        }
      }
      continue
    }
    for (const sample of evidence.samples) {
      summaries.push(Object.freeze({
        suite,
        browser: sample.browser,
        sampleIndex: sample.sampleIndex,
        summaryKind: 'evidence',
        resultPresent: true,
        guardPresent: true,
        accepted: true,
      }))
    }
  }
  return Object.freeze(summaries)
}

function verifyExactTree(root, expectedDirectories, expectedFiles) {
  const observedDirectories = new Set()
  const observedFiles = new Set()
  enumerateTree(root, '', observedDirectories, observedFiles)
  if (!sameStringSets(observedDirectories, expectedDirectories)) {
    throw new Error('sealed upload directory inventory is not exact')
  }
  if (!sameStringSets(observedFiles, new Set(expectedFiles.keys()))) {
    throw new Error('sealed upload file inventory is not exact')
  }
  for (const [relativePath, expected] of expectedFiles) {
    const fullPath = join(root, ...relativePath.split('/'))
    const maximumBytes = expected.byteLength === null
      ? relativePath.endsWith('/guard.json')
        ? MAXIMUM_GUARD_RESULT_BYTES
        : MAXIMUM_SAMPLE_RESULT_BYTES
      : Math.max(Number(expected.byteLength), 1)
    const digest = digestStableFile(fullPath, maximumBytes, `sealed file ${relativePath}`)
    if (
      (expected.byteLength !== null && String(digest.byteLength) !== expected.byteLength) ||
      digest.sha256 !== expected.sha256
    ) throw new Error(`sealed file ${relativePath} differs from its manifest authority`)
  }
}

function enumerateTree(root, relativeDirectory, directories, files) {
  const directory = relativeDirectory === '' ? root : join(root, ...relativeDirectory.split('/'))
  requireRegularDirectory(directory, 'sealed upload directory')
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const relativePath = relativeDirectory === ''
      ? entry.name
      : `${relativeDirectory}/${entry.name}`
    const fullPath = join(directory, entry.name)
    const metadata = lstatSync(fullPath)
    if (entry.isSymbolicLink() || metadata.isSymbolicLink()) {
      throw new Error('sealed upload contains a symbolic link or junction')
    }
    if (entry.isDirectory() && metadata.isDirectory()) {
      directories.add(relativePath)
      enumerateTree(root, relativePath, directories, files)
    } else if (entry.isFile() && metadata.isFile()) {
      files.add(relativePath)
    } else {
      throw new Error('sealed upload contains an unsupported filesystem entry')
    }
  }
}

function requireSealedRoot(value, label) {
  if (typeof value !== 'string' || value === '') throw new Error(`${label} is missing`)
  const path = resolve(value)
  requireRegularDirectory(path, label)
  if (resolve(realpathSync(path)) !== path) throw new Error(`${label} is not canonical`)
  return path
}

function requireRegularDirectory(path, label) {
  const metadata = lstatSync(path)
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new Error(`${label} must be a non-symbolic directory`)
  }
}

function readStableFile(path, maximumBytes, label) {
  const metadata = lstatSync(path, { bigint: true })
  requireRegularFileMetadata(metadata, maximumBytes, label)
  const descriptor = openSync(path, constants.O_RDONLY)
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    requireSameIdentity(metadata, openedBefore, label)
    requireSameRevision(metadata, openedBefore, label)
    const byteLength = Number(openedBefore.size)
    const bytes = Buffer.alloc(byteLength)
    let offset = 0
    while (offset < byteLength) {
      const count = readSync(descriptor, bytes, offset, byteLength - offset, offset)
      if (count === 0) throw new Error(`${label} ended before its recorded length`)
      offset += count
    }
    const openedAfter = fstatSync(descriptor, { bigint: true })
    const namedAfter = lstatSync(path, { bigint: true })
    requireSameIdentity(openedBefore, openedAfter, label)
    requireSameIdentity(openedAfter, namedAfter, label)
    requireSameRevision(openedBefore, openedAfter, label)
    requireSameRevision(openedAfter, namedAfter, label)
    return Object.freeze({
      bytes,
      byteLength,
      sha256: sha256Bytes(bytes),
    })
  } finally {
    closeSync(descriptor)
  }
}

function digestStableFile(path, maximumBytes, label) {
  const metadata = lstatSync(path, { bigint: true })
  requireRegularFileMetadata(metadata, maximumBytes, label)
  const descriptor = openSync(path, constants.O_RDONLY)
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    requireSameIdentity(metadata, openedBefore, label)
    requireSameRevision(metadata, openedBefore, label)
    const digest = createHash('sha256')
    const buffer = Buffer.allocUnsafe(COPY_BUFFER_BYTES)
    let offset = 0
    const byteLength = Number(openedBefore.size)
    while (offset < byteLength) {
      const count = readSync(
        descriptor,
        buffer,
        0,
        Math.min(buffer.byteLength, byteLength - offset),
        offset,
      )
      if (count === 0) throw new Error(`${label} ended before its recorded length`)
      digest.update(buffer.subarray(0, count))
      offset += count
    }
    const openedAfter = fstatSync(descriptor, { bigint: true })
    const namedAfter = lstatSync(path, { bigint: true })
    requireSameIdentity(openedBefore, openedAfter, label)
    requireSameIdentity(openedAfter, namedAfter, label)
    requireSameRevision(openedBefore, openedAfter, label)
    requireSameRevision(openedAfter, namedAfter, label)
    return Object.freeze({ byteLength, sha256: digest.digest('hex') })
  } finally {
    closeSync(descriptor)
  }
}

function requireRegularFileMetadata(metadata, maximumBytes, label) {
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new Error(`${label} must be a regular non-symbolic file`)
  }
  if (metadata.size < 0n || metadata.size > BigInt(maximumBytes)) {
    throw new Error(`${label} exceeds its byte authority`)
  }
}

function requireSameIdentity(left, right, label) {
  if (left.dev !== right.dev || left.ino !== right.ino) {
    throw new Error(`${label} changed filesystem identity while read`)
  }
}

function requireSameRevision(left, right, label) {
  if (
    left.size !== right.size || left.mtimeNs !== right.mtimeNs ||
    left.ctimeNs !== right.ctimeNs || left.mode !== right.mode
  ) throw new Error(`${label} changed revision while read`)
}

function parseCanonicalJson(bytes, label) {
  const encoded = decodeUtf8(bytes, label)
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error(`${label} is not valid JSON`, { cause })
  }
  if (!isRecord(value) || JSON.stringify(value) !== encoded) {
    throw new Error(`${label} is not canonical JSON`)
  }
  return value
}

function decodeUtf8(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw new Error(`${label} is not valid UTF-8`)
  }
}

function exactKeys(value, required, label, optional = []) {
  if (!isRecord(value)) throw new Error(`${label} must be an object`)
  const expected = new Set([...required, ...optional])
  const keys = Object.keys(value)
  if (required.some((key) => !Object.hasOwn(value, key)) || keys.some((key) => !expected.has(key))) {
    throw new Error(`${label} does not have its exact keys`)
  }
}

function requireLiteral(value, expected, label) {
  if (value !== expected) throw new Error(`${label} is not the required literal`)
  return value
}

function requireEnum(value, values, label) {
  if (!values.includes(value)) throw new Error(`${label} is invalid`)
  return value
}

function requireBoundedString(value, maximumLength, label) {
  if (typeof value !== 'string' || value.length === 0 || value.length > maximumLength) {
    throw new Error(`${label} must be bounded nonempty text`)
  }
  return value
}

function requirePortableToken(value, label) {
  requireBoundedString(value, 128, label)
  if (!PORTABLE_TOKEN_PATTERN.test(value)) throw new Error(`${label} is not portable`)
  return value
}

function requireSha256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(`${label} is not lowercase 64-hex`)
  }
  return value
}

function requireSafeInteger(value, minimum, maximum, label) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new Error(`${label} is outside its integer authority`)
  }
  return value
}

function requireOrderedStrings(actual, expected, label) {
  if (!Array.isArray(actual) || !actual.every((value) => typeof value === 'string') ||
      !sameOrderedStrings(actual, expected)) throw new Error(`${label} is not exact and ordered`)
}

function requireSameJson(actual, expected, label) {
  if (!sameJson(actual, expected)) throw new Error(`${label} differs from its authority`)
}

function sameJson(left, right) {
  return JSON.stringify(left) === JSON.stringify(right)
}

function sameOrderedStrings(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

function sameStringSets(left, right) {
  return left.size === right.size && [...left].every((value) => right.has(value))
}

function compareStrings(left, right) {
  if (left === right) return 0
  return left < right ? -1 : 1
}

function sampleKey(sample) {
  return `${sample.browser}/${sample.sampleIndex}`
}

function sha256Bytes(value) {
  return createHash('sha256').update(value).digest('hex')
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function canonicalGuardOutcome(value) {
  if (value === 'passed' || value === 'quarantined' || value === 'failed') return value
  return value === '' || value === undefined ? 'missing' : `invalid:${value}`
}

function canonicalManifestByteLength(value) {
  if (typeof value !== 'string' || !/^[1-9]\d*$/u.test(value)) return null
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed) || parsed > MAXIMUM_UPLOAD_MANIFEST_BYTES) return null
  return value
}

function boundedError(_cause) {
  // Verdict publication must remain total even when a dependency throws an
  // active Error/Proxy. Detailed causes stay in the internal error chain.
  return 'dependency-operation-failed'
}

function parseCliArguments(argv) {
  const singles = new Map()
  const maps = new Map([
    ['suite-job-outcome', new Map()],
    ['guard-outcome', new Map()],
    ['manifest-sha256', new Map()],
    ['manifest-byte-length', new Map()],
    ['download-outcome', new Map()],
  ])
  const allowedSingles = new Set([
    'run-id',
    'checkout-sha',
    'main-root',
    'pion-root',
    'output',
  ])
  for (let index = 0; index < argv.length; index += 2) {
    const token = argv[index]
    const value = argv[index + 1]
    if (typeof token !== 'string' || !token.startsWith('--') || value === undefined) {
      throw new Error('browser verdict arguments must be --name value pairs')
    }
    const name = token.slice(2)
    if (maps.has(name)) {
      const separator = value.indexOf('=')
      if (separator < 1) throw new Error(`${name} must be suite=value`)
      const suite = value.slice(0, separator)
      requireEnum(suite, SUITES, `${name} suite`)
      const target = maps.get(name)
      if (target.has(suite)) throw new Error(`${name} repeats suite ${suite}`)
      target.set(suite, value.slice(separator + 1))
    } else {
      if (!allowedSingles.has(name)) throw new Error(`unknown browser verdict option --${name}`)
      if (singles.has(name)) throw new Error(`browser verdict option --${name} appears twice`)
      singles.set(name, value)
    }
  }
  for (const name of allowedSingles) {
    if (!singles.has(name)) throw new Error(`missing browser verdict option --${name}`)
  }
  for (const [name, values] of maps) {
    for (const suite of SUITES) {
      if (!values.has(suite)) throw new Error(`${name} lacks suite ${suite}`)
    }
  }
  return Object.freeze({
    runId: singles.get('run-id'),
    checkoutSha: singles.get('checkout-sha'),
    output: singles.get('output'),
    suites: Object.freeze(Object.fromEntries(SUITES.map((suite) => [suite, Object.freeze({
      root: singles.get(`${suite}-root`),
      jobOutcome: maps.get('suite-job-outcome').get(suite),
      guardOutcome: maps.get('guard-outcome').get(suite),
      manifestSha256: maps.get('manifest-sha256').get(suite),
      manifestByteLength: maps.get('manifest-byte-length').get(suite),
      downloadOutcome: maps.get('download-outcome').get(suite),
    })]))),
  })
}

function writeVerdict(path, verdict) {
  const output = resolve(path)
  if (!isAbsolute(output)) throw new Error('browser verdict output path is not absolute')
  mkdirSync(dirname(output), { recursive: true, mode: 0o700 })
  const descriptor = openSync(output, 'w', 0o600)
  try {
    const bytes = Buffer.from(`${JSON.stringify(verdict)}\n`, 'utf8')
    let offset = 0
    while (offset < bytes.byteLength) offset += writeSync(descriptor, bytes, offset)
    fsyncSync(descriptor)
  } finally {
    closeSync(descriptor)
  }
}

export async function runBrowserVerdictCli(argv = process.argv.slice(2)) {
  try {
    const options = parseCliArguments(argv)
    const verdict = await evaluateBrowserGate(options)
    writeVerdict(options.output, verdict)
    process.stdout.write(`${JSON.stringify({
      component: 'browser-gate-verdict',
      verdict: verdict.verdict,
      violationCount: verdict.violations.length,
      output: resolve(options.output),
    })}\n`)
    return verdict.verdict === 'passed' ? 0 : 1
  } catch (cause) {
    process.stderr.write(`browser verdict failed: ${boundedError(cause)}\n`)
    return 1
  }
}

if (resolve(process.argv[1] ?? '') === fileURLToPath(import.meta.url)) {
  process.exitCode = await runBrowserVerdictCli()
}
