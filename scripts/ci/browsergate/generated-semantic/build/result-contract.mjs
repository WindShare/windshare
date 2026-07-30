import {
  GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
  GENERATED_SEMANTIC_DIGEST,
  GENERATED_SEMANTIC_EXPORTS,
} from './artifact-policy.mjs'
import { GENERATED_SEMANTIC_FILENAME } from './config.mjs'
import { isGeneratedSemanticFailure } from './failure.mjs'
import { encodeCanonicalJsonLine, parseCanonicalJsonLine } from './json-record.mjs'
import { assertGeneratedSemanticToolVersions } from './tool-authorization.mjs'

export const GENERATED_SEMANTIC_RESULT_SCHEMA =
  'windshare.generated-semantic-result/v1'
export const GENERATED_SEMANTIC_RESULT_COMPONENT =
  'browser-generated-semantic'

const RESULT_KEYS = Object.freeze([
  'schemaVersion',
  'component',
  'mode',
  'outcome',
  'tools',
  'artifact',
  'failures',
])
const TOOL_KEYS = Object.freeze(['node', 'vite', 'rolldown'])
const ARTIFACT_KEYS = Object.freeze([
  'fileName',
  'byteLength',
  'sha256',
  'semanticDigest',
  'exports',
  'externalImports',
])
const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/u
const SHA256_PATTERN = /^[0-9a-f]{64}$/u

export function createGeneratedSemanticResult({
  mode,
  outcome,
  tools,
  artifact,
  failures,
}) {
  return freezeResult({
    schemaVersion: GENERATED_SEMANTIC_RESULT_SCHEMA,
    component: GENERATED_SEMANTIC_RESULT_COMPONENT,
    mode,
    outcome,
    tools,
    artifact,
    failures,
  })
}

export function encodeGeneratedSemanticResult(result) {
  return encodeCanonicalJsonLine(freezeResult(result))
}

export function parseGeneratedSemanticResultRecord(encoded) {
  return freezeResult(parseCanonicalJsonLine(encoded, 'generated semantic result record'))
}

function freezeResult(value) {
  exactKeys(value, RESULT_KEYS, 'generated semantic result')
  if (value.schemaVersion !== GENERATED_SEMANTIC_RESULT_SCHEMA) {
    throw new Error('generated semantic result schema is unsupported')
  }
  if (value.component !== GENERATED_SEMANTIC_RESULT_COMPONENT) {
    throw new Error('generated semantic result component is unsupported')
  }
  if (value.mode !== null && !['verify', 'write'].includes(value.mode)) {
    throw new Error('generated semantic result mode is invalid')
  }
  if (!['current', 'published', 'failed'].includes(value.outcome)) {
    throw new Error('generated semantic result outcome is invalid')
  }
  const tools = value.tools === null ? null : freezeTools(value.tools)
  const artifact = value.artifact === null ? null : freezeArtifact(value.artifact)
  if (!Array.isArray(value.failures) || value.failures.some((failure) =>
    !isGeneratedSemanticFailure(failure))) {
    throw new Error('generated semantic result failures are invalid')
  }
  const failures = Object.freeze(value.failures.map((failure) => Object.freeze({ ...failure })))
  if (value.outcome === 'failed') {
    if (failures.length === 0) throw new Error('failed generated semantic result lacks a failure')
  } else if (
    value.mode === null || tools === null || artifact === null || failures.length !== 0
  ) {
    throw new Error('successful generated semantic result is incomplete')
  }
  if (value.outcome === 'published' && value.mode !== 'write') {
    throw new Error('published generated semantic result requires write mode')
  }
  return Object.freeze({
    schemaVersion: GENERATED_SEMANTIC_RESULT_SCHEMA,
    component: GENERATED_SEMANTIC_RESULT_COMPONENT,
    mode: value.mode,
    outcome: value.outcome,
    tools,
    artifact,
    failures,
  })
}

function freezeTools(value) {
  exactKeys(value, TOOL_KEYS, 'generated semantic result tools')
  for (const name of TOOL_KEYS) {
    if (typeof value[name] !== 'string' || !VERSION_PATTERN.test(value[name])) {
      throw new Error(`generated semantic ${name} result version is invalid`)
    }
  }
  assertGeneratedSemanticToolVersions({ vite: value.vite, rolldown: value.rolldown })
  return Object.freeze({ node: value.node, vite: value.vite, rolldown: value.rolldown })
}

function freezeArtifact(value) {
  exactKeys(value, ARTIFACT_KEYS, 'generated semantic result artifact')
  if (value.fileName !== GENERATED_SEMANTIC_FILENAME) {
    throw new Error('generated semantic result artifact name is invalid')
  }
  if (!Number.isSafeInteger(value.byteLength) || value.byteLength < 1) {
    throw new Error('generated semantic result artifact length is invalid')
  }
  if (!SHA256_PATTERN.test(value.sha256) || value.semanticDigest !== GENERATED_SEMANTIC_DIGEST) {
    throw new Error('generated semantic result artifact digest is invalid')
  }
  return Object.freeze({
    fileName: value.fileName,
    byteLength: value.byteLength,
    sha256: value.sha256,
    semanticDigest: value.semanticDigest,
    exports: canonicalStringSet(
      value.exports,
      GENERATED_SEMANTIC_EXPORTS,
      false,
      'generated semantic result exports',
    ),
    externalImports: canonicalStringSet(
      value.externalImports,
      GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
      true,
      'generated semantic result external imports',
    ),
  })
}

function canonicalStringSet(value, policy, subset, label) {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== 'string')) {
    throw new Error(`${label} must be an array of strings`)
  }
  if (
    new Set(value).size !== value.length ||
    value.some((entry, index) => index > 0 && value[index - 1] >= entry) ||
    (subset
      ? value.some((entry) => !policy.includes(entry))
      : value.length !== policy.length || value.some((entry, index) => entry !== policy[index]))
  ) throw new Error(`${label} are not a canonical policy-constrained set`)
  return Object.freeze([...value])
}

function exactKeys(value, keys, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  const actual = Object.keys(value)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}
