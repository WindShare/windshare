import { createHash, randomUUID } from 'node:crypto'
import {
  closeSync,
  constants as fsConstants,
  fstatSync,
  fsyncSync,
  linkSync,
  lstatSync,
  openSync,
  opendirSync,
  readFileSync,
  realpathSync,
  unlinkSync,
  writeSync,
} from 'node:fs'
import { basename, dirname, isAbsolute, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { types as nodeTypes } from 'node:util'

import { parseNetworkMatrixAggregateJson } from '../../../web/scripts/browser-network-matrix/aggregate.ts'
import { loadNetworkMatrixRegistry } from '../../../web/scripts/browser-network-matrix/manifest.ts'
import { parseNetworkRunResultJson } from '../../../web/scripts/browser-network-matrix/result.ts'

export const NETWORK_COMPLETION_SCHEMA = 'windshare.browser-network-matrix.completion/v1'
export const PREPARED_INPUT_SCHEMA = 'windshare.browser-network-matrix.prepared-input/v1'

const EXPECTED_NODE_VERSION = '24.16.0'
const EXPECTED_IDENTITIES = 45
const CHECKOUT_SHA_PATTERN = /^[a-f0-9]{40}$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const RUN_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const RETAINED_LINUX_FILE_PATTERN = /^\/proc\/[1-9][0-9]*\/fd\/[0-9]+$/u
const MAXIMUM_COMPLETION_BYTES = 64 * 1024
const MAXIMUM_MANIFEST_BYTES = 1024 * 1024
const MAXIMUM_EVIDENCE_BYTES = 256 * 1024 * 1024
const COMPLETION_FILE_NAME = 'browser-network-completion.json'
const PRODUCER_MANIFEST_FILE_NAME = 'browser-network-producer-manifest.json'
const RUNTIME_HELPER_MANIFEST_FILE_NAME = 'browser-network-runtime-helper-manifest.json'
const EXECUTION_BINDING_FILE_NAME = 'browser-network-execution-binding.json'
const EXECUTION_BINDING_SCHEMA = 'windshare.browser-network-matrix.execution-binding/v1'
const SCHEDULED_MANIFEST_RELATIVE_PATH = 'testdata/browser-network-matrix/scheduled-hard.manifest.v2.json'
const EXPECTED_PROFILE_FILE_NAMES = Object.freeze([
  'profiles/scheduled-coturn.v2.json',
  'profiles/scheduled-public-stun.v2.json',
  'profiles/scheduled-restricted-udp.v2.json',
])
const COMPLETION_KEYS = Object.freeze([
  'schemaVersion',
  'runId',
  'checkoutSha',
  'manifestSha256',
  'runSha256',
  'aggregateSha256',
  'producerManifestSha256',
  'runtimeBundleSha256',
  'runtimeHelperManifestSha256',
  'runtimeConfigSha256',
  'executionBindingSha256',
  'artifactDirectory',
  'runFile',
  'aggregateFile',
  'producerManifestFile',
  'runtimeHelperManifestFile',
  'executionBindingFile',
  'expectedIdentities',
  'commandOutcome',
  'runtimeCleanupOutcome',
  'processTreeOutcome',
  'identityLeaseOutcome',
  'evidenceOutcome',
])
const PRODUCER_MANIFEST_KEYS = Object.freeze([
  'schemaVersion',
  'checkoutSha',
  'nodeVersion',
  'broker',
  'runtimeBundle',
  'completionBundle',
  'scheduledManifest',
  'scheduledProfiles',
  'publisherHelper',
  'processOwner',
])

export async function publishNetworkCompletion(options) {
  const repositoryRoot = requireCanonicalDirectory(options.repositoryRoot, 'completion repository root')
  const outputRoot = requireCanonicalDirectory(options.outputRoot, 'network evidence root')
  if (outputRoot !== join(repositoryRoot, 'test-results', 'browser-network')) {
    throw new Error('network evidence root is outside the completion authority')
  }
  const runId = requireString(options.runId, RUN_ID_PATTERN, 'completion run ID')
  const checkoutSha = requireString(options.checkoutSha, CHECKOUT_SHA_PATTERN, 'completion checkout SHA')
  const runtimeConfigSha256 = requireString(
    options.runtimeConfigSha256,
    SHA256_PATTERN,
    'runtime-config digest',
  )
  if (options.childExitCode !== 0 || options.childSignal !== null) {
    throw new Error('network child did not settle as an exit-zero completion authority')
  }
  const producerManifestBytes = readStableRegularFile(
    options.producerManifestPath,
    'prepared producer manifest',
    MAXIMUM_MANIFEST_BYTES,
  )
  const producerManifestSha256 = sha256(producerManifestBytes)
  if (producerManifestSha256 !== options.producerManifestSha256) {
    throw new Error('prepared producer manifest digest changed before completion')
  }
  const producerManifest = parsePreparedInputManifestJson(decodeUtf8(
    producerManifestBytes,
    'prepared producer manifest',
  ))
  if (producerManifest.checkoutSha !== checkoutSha) {
    throw new Error('prepared producer manifest belongs to another checkout')
  }

  const scheduledManifestPath = join(repositoryRoot, ...SCHEDULED_MANIFEST_RELATIVE_PATH.split('/'))
  const registry = await loadNetworkMatrixRegistry(scheduledManifestPath)
  verifyProducerRegistryBinding(producerManifest, registry, repositoryRoot)

  requireExactEvidenceInventory(outputRoot)
  const runBytes = readStableRegularFile(join(outputRoot, 'run.json'), 'network run evidence', MAXIMUM_EVIDENCE_BYTES)
  const aggregateBytes = readStableRegularFile(
    join(outputRoot, 'aggregate.json'),
    'network aggregate evidence',
    MAXIMUM_EVIDENCE_BYTES,
  )
  const run = parseNetworkRunResultJson(decodeUtf8(runBytes, 'network run evidence'), registry)
  const aggregate = parseNetworkMatrixAggregateJson(
    decodeUtf8(aggregateBytes, 'network aggregate evidence'),
    registry,
    [run],
  )
  requireExactEvidenceInventory(outputRoot)
  requireAcceptedEvidence(registry, run, aggregate, runId)

  const runtimeHelperManifestBytes = readStableRegularFile(
    options.runtimeHelperManifestPath,
    'runtime helper manifest',
    MAXIMUM_MANIFEST_BYTES,
  )
  const runtimeHelperManifestSha256 = sha256(runtimeHelperManifestBytes)
  if (runtimeHelperManifestSha256 !== options.runtimeHelperManifestSha256) {
    throw new Error('runtime helper manifest digest changed before completion')
  }
  verifyRuntimeHelperManifest(runtimeHelperManifestBytes, producerManifest, true)

  // Exit zero is meaningful only after network-entry has accepted the existing
  // run/aggregate parsers, settled the runtime, emptied its process-owner ledger,
  // and force-closed the minted identity lease. This separate measured binding
  // lets the token-free consumer reject an envelope that contradicts that exit.
  const executionBinding = Object.freeze({
    schemaVersion: EXECUTION_BINDING_SCHEMA,
    runId,
    checkoutSha,
    producerManifestSha256,
    runtimeBundleSha256: producerManifest.runtimeBundle.sha256,
    runtimeHelperManifestSha256,
    runtimeConfigSha256,
    childExitCode: options.childExitCode,
    childSignal: options.childSignal,
    commandOutcome: 'completed',
    runtimeCleanupOutcome: 'completed',
    processTreeOutcome: 'empty',
    identityLeaseOutcome: 'closed',
  })
  const executionBindingBytes = Buffer.from(`${JSON.stringify(executionBinding)}\n`, 'utf8')

  const completion = Object.freeze({
    schemaVersion: NETWORK_COMPLETION_SCHEMA,
    runId,
    checkoutSha,
    manifestSha256: registry.manifestSha256,
    runSha256: sha256(runBytes),
    aggregateSha256: sha256(aggregateBytes),
    producerManifestSha256,
    runtimeBundleSha256: producerManifest.runtimeBundle.sha256,
    runtimeHelperManifestSha256,
    runtimeConfigSha256,
    executionBindingSha256: sha256(executionBindingBytes),
    artifactDirectory: 'browser-network',
    runFile: 'run.json',
    aggregateFile: 'aggregate.json',
    producerManifestFile: PRODUCER_MANIFEST_FILE_NAME,
    runtimeHelperManifestFile: RUNTIME_HELPER_MANIFEST_FILE_NAME,
    executionBindingFile: EXECUTION_BINDING_FILE_NAME,
    expectedIdentities: EXPECTED_IDENTITIES,
    commandOutcome: 'completed',
    runtimeCleanupOutcome: 'completed',
    processTreeOutcome: 'empty',
    identityLeaseOutcome: 'closed',
    evidenceOutcome: 'complete',
  })
  const resultsRoot = requireCanonicalDirectory(join(repositoryRoot, 'test-results'), 'test-results root')
  atomicPublish(join(resultsRoot, PRODUCER_MANIFEST_FILE_NAME), producerManifestBytes)
  atomicPublish(join(resultsRoot, RUNTIME_HELPER_MANIFEST_FILE_NAME), runtimeHelperManifestBytes)
  atomicPublish(join(resultsRoot, EXECUTION_BINDING_FILE_NAME), executionBindingBytes)
  atomicPublish(join(resultsRoot, COMPLETION_FILE_NAME), Buffer.from(`${JSON.stringify(completion)}\n`, 'utf8'))
  return completion
}

export async function consumeNetworkCompletion(options) {
  const repositoryRoot = requireCanonicalDirectory(options.repositoryRoot, 'completion repository root')
  const checkoutSha = requireString(options.checkoutSha, CHECKOUT_SHA_PATTERN, 'completion checkout SHA')
  const completionBytes = readStableRegularFile(
    options.completionPath,
    'browser network completion',
    MAXIMUM_COMPLETION_BYTES,
    { retainedLinuxDescriptor: true },
  )
  const completion = parseNetworkCompletionJson(decodeUtf8(
    completionBytes,
    'browser network completion',
  ))
  if (completion.checkoutSha !== checkoutSha) {
    throw new Error('browser network completion belongs to another checkout')
  }

  const resultsRoot = requireCanonicalDirectory(join(repositoryRoot, 'test-results'), 'test-results root')
  const producerManifestBytes = readStableRegularFile(
    join(resultsRoot, completion.producerManifestFile),
    'transferred producer manifest',
    MAXIMUM_MANIFEST_BYTES,
  )
  if (sha256(producerManifestBytes) !== completion.producerManifestSha256) {
    throw new Error('transferred producer manifest differs from completion')
  }
  const producerManifest = parsePreparedInputManifestJson(decodeUtf8(
    producerManifestBytes,
    'transferred producer manifest',
  ))
  if (
    producerManifest.checkoutSha !== checkoutSha ||
    producerManifest.runtimeBundle.sha256 !== completion.runtimeBundleSha256
  ) throw new Error('producer identity contradicts browser network completion')

  const registry = await loadNetworkMatrixRegistry(
    join(repositoryRoot, ...SCHEDULED_MANIFEST_RELATIVE_PATH.split('/')),
  )
  verifyProducerRegistryBinding(producerManifest, registry, repositoryRoot)
  if (registry.manifestSha256 !== completion.manifestSha256) {
    throw new Error('current scheduled manifest differs from browser network completion')
  }

  const artifactRoot = requireCanonicalDirectory(
    join(resultsRoot, completion.artifactDirectory),
    'transferred network evidence root',
  )
  requireExactEvidenceInventory(artifactRoot)
  const runBytes = readStableRegularFile(
    join(artifactRoot, completion.runFile),
    'transferred network run evidence',
    MAXIMUM_EVIDENCE_BYTES,
  )
  const aggregateBytes = readStableRegularFile(
    join(artifactRoot, completion.aggregateFile),
    'transferred network aggregate evidence',
    MAXIMUM_EVIDENCE_BYTES,
  )
  if (sha256(runBytes) !== completion.runSha256 || sha256(aggregateBytes) !== completion.aggregateSha256) {
    throw new Error('transferred network evidence differs from browser network completion')
  }
  const run = parseNetworkRunResultJson(decodeUtf8(runBytes, 'transferred network run evidence'), registry)
  const aggregate = parseNetworkMatrixAggregateJson(
    decodeUtf8(aggregateBytes, 'transferred network aggregate evidence'),
    registry,
    [run],
  )
  requireExactEvidenceInventory(artifactRoot)
  requireAcceptedEvidence(registry, run, aggregate, completion.runId)

  const runtimeHelperManifestBytes = readStableRegularFile(
    join(resultsRoot, completion.runtimeHelperManifestFile),
    'transferred runtime helper manifest',
    MAXIMUM_MANIFEST_BYTES,
  )
  if (sha256(runtimeHelperManifestBytes) !== completion.runtimeHelperManifestSha256) {
    throw new Error('transferred runtime helper manifest differs from browser network completion')
  }
  verifyRuntimeHelperManifest(runtimeHelperManifestBytes, producerManifest, false)
  const executionBindingBytes = readStableRegularFile(
    join(resultsRoot, completion.executionBindingFile),
    'transferred execution binding',
    MAXIMUM_COMPLETION_BYTES,
  )
  if (sha256(executionBindingBytes) !== completion.executionBindingSha256) {
    throw new Error('transferred execution binding differs from browser network completion')
  }
  const executionBinding = parseExecutionBindingJson(decodeUtf8(
    executionBindingBytes,
    'transferred execution binding',
  ))
  if (
    executionBinding.runId !== completion.runId || executionBinding.checkoutSha !== checkoutSha ||
    executionBinding.producerManifestSha256 !== completion.producerManifestSha256 ||
    executionBinding.runtimeBundleSha256 !== completion.runtimeBundleSha256 ||
    executionBinding.runtimeHelperManifestSha256 !== completion.runtimeHelperManifestSha256 ||
    executionBinding.runtimeConfigSha256 !== completion.runtimeConfigSha256 ||
    executionBinding.commandOutcome !== completion.commandOutcome ||
    executionBinding.runtimeCleanupOutcome !== completion.runtimeCleanupOutcome ||
    executionBinding.processTreeOutcome !== completion.processTreeOutcome ||
    executionBinding.identityLeaseOutcome !== completion.identityLeaseOutcome
  ) throw new Error('execution binding contradicts browser network completion')
  return Object.freeze({
    schemaVersion: NETWORK_COMPLETION_SCHEMA,
    runId: completion.runId,
    checkoutSha,
    expectedIdentities: EXPECTED_IDENTITIES,
    outcome: 'accepted',
  })
}

export function parseNetworkCompletionJson(encoded) {
  if (typeof encoded !== 'string') throw new Error('browser network completion JSON must be text')
  let value
  try {
    value = JSON.parse(encoded)
  } catch {
    throw new Error('browser network completion JSON is invalid')
  }
  const parsed = parseNetworkCompletion(value)
  if (encoded !== `${JSON.stringify(parsed)}\n`) {
    throw new Error('browser network completion JSON is not canonical')
  }
  return parsed
}

export function parseNetworkCompletion(value) {
  const record = exactDataRecord(value, COMPLETION_KEYS, 'browser network completion')
  return Object.freeze({
    schemaVersion: requireLiteral(record.schemaVersion, NETWORK_COMPLETION_SCHEMA, 'completion schema'),
    runId: requireString(record.runId, RUN_ID_PATTERN, 'completion run ID'),
    checkoutSha: requireString(record.checkoutSha, CHECKOUT_SHA_PATTERN, 'completion checkout SHA'),
    manifestSha256: requireString(record.manifestSha256, SHA256_PATTERN, 'completion manifest digest'),
    runSha256: requireString(record.runSha256, SHA256_PATTERN, 'completion run digest'),
    aggregateSha256: requireString(record.aggregateSha256, SHA256_PATTERN, 'completion aggregate digest'),
    producerManifestSha256: requireString(
      record.producerManifestSha256,
      SHA256_PATTERN,
      'completion producer-manifest digest',
    ),
    runtimeBundleSha256: requireString(
      record.runtimeBundleSha256,
      SHA256_PATTERN,
      'completion runtime-bundle digest',
    ),
    runtimeHelperManifestSha256: requireString(
      record.runtimeHelperManifestSha256,
      SHA256_PATTERN,
      'completion runtime-helper-manifest digest',
    ),
    runtimeConfigSha256: requireString(
      record.runtimeConfigSha256,
      SHA256_PATTERN,
      'completion runtime-config digest',
    ),
    executionBindingSha256: requireString(
      record.executionBindingSha256,
      SHA256_PATTERN,
      'completion execution-binding digest',
    ),
    artifactDirectory: requireLiteral(record.artifactDirectory, 'browser-network', 'completion artifact directory'),
    runFile: requireLiteral(record.runFile, 'run.json', 'completion run file'),
    aggregateFile: requireLiteral(record.aggregateFile, 'aggregate.json', 'completion aggregate file'),
    producerManifestFile: requireLiteral(
      record.producerManifestFile,
      PRODUCER_MANIFEST_FILE_NAME,
      'completion producer-manifest file',
    ),
    runtimeHelperManifestFile: requireLiteral(
      record.runtimeHelperManifestFile,
      RUNTIME_HELPER_MANIFEST_FILE_NAME,
      'completion runtime-helper-manifest file',
    ),
    executionBindingFile: requireLiteral(
      record.executionBindingFile,
      EXECUTION_BINDING_FILE_NAME,
      'completion execution-binding file',
    ),
    expectedIdentities: requireLiteral(
      record.expectedIdentities,
      EXPECTED_IDENTITIES,
      'completion identity count',
    ),
    commandOutcome: requireLiteral(record.commandOutcome, 'completed', 'completion command outcome'),
    runtimeCleanupOutcome: requireLiteral(
      record.runtimeCleanupOutcome,
      'completed',
      'completion runtime cleanup outcome',
    ),
    processTreeOutcome: requireLiteral(record.processTreeOutcome, 'empty', 'completion process-tree outcome'),
    identityLeaseOutcome: requireLiteral(record.identityLeaseOutcome, 'closed', 'completion identity-lease outcome'),
    evidenceOutcome: requireLiteral(record.evidenceOutcome, 'complete', 'completion evidence outcome'),
  })
}

export function parseExecutionBindingJson(encoded) {
  if (typeof encoded !== 'string') throw new Error('execution binding must be text')
  let value
  try {
    value = JSON.parse(encoded)
  } catch {
    throw new Error('execution binding JSON is invalid')
  }
  const keys = [
    'schemaVersion', 'runId', 'checkoutSha', 'producerManifestSha256',
    'runtimeBundleSha256', 'runtimeHelperManifestSha256', 'runtimeConfigSha256',
    'childExitCode', 'childSignal', 'commandOutcome', 'runtimeCleanupOutcome',
    'processTreeOutcome', 'identityLeaseOutcome',
  ]
  const record = exactDataRecord(value, keys, 'execution binding')
  const parsed = Object.freeze({
    schemaVersion: requireLiteral(record.schemaVersion, EXECUTION_BINDING_SCHEMA, 'execution-binding schema'),
    runId: requireString(record.runId, RUN_ID_PATTERN, 'execution-binding run ID'),
    checkoutSha: requireString(record.checkoutSha, CHECKOUT_SHA_PATTERN, 'execution-binding checkout SHA'),
    producerManifestSha256: requireString(
      record.producerManifestSha256,
      SHA256_PATTERN,
      'execution-binding producer digest',
    ),
    runtimeBundleSha256: requireString(
      record.runtimeBundleSha256,
      SHA256_PATTERN,
      'execution-binding runtime digest',
    ),
    runtimeHelperManifestSha256: requireString(
      record.runtimeHelperManifestSha256,
      SHA256_PATTERN,
      'execution-binding helper-manifest digest',
    ),
    runtimeConfigSha256: requireString(
      record.runtimeConfigSha256,
      SHA256_PATTERN,
      'execution-binding runtime-config digest',
    ),
    childExitCode: requireLiteral(record.childExitCode, 0, 'execution-binding child exit'),
    childSignal: requireLiteral(record.childSignal, null, 'execution-binding child signal'),
    commandOutcome: requireLiteral(record.commandOutcome, 'completed', 'execution-binding command outcome'),
    runtimeCleanupOutcome: requireLiteral(
      record.runtimeCleanupOutcome,
      'completed',
      'execution-binding runtime cleanup',
    ),
    processTreeOutcome: requireLiteral(record.processTreeOutcome, 'empty', 'execution-binding process tree'),
    identityLeaseOutcome: requireLiteral(record.identityLeaseOutcome, 'closed', 'execution-binding identity lease'),
  })
  if (encoded !== `${JSON.stringify(parsed)}\n`) {
    throw new Error('execution binding JSON is not canonical')
  }
  return parsed
}

export function parsePreparedInputManifestJson(encoded) {
  if (typeof encoded !== 'string') throw new Error('prepared producer manifest must be text')
  let value
  try {
    value = JSON.parse(encoded)
  } catch {
    throw new Error('prepared producer manifest JSON is invalid')
  }
  const record = exactDataRecord(value, PRODUCER_MANIFEST_KEYS, 'prepared producer manifest')
  const profileValues = denseDataArray(record.scheduledProfiles, 'prepared scheduled profiles')
  if (profileValues.length !== EXPECTED_PROFILE_FILE_NAMES.length) {
    throw new Error('prepared scheduled profile registry has the wrong size')
  }
  const parsed = Object.freeze({
    schemaVersion: requireLiteral(record.schemaVersion, PREPARED_INPUT_SCHEMA, 'prepared manifest schema'),
    checkoutSha: requireString(record.checkoutSha, CHECKOUT_SHA_PATTERN, 'prepared checkout SHA'),
    nodeVersion: requireLiteral(record.nodeVersion, EXPECTED_NODE_VERSION, 'prepared Node version'),
    broker: parseFileIdentity(record.broker, 'oidc-network-broker.mjs', 'prepared broker'),
    runtimeBundle: parseFileIdentity(
      record.runtimeBundle,
      'network-entry-bundle.mjs',
      'prepared runtime bundle',
    ),
    completionBundle: parseFileIdentity(
      record.completionBundle,
      'network-completion-bundle.mjs',
      'prepared completion bundle',
    ),
    scheduledManifest: parseFileIdentity(
      record.scheduledManifest,
      'scheduled-hard.manifest.v2.json',
      'prepared scheduled manifest',
    ),
    scheduledProfiles: Object.freeze(profileValues.map((entry, index) => parseFileIdentity(
      entry,
      EXPECTED_PROFILE_FILE_NAMES[index],
      `prepared scheduled profile ${index}`,
    ))),
    publisherHelper: parseFileIdentity(
      record.publisherHelper,
      'browsermatrixpublish',
      'prepared publisher helper',
    ),
    processOwner: parseFileIdentity(
      record.processOwner,
      'testprocessowner',
      'prepared process owner',
    ),
  })
  if (encoded !== `${JSON.stringify(parsed)}\n`) {
    throw new Error('prepared producer manifest JSON is not canonical')
  }
  return parsed
}

function verifyProducerRegistryBinding(producer, registry, repositoryRoot) {
  if (
    producer.scheduledManifest.sha256 !== registry.manifestSha256 ||
    registry.manifest.identityCounts.total !== EXPECTED_IDENTITIES ||
    registry.manifest.identityCounts.scheduled !== EXPECTED_IDENTITIES
  ) throw new Error('prepared scheduled manifest differs from the current matrix registry')
  const manifestBytes = readStableRegularFile(
    join(repositoryRoot, ...SCHEDULED_MANIFEST_RELATIVE_PATH.split('/')),
    'current scheduled manifest',
    MAXIMUM_MANIFEST_BYTES,
  )
  requireIdentityBytes(producer.scheduledManifest, manifestBytes, 'current scheduled manifest')
  for (const identity of producer.scheduledProfiles) {
    const bytes = readStableRegularFile(
      join(repositoryRoot, 'testdata', 'browser-network-matrix', ...identity.fileName.split('/')),
      `current ${identity.fileName}`,
      MAXIMUM_MANIFEST_BYTES,
    )
    requireIdentityBytes(identity, bytes, `current ${identity.fileName}`)
  }
}

function verifyRuntimeHelperManifest(bytes, producer, verifyHelperBytes) {
  const encoded = decodeUtf8(bytes, 'runtime helper manifest')
  let value
  try {
    value = JSON.parse(encoded)
  } catch {
    throw new Error('runtime helper manifest JSON is invalid')
  }
  const record = exactDataRecord(
    value,
    ['schemaVersion', 'platform', 'architecture', 'helpers'],
    'runtime helper manifest',
  )
  const helpers = denseDataArray(record.helpers, 'runtime helper registry')
  const expected = [
    ['artifact-publisher', producer.publisherHelper],
    ['test-process-owner', producer.processOwner],
  ]
  if (
    record.schemaVersion !== 'windshare.browser-network-matrix.helper-build/v2' ||
    record.platform !== 'linux' || record.architecture !== 'amd64' || helpers.length !== expected.length
  ) throw new Error('runtime helper manifest is invalid')
  for (let index = 0; index < expected.length; index += 1) {
    const entry = exactDataRecord(helpers[index], ['role', 'path'], `runtime helper ${index}`)
    const [role, identity] = expected[index]
    if (
      entry.role !== role || typeof entry.path !== 'string' || !isAbsolute(entry.path) ||
      basename(entry.path) !== identity.fileName
    ) throw new Error(`runtime helper binding is invalid: ${role}`)
    if (verifyHelperBytes) {
      const helperBytes = readStableRegularFile(entry.path, `runtime helper ${role}`, MAXIMUM_EVIDENCE_BYTES)
      requireIdentityBytes(identity, helperBytes, `runtime helper ${role}`)
    }
  }
  if (encoded !== `${JSON.stringify(value)}\n`) {
    throw new Error('runtime helper manifest JSON is not canonical')
  }
}

function requireAcceptedEvidence(registry, run, aggregate, runId) {
  if (
    registry.manifest.identityCounts.total !== EXPECTED_IDENTITIES ||
    registry.manifest.identityCounts.scheduled !== EXPECTED_IDENTITIES ||
    run.runId !== runId || run.manifestSha256 !== registry.manifestSha256 ||
    run.executionMode !== 'scheduled' || run.orchestrationOutcome !== 'healthy' ||
    run.orchestrationFailure !== null || run.runOutcome !== 'completed' ||
    run.expectedIdentities.length !== EXPECTED_IDENTITIES ||
    run.samples.length !== EXPECTED_IDENTITIES ||
    run.runtimeAttestations.some((attestation) => attestation.prerequisiteOutcome !== 'satisfied') ||
    run.samples.some((sample) =>
      sample.sampleOutcome !== 'observed' || sample.candidatePolicyOutcome !== 'matched' ||
      sample.failure !== null) ||
    run.profileResults.some((profile) =>
      profile.prerequisiteOutcome !== 'satisfied' || profile.expectedSamples !== 15 ||
      profile.observedSamples !== 15 || profile.sampleInfrastructureFailures !== 0 ||
      profile.profileOutcome !== 'completed') ||
    aggregate.manifestSha256 !== registry.manifestSha256 || aggregate.runs.length !== 1 ||
    aggregate.runs[0]?.runId !== runId || aggregate.runs[0]?.runOutcome !== 'completed' ||
    aggregate.counts.expectedIdentities !== EXPECTED_IDENTITIES ||
    aggregate.counts.observedSamples !== EXPECTED_IDENTITIES ||
    aggregate.counts.matched !== EXPECTED_IDENTITIES || aggregate.counts.mismatched !== 0 ||
    aggregate.counts.notEvaluated !== 0 || aggregate.counts.sampleInfrastructureFailures !== 0 ||
    aggregate.evidenceOutcome !== 'complete'
  ) throw new Error('browser network evidence does not satisfy the hard scheduled oracle')
}

function requireExactEvidenceInventory(directory) {
  const authority = opendirSync(directory)
  const entries = []
  try {
    while (true) {
      const entry = authority.readSync()
      if (entry === null) break
      if (entries.length === 2) {
        throw new Error('browser network evidence inventory contains an unexpected third entry')
      }
      entries.push(entry)
    }
  } finally {
    authority.closeSync()
  }
  entries.sort((left, right) => left.name.localeCompare(right.name, 'en'))
  if (
    entries.length !== 2 || entries[0]?.name !== 'aggregate.json' ||
    entries[1]?.name !== 'run.json' || entries.some((entry) => !entry.isFile() || entry.isSymbolicLink())
  ) throw new Error('browser network evidence inventory is not exactly run.json and aggregate.json')
}

function parseFileIdentity(value, expectedFileName, label) {
  const record = exactDataRecord(value, ['fileName', 'byteLength', 'sha256'], label)
  if (
    record.fileName !== expectedFileName || !Number.isSafeInteger(record.byteLength) ||
    record.byteLength < 1 || record.byteLength > MAXIMUM_EVIDENCE_BYTES
  ) throw new Error(`${label} identity is invalid`)
  return Object.freeze({
    fileName: expectedFileName,
    byteLength: record.byteLength,
    sha256: requireString(record.sha256, SHA256_PATTERN, `${label} digest`),
  })
}

function requireIdentityBytes(identity, bytes, label) {
  if (bytes.byteLength !== identity.byteLength || sha256(bytes) !== identity.sha256) {
    throw new Error(`${label} differs from its prepared identity`)
  }
}

function readStableRegularFile(pathValue, label, maximumBytes, { retainedLinuxDescriptor = false } = {}) {
  if (typeof pathValue !== 'string' || pathValue.length === 0 || pathValue.includes('\0')) {
    throw new Error(`${label} path is invalid`)
  }
  const retained = retainedLinuxDescriptor && RETAINED_LINUX_FILE_PATTERN.test(pathValue)
  const canonical = retained ? pathValue : resolve(pathValue)
  if (!retained && (!isAbsolute(pathValue) || canonical !== pathValue)) {
    throw new Error(`${label} path is not absolute and canonical`)
  }
  let namedBefore
  if (!retained) {
    namedBefore = lstatSync(canonical, { bigint: true })
    if (!namedBefore.isFile() || namedBefore.isSymbolicLink() || realpathSync(canonical) !== canonical) {
      throw new Error(`${label} is not a canonical real file`)
    }
  }
  const descriptor = openSync(
    canonical,
    fsConstants.O_RDONLY | (retained ? 0 : (fsConstants.O_NOFOLLOW ?? 0)),
  )
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    if (
      !openedBefore.isFile() || openedBefore.size < 1n || openedBefore.size > BigInt(maximumBytes) ||
      namedBefore !== undefined && !sameFileRevision(namedBefore, openedBefore)
    ) throw new Error(`${label} does not retain one bounded file revision`)
    const bytes = readFileSync(descriptor)
    const openedAfter = fstatSync(descriptor, { bigint: true })
    if (
      bytes.byteLength !== Number(openedAfter.size) ||
      !sameFileRevision(openedBefore, openedAfter) ||
      (!retained && !sameFileRevision(openedAfter, lstatSync(canonical, { bigint: true })))
    ) throw new Error(`${label} changed while it was read`)
    return bytes
  } finally {
    closeSync(descriptor)
  }
}

