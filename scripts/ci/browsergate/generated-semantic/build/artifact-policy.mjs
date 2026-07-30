import { createHash } from 'node:crypto'

import { GENERATED_SEMANTIC_FILENAME } from './config.mjs'
import { throwGeneratedSemanticFailure } from './failure.mjs'

export const GENERATED_SEMANTIC_DIGEST =
  '25e349f1212bb99491944eb8e885665bb71edc5d5db49d1cd2ef1ffafac1dd5d'
export const GENERATED_SEMANTIC_EXPORTS = Object.freeze([
  'evaluateFinalBrowserSample',
  'parseFinalGuardUploadManifest',
])
export const GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS = Object.freeze([
  'node:crypto',
  'node:fs/promises',
  'node:path',
])
export const GENERATED_SEMANTIC_ROOT_ALLOWLIST = Object.freeze([
  'build',
  GENERATED_SEMANTIC_FILENAME,
  'runtime-preflight.mjs',
  'verify-generated.mjs',
])
export const GENERATED_SEMANTIC_BUILD_ALLOWLIST = Object.freeze([
  'arguments.mjs',
  'artifact-policy.mjs',
  'cli.mjs',
  'config.mjs',
  'environment.mjs',
  'failure.mjs',
  'json-record.mjs',
  'publisher.mjs',
  'result-contract.mjs',
  'tool-authorization.mjs',
  'worker-launcher.mjs',
  'worker-protocol.mjs',
  'worker.mjs',
].sort(compareOrdinal))

const validatedArtifacts = new WeakSet()
const SHA256_PATTERN = /^[0-9a-f]{64}$/u

export function validateGeneratedSemanticArtifact({
  builds,
  generatedRootEntries,
  buildDirectoryEntries,
}) {
  validateGeneratedSemanticDirectorySurface({
    generatedRootEntries,
    buildDirectoryEntries,
  })
  if (!Array.isArray(builds) || builds.length !== 1) {
    policyFailure('build-result-count', 'generated semantic build must return exactly one result')
  }
  const outputs = builds[0]?.outputs
  if (!Array.isArray(outputs) || outputs.length !== 1) {
    policyFailure('output-count', 'generated semantic build must return exactly one output')
  }
  const chunk = outputs[0]
  if (chunk?.type !== 'chunk') {
    policyFailure('output-type', 'generated semantic output must be one JavaScript chunk')
  }
  if (chunk.fileName !== GENERATED_SEMANTIC_FILENAME) {
    policyFailure('output-name', 'generated semantic chunk has an unexpected file name')
  }
  if (chunk.isEntry !== true || chunk.isDynamicEntry !== false) {
    policyFailure('entry-shape', 'generated semantic chunk must be one static entry')
  }
  if (chunk.hasSourceMap !== false) {
    policyFailure('source-map', 'generated semantic chunk must not contain a source map')
  }
  const externalImports = requireApprovedStringSet(
    chunk.imports,
    GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
    'external-imports',
    'generated semantic external imports exceed the approved set',
  )
  requireExactStringSet(
    chunk.dynamicImports,
    [],
    'dynamic-imports',
    'generated semantic chunk must not contain dynamic imports',
  )
  requireExactStringSet(
    chunk.exports,
    GENERATED_SEMANTIC_EXPORTS,
    'exports',
    'generated semantic exports differ from the expected surface',
  )
  if (typeof chunk.code !== 'string' || chunk.code.length === 0) {
    policyFailure('empty-code', 'generated semantic chunk code is empty')
  }
  requireCompleteSemanticDigest(chunk.code)

  const bytes = Buffer.from(chunk.code, 'utf8')
  const artifact = Object.freeze({
    fileName: GENERATED_SEMANTIC_FILENAME,
    code: chunk.code,
    byteLength: bytes.byteLength,
    sha256: createHash('sha256').update(bytes).digest('hex'),
    semanticDigest: GENERATED_SEMANTIC_DIGEST,
    exports: GENERATED_SEMANTIC_EXPORTS,
    externalImports,
  })
  validatedArtifacts.add(artifact)
  return artifact
}

export function validateGeneratedSemanticDirectorySurface({
  generatedRootEntries,
  buildDirectoryEntries,
}) {
  requireExactInventory(
    generatedRootEntries,
    GENERATED_SEMANTIC_ROOT_ALLOWLIST,
    'generated semantic root',
    ['build'],
  )
  requireExactInventory(
    buildDirectoryEntries,
    GENERATED_SEMANTIC_BUILD_ALLOWLIST,
    'generated semantic build directory',
  )
}

