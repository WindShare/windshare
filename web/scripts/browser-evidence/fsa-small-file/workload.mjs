import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { CONTENT_ALGORITHM, contentBytes } from './content.mjs'
import {
  AT_MOST_16_KIB_COUNT,
  AT_MOST_64_KIB_COUNT,
  buildCanonicalWorkload,
  DIRECTORY_COUNT,
  EMPTY_FILE_COUNT,
  FILE_COUNT,
  MAXIMUM_DIRECTORY_DEPTH,
  MAXIMUM_FILE_SIZE_BYTES,
  MEDIAN_FILE_SIZE_BYTES,
  serializeCanonicalWorkload,
  TOTAL_BYTES,
  WORKLOAD_SCHEMA,
} from './generate-workload.mjs'

const MODULE_DIRECTORY = dirname(fileURLToPath(import.meta.url))
export const REPOSITORY_ROOT = resolve(MODULE_DIRECTORY, '../../../..')
export const WORKLOAD_PATH = resolve(REPOSITORY_ROOT, 'testdata/browser-evidence/v1/fsa-small-file-workload.json')
export const WORKLOAD_SCHEMA_PATH = resolve(REPOSITORY_ROOT, 'testdata/browser-evidence/v1/fsa-small-file-workload.schema.json')
export const WORKLOAD_DIGEST_PATH = resolve(REPOSITORY_ROOT, 'testdata/browser-evidence/v1/fsa-small-file-workload.sha256')

const SHA256_PATTERN = /^[0-9a-f]{64}$/
const RELATIVE_PATH_PATTERN = /^(?:[a-z0-9][a-z0-9._-]*\/)*[a-z0-9][a-z0-9._-]*$/
const EXPECTED_FACTS = Object.freeze({
  fileCount: FILE_COUNT,
  directoryCount: DIRECTORY_COUNT,
  totalBytes: TOTAL_BYTES,
  emptyFileCount: EMPTY_FILE_COUNT,
  medianFileSizeBytes: MEDIAN_FILE_SIZE_BYTES,
  atMost16KiBCount: AT_MOST_16_KIB_COUNT,
  atMost64KiBCount: AT_MOST_64_KIB_COUNT,
  maximumFileSizeBytes: MAXIMUM_FILE_SIZE_BYTES,
  maximumDirectoryDepth: MAXIMUM_DIRECTORY_DEPTH,
})

export function sha256(value) {
  return createHash('sha256').update(value).digest('hex')
}

function assertExactKeys(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} must be an object`)
  const actual = Object.keys(value).sort()
  const wanted = [...expected].sort()
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    throw new Error(`${label} keys must be exactly: ${wanted.join(', ')}`)
  }
}

function assertSafeInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`${label} must be a non-negative safe integer`)
}

function assertRelativePath(path, label) {
  if (typeof path !== 'string' || !RELATIVE_PATH_PATTERN.test(path) || path.includes('\\')) {
    throw new Error(`${label} must be a normalized relative path`)
  }
}

export function validateWorkload(workload) {
  assertExactKeys(workload, ['schema', 'contentAlgorithm', 'facts', 'digests', 'directories', 'files'], 'workload')
  if (workload.schema !== WORKLOAD_SCHEMA) throw new Error(`Unsupported workload schema: ${workload.schema}`)
  if (workload.contentAlgorithm !== CONTENT_ALGORITHM) throw new Error(`Unsupported content algorithm: ${workload.contentAlgorithm}`)
  assertExactKeys(workload.facts, Object.keys(EXPECTED_FACTS), 'workload.facts')
  for (const [name, expected] of Object.entries(EXPECTED_FACTS)) {
    if (workload.facts[name] !== expected) throw new Error(`workload.facts.${name} must equal ${expected}`)
  }
  assertExactKeys(workload.digests, ['pathsSha256', 'sizesSha256', 'contentsSha256'], 'workload.digests')
  for (const [name, digest] of Object.entries(workload.digests)) {
    if (typeof digest !== 'string' || !SHA256_PATTERN.test(digest)) throw new Error(`workload.digests.${name} must be SHA-256`)
  }
  if (!Array.isArray(workload.directories) || workload.directories.length !== DIRECTORY_COUNT) {
    throw new Error(`workload.directories must contain exactly ${DIRECTORY_COUNT} entries`)
  }
  if (!Array.isArray(workload.files) || workload.files.length !== FILE_COUNT) {
    throw new Error(`workload.files must contain exactly ${FILE_COUNT} entries`)
  }
  const directories = new Set()
  for (const [index, path] of workload.directories.entries()) {
    assertRelativePath(path, `workload.directories[${index}]`)
    if (directories.has(path)) throw new Error(`Duplicate directory path: ${path}`)
    const slash = path.lastIndexOf('/')
    if (slash >= 0 && !directories.has(path.slice(0, slash))) throw new Error(`Directory parent is missing or out of order: ${path}`)
    directories.add(path)
  }
  const filePaths = new Set()
  for (const [index, file] of workload.files.entries()) {
    assertExactKeys(file, ['ordinal', 'path', 'sizeBytes', 'contentSha256'], `workload.files[${index}]`)
    if (file.ordinal !== index) throw new Error(`workload.files[${index}].ordinal must equal ${index}`)
    assertRelativePath(file.path, `workload.files[${index}].path`)
    assertSafeInteger(file.sizeBytes, `workload.files[${index}].sizeBytes`)
    if (!SHA256_PATTERN.test(file.contentSha256)) throw new Error(`workload.files[${index}].contentSha256 must be SHA-256`)
    if (filePaths.has(file.path) || directories.has(file.path)) throw new Error(`Duplicate workload path: ${file.path}`)
    const slash = file.path.lastIndexOf('/')
    if (slash < 0 || !directories.has(file.path.slice(0, slash))) throw new Error(`File parent is missing: ${file.path}`)
    const expectedDigest = sha256(contentBytes(file.ordinal, file.sizeBytes))
    if (file.contentSha256 !== expectedDigest) throw new Error(`Content digest mismatch in manifest: ${file.path}`)
    filePaths.add(file.path)
  }
  const canonical = buildCanonicalWorkload()
  if (serializeCanonicalWorkload(workload) !== serializeCanonicalWorkload(canonical)) {
    throw new Error('Workload differs from the canonical deterministic construction')
  }
  return workload
}

export async function loadCanonicalWorkload() {
  const [raw, sidecar] = await Promise.all([
    readFile(WORKLOAD_PATH),
    readFile(WORKLOAD_DIGEST_PATH, 'utf8'),
  ])
  const match = /^([0-9a-f]{64})  fsa-small-file-workload\.json\r?\n?$/.exec(sidecar)
  if (match === null) throw new Error('Workload SHA-256 sidecar has an invalid format')
  const digest = sha256(raw)
  if (digest !== match[1]) throw new Error(`Workload SHA-256 mismatch: observed=${digest} expected=${match[1]}`)
  const workload = validateWorkload(JSON.parse(raw.toString('utf8')))
  return Object.freeze({ workload, sha256: digest })
}