function atomicPublish(target, bytes) {
  const parent = requireCanonicalDirectory(dirname(target), 'completion publication parent')
  const temporary = join(parent, `.windshare-completion-${randomUUID()}.tmp`)
  let descriptor
  let temporaryExists = false
  try {
    descriptor = openSync(temporary, fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL, 0o600)
    temporaryExists = true
    let offset = 0
    while (offset < bytes.byteLength) offset += writeSync(descriptor, bytes, offset)
    fsyncSync(descriptor)
    closeSync(descriptor)
    descriptor = undefined
    linkSync(temporary, target)
    unlinkSync(temporary)
    temporaryExists = false
    if (process.platform === 'linux') {
      const parentDescriptor = openSync(parent, fsConstants.O_RDONLY | (fsConstants.O_DIRECTORY ?? 0))
      try { fsyncSync(parentDescriptor) } finally { closeSync(parentDescriptor) }
    }
  } finally {
    if (descriptor !== undefined) closeSync(descriptor)
    if (temporaryExists) {
      try { unlinkSync(temporary) } catch {}
    }
  }
}

function requireCanonicalDirectory(pathValue, label) {
  const canonical = resolve(pathValue)
  const metadata = lstatSync(canonical)
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || realpathSync(canonical) !== canonical) {
    throw new Error(`${label} is not a canonical real directory`)
  }
  return canonical
}

