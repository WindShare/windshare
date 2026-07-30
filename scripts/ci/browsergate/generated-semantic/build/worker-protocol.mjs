import { isAbsolute, resolve } from 'node:path'

import { isGeneratedSemanticFailure } from './failure.mjs'
import {
  encodeCanonicalJsonLine,
  encodeCanonicalJsonValue,
  parseCanonicalJsonLine,
  parseCanonicalJsonValue,
} from './json-record.mjs'

export const GENERATED_SEMANTIC_WORKER_SCHEMA =
  'windshare.generated-semantic-worker/v1'

const REQUEST_KEYS = Object.freeze([
  'schemaVersion',
  'webRoot',
  'semanticEntry',
  'isolatedCacheRoot',
  'viteModulePath',
])
const RESULT_KEYS = Object.freeze([
  'schemaVersion',
  'outcome',
  'tools',
  'builds',
  'failure',
])
const TOOL_KEYS = Object.freeze(['vite', 'rolldown'])
const CHUNK_KEYS = Object.freeze([
  'type',
  'fileName',
  'isEntry',
  'isDynamicEntry',
  'exports',
  'imports',
  'dynamicImports',
  'code',
  'hasSourceMap',
])
const ASSET_KEYS = Object.freeze(['type', 'fileName'])
const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/u

export function createGeneratedSemanticWorkerRequest({
  webRoot,
  semanticEntry,
  isolatedCacheRoot,
  viteModulePath,
}) {
  return freezeWorkerRequest({
    schemaVersion: GENERATED_SEMANTIC_WORKER_SCHEMA,
    webRoot,
    semanticEntry,
    isolatedCacheRoot,
    viteModulePath,
  })
}

export function encodeGeneratedSemanticWorkerRequest(request) {
  return encodeCanonicalJsonValue(freezeWorkerRequest(request))
}

export function parseGeneratedSemanticWorkerRequest(encoded) {
  return freezeWorkerRequest(parseCanonicalJsonValue(encoded, 'generated semantic worker request'))
}

export function createGeneratedSemanticWorkerBuiltResult({ tools, builds }) {
  return freezeWorkerResult({
    schemaVersion: GENERATED_SEMANTIC_WORKER_SCHEMA,
    outcome: 'built',
    tools,
    builds,
    failure: null,
  })
}

export function createGeneratedSemanticWorkerFailedResult(failure, tools = null) {
  return freezeWorkerResult({
    schemaVersion: GENERATED_SEMANTIC_WORKER_SCHEMA,
    outcome: 'failed',
    tools,
    builds: [],
    failure,
  })
}

export function encodeGeneratedSemanticWorkerResult(result) {
  return encodeCanonicalJsonLine(freezeWorkerResult(result))
}

export function parseGeneratedSemanticWorkerResult(encoded) {
  return freezeWorkerResult(parseCanonicalJsonLine(
    encoded,
    'generated semantic worker result',
  ))
}

export function distillGeneratedSemanticBuildResult(rawResult) {
  const rawBuilds = Array.isArray(rawResult) ? rawResult : [rawResult]
  if (rawBuilds.length === 0) throw new Error('Vite returned no generated semantic build result')
  return Object.freeze(rawBuilds.map((rawBuild) => {
    if (!isRecord(rawBuild) || !Array.isArray(rawBuild.output)) {
      throw new Error('Vite returned an unsupported generated semantic build result')
    }
    return Object.freeze({
      outputs: Object.freeze(rawBuild.output.map(distillOutput)),
    })
  }))
}

function distillOutput(output) {
  if (!isRecord(output) || !['asset', 'chunk'].includes(output.type)) {
    throw new Error('Vite returned an unknown generated semantic output')
  }
  if (output.type === 'asset') {
    return Object.freeze({ type: 'asset', fileName: requireString(output.fileName, 'asset file name') })
  }
  return Object.freeze({
    type: 'chunk',
    fileName: requireString(output.fileName, 'chunk file name'),
    isEntry: requireBoolean(output.isEntry, 'chunk entry flag'),
    isDynamicEntry: requireBoolean(output.isDynamicEntry, 'chunk dynamic-entry flag'),
    exports: stringArray(output.exports, 'chunk exports'),
    imports: stringArray(output.imports, 'chunk imports'),
    dynamicImports: stringArray(output.dynamicImports, 'chunk dynamic imports'),
    code: requireString(output.code, 'chunk code'),
    hasSourceMap: output.map !== null && output.map !== undefined,
  })
}