export function requireValidatedGeneratedSemanticArtifact(value) {
  if (!validatedArtifacts.has(value)) {
    throw new TypeError('generated semantic publication requires a validated artifact')
  }
  return value
}

export function validatedGeneratedSemanticBytes(value) {
  const artifact = requireValidatedGeneratedSemanticArtifact(value)
  return Buffer.from(artifact.code, 'utf8')
}

export function generatedSemanticArtifactSummary(value) {
  const artifact = requireValidatedGeneratedSemanticArtifact(value)
  return Object.freeze({
    fileName: artifact.fileName,
    byteLength: artifact.byteLength,
    sha256: artifact.sha256,
    semanticDigest: artifact.semanticDigest,
    exports: artifact.exports,
    externalImports: artifact.externalImports,
  })
}

function requireCompleteSemanticDigest(code) {
  if (!SHA256_PATTERN.test(GENERATED_SEMANTIC_DIGEST)) {
    throw new Error('configured generated semantic digest is invalid')
  }
  const prefix = GENERATED_SEMANTIC_DIGEST.slice(0, -1)
  const observed = code.match(new RegExp(`${prefix}[0-9a-f]`, 'gu')) ?? []
  if (observed.length === 0 || observed.some((digest) => digest !== GENERATED_SEMANTIC_DIGEST)) {
    policyFailure('semantic-digest', 'generated semantic chunk has an incorrect full digest')
  }
}

function requireExactStringSet(actual, expected, code, message) {
  if (!Array.isArray(actual) || actual.some((entry) => typeof entry !== 'string')) {
    policyFailure(code, message)
  }
  const canonical = [...actual].sort(compareOrdinal)
  if (
    new Set(canonical).size !== canonical.length ||
    canonical.length !== expected.length ||
    canonical.some((entry, index) => entry !== expected[index])
  ) policyFailure(code, message)
}

function requireApprovedStringSet(actual, approved, code, message) {
  if (!Array.isArray(actual) || actual.some((entry) => typeof entry !== 'string')) {
    policyFailure(code, message)
  }
  if (new Set(actual).size !== actual.length || actual.some((entry) => !approved.includes(entry))) {
    policyFailure(code, message)
  }
  return Object.freeze([...actual].sort(compareOrdinal))
}

function requireExactInventory(actual, expected, label, directoryNames = []) {
  if (!Array.isArray(actual)) policyFailure('directory-surface', `${label} inventory is invalid`)
  const names = actual.map((entry) => requireDirectoryEntry(entry, label, directoryNames))
  const canonical = names.sort(compareOrdinal)
  if (
    new Set(canonical).size !== canonical.length ||
    canonical.length !== expected.length ||
    canonical.some((entry, index) => entry !== expected[index])
  ) policyFailure('directory-surface', `${label} differs from its exact allowlist`)
}

function requireDirectoryEntry(entry, label, directoryNames) {
  if (
    entry === null || typeof entry !== 'object' ||
    typeof entry.name !== 'string' || entry.name === '' ||
    typeof entry.isFile !== 'function' ||
    typeof entry.isDirectory !== 'function' ||
    typeof entry.isSymbolicLink !== 'function'
  ) policyFailure('directory-entry-type', `${label} entry metadata is invalid`)

  let isFile
  let isDirectory
  let isSymbolicLink
  try {
    isFile = entry.isFile()
    isDirectory = entry.isDirectory()
    isSymbolicLink = entry.isSymbolicLink()
  } catch {
    policyFailure('directory-entry-type', `${label} entry metadata is unreadable`)
  }
  if (
    typeof isFile !== 'boolean' || typeof isDirectory !== 'boolean' ||
    typeof isSymbolicLink !== 'boolean' || isSymbolicLink
  ) policyFailure('directory-entry-type', `${label} entries must not be symbolic links`)

  const expectedDirectory = directoryNames.includes(entry.name)
  if (isDirectory !== expectedDirectory || isFile === expectedDirectory) {
    policyFailure('directory-entry-type', `${label} entry has an unexpected filesystem type`)
  }
  return entry.name
}

function policyFailure(code, message) {
  throwGeneratedSemanticFailure('artifact-policy', code, message)
}

function compareOrdinal(left, right) {
  return left < right ? -1 : left > right ? 1 : 0
}