function exactDataRecord(value, expectedKeys, label) {
  if (
    typeof value !== 'object' || value === null || Array.isArray(value) || nodeTypes.isProxy(value) ||
    (Object.getPrototypeOf(value) !== Object.prototype && Object.getPrototypeOf(value) !== null)
  ) throw new Error(`${label} must be one inert object`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const keys = Reflect.ownKeys(descriptors)
  if (
    keys.length !== expectedKeys.length || keys.some((key) => typeof key !== 'string') ||
    keys.some((key, index) => key !== expectedKeys[index])
  ) throw new Error(`${label} fields are invalid`)
  const result = Object.create(null)
  for (const key of expectedKeys) {
    const descriptor = descriptors[key]
    if (
      descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true
    ) throw new Error(`${label} contains an active field`)
    result[key] = descriptor.value
  }
  return result
}

function denseDataArray(value, label) {
  if (!Array.isArray(value) || nodeTypes.isProxy(value)) throw new Error(`${label} must be one inert array`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const keys = Reflect.ownKeys(descriptors)
  const length = descriptors.length?.value
  if (
    !Number.isSafeInteger(length) || length < 0 || keys.length !== length + 1 ||
    keys.some((key) => typeof key !== 'string')
  ) throw new Error(`${label} must be dense`)
  const result = []
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    if (
      descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true
    ) throw new Error(`${label} contains an active entry`)
    result.push(descriptor.value)
  }
  return result
}

function requireString(value, pattern, label) {
  if (typeof value !== 'string' || !pattern.test(value)) throw new Error(`${label} is invalid`)
  return value
}

function requireLiteral(value, expected, label) {
  if (value !== expected) throw new Error(`${label} is invalid`)
  return expected
}

function decodeUtf8(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw new Error(`${label} is not UTF-8`)
  }
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

function sameFileRevision(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs && left.mode === right.mode
}

function takeEnvironment(environment, expectedName) {
  let selected
  for (const name of Object.keys(environment)) {
    if (name.toUpperCase() !== expectedName.toUpperCase()) continue
    if (selected !== undefined) throw new Error(`${expectedName} is duplicated`)
    const descriptor = Object.getOwnPropertyDescriptor(environment, name)
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || typeof descriptor.value !== 'string') {
      throw new Error(`${expectedName} is active`)
    }
    selected = { name, value: descriptor.value }
  }
  if (selected === undefined) throw new Error(`${expectedName} is unavailable`)
  delete environment[selected.name]
  return selected.value
}