function freezeWorkerRequest(value) {
  exactKeys(value, REQUEST_KEYS, 'generated semantic worker request')
  if (value.schemaVersion !== GENERATED_SEMANTIC_WORKER_SCHEMA) {
    throw new Error('generated semantic worker request schema is unsupported')
  }
  return Object.freeze({
    schemaVersion: GENERATED_SEMANTIC_WORKER_SCHEMA,
    webRoot: requireAbsolutePath(value.webRoot, 'worker web root'),
    semanticEntry: requireAbsolutePath(value.semanticEntry, 'worker semantic entry'),
    isolatedCacheRoot: requireAbsolutePath(value.isolatedCacheRoot, 'worker cache root'),
    viteModulePath: requireAbsolutePath(value.viteModulePath, 'worker Vite module path'),
  })
}

function freezeWorkerResult(value) {
  exactKeys(value, RESULT_KEYS, 'generated semantic worker result')
  if (value.schemaVersion !== GENERATED_SEMANTIC_WORKER_SCHEMA) {
    throw new Error('generated semantic worker result schema is unsupported')
  }
  if (!['built', 'failed'].includes(value.outcome)) {
    throw new Error('generated semantic worker result outcome is invalid')
  }
  const tools = value.tools === null ? null : freezeTools(value.tools)
  if (!Array.isArray(value.builds)) throw new Error('generated semantic worker builds must be an array')
  const builds = Object.freeze(value.builds.map(freezeBuild))
  if (value.outcome === 'built') {
    if (tools === null || builds.length === 0 || value.failure !== null) {
      throw new Error('successful generated semantic worker result is incomplete')
    }
  } else if (
    builds.length !== 0 || !isGeneratedSemanticFailure(value.failure) ||
    value.failure.kind !== 'build'
  ) {
    throw new Error('failed generated semantic worker result is incomplete')
  }
  return Object.freeze({
    schemaVersion: GENERATED_SEMANTIC_WORKER_SCHEMA,
    outcome: value.outcome,
    tools,
    builds,
    failure: value.failure === null ? null : Object.freeze({ ...value.failure }),
  })
}

function freezeTools(value) {
  exactKeys(value, TOOL_KEYS, 'generated semantic worker tools')
  for (const name of TOOL_KEYS) {
    if (typeof value[name] !== 'string' || !VERSION_PATTERN.test(value[name])) {
      throw new Error(`generated semantic ${name} version is invalid`)
    }
  }
  return Object.freeze({ vite: value.vite, rolldown: value.rolldown })
}

function freezeBuild(value) {
  exactKeys(value, ['outputs'], 'generated semantic build result')
  if (!Array.isArray(value.outputs)) throw new Error('generated semantic outputs must be an array')
  return Object.freeze({ outputs: Object.freeze(value.outputs.map(freezeOutput)) })
}

function freezeOutput(value) {
  if (!isRecord(value)) throw new Error('generated semantic output must be an object')
  if (value.type === 'asset') {
    exactKeys(value, ASSET_KEYS, 'generated semantic asset')
    return Object.freeze({ type: 'asset', fileName: requireString(value.fileName, 'asset file name') })
  }
  if (value.type !== 'chunk') throw new Error('generated semantic output type is invalid')
  exactKeys(value, CHUNK_KEYS, 'generated semantic chunk')
  return Object.freeze({
    type: 'chunk',
    fileName: requireString(value.fileName, 'chunk file name'),
    isEntry: requireBoolean(value.isEntry, 'chunk entry flag'),
    isDynamicEntry: requireBoolean(value.isDynamicEntry, 'chunk dynamic-entry flag'),
    exports: stringArray(value.exports, 'chunk exports'),
    imports: stringArray(value.imports, 'chunk imports'),
    dynamicImports: stringArray(value.dynamicImports, 'chunk dynamic imports'),
    code: requireString(value.code, 'chunk code'),
    hasSourceMap: requireBoolean(value.hasSourceMap, 'chunk source-map flag'),
  })
}

function stringArray(value, label) {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== 'string')) {
    throw new Error(`${label} must be an array of strings`)
  }
  return Object.freeze([...value])
}

function requireString(value, label) {
  if (typeof value !== 'string' || value.length === 0 || value.includes('\0')) {
    throw new Error(`${label} must be nonempty text`)
  }
  return value
}

function requireAbsolutePath(value, label) {
  const path = requireString(value, label)
  if (!isAbsolute(path) || resolve(path) !== path) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return path
}

function requireBoolean(value, label) {
  if (typeof value !== 'boolean') throw new Error(`${label} must be boolean`)
  return value
}

function exactKeys(value, keys, label) {
  if (!isRecord(value)) throw new Error(`${label} must be an object`)
  const actual = Object.keys(value)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