function rejectAmbientOidc(environment) {
  for (const name of Object.keys(environment)) {
    const folded = name.toUpperCase()
    if (folded !== 'ACTIONS_ID_TOKEN_REQUEST_URL' && folded !== 'ACTIONS_ID_TOKEN_REQUEST_TOKEN') continue
    const descriptor = Object.getOwnPropertyDescriptor(environment, name)
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.value !== '') {
      throw new Error('OIDC request authority reached the token-free completion consumer')
    }
    delete environment[name]
  }
}

const invokedPath = process.argv[1] === undefined ? undefined : pathToFileURL(resolve(process.argv[1])).href
if (invokedPath === import.meta.url) {
  try {
    if (process.argv.length !== 3 || process.argv[2] !== 'consume') {
      throw new Error('browser network completion accepts only the consume command')
    }
    rejectAmbientOidc(process.env)
    const completionPath = takeEnvironment(process.env, 'BROWSER_NETWORK_COMPLETION')
    const checkoutSha = takeEnvironment(process.env, 'WINDSHARE_CORE_ARTIFACT_COMMIT_SHA')
    const repositoryRoot = resolve(import.meta.dirname, '..', '..', '..')
    const result = await consumeNetworkCompletion({ completionPath, checkoutSha, repositoryRoot })
    process.stdout.write(`${JSON.stringify(result)}\n`)
  } catch {
    process.stderr.write(`${JSON.stringify({
      schemaVersion: NETWORK_COMPLETION_SCHEMA,
      outcome: 'rejected',
      failureCode: 'browser-network-completion-rejected',
    })}\n`)
    process.exitCode = 1
  }
}
