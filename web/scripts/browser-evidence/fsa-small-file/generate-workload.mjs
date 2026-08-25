import { createHash } from 'node:crypto'
import { writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { CONTENT_ALGORITHM, contentBytes } from './content.mjs'

export const WORKLOAD_SCHEMA = 'windshare/fsa-small-file-workload/v1'
export const FILE_COUNT = 582
export const DIRECTORY_COUNT = 105
export const TOTAL_BYTES = 6_762_858
export const EMPTY_FILE_COUNT = 31
export const MEDIAN_FILE_SIZE_BYTES = 3_961
export const AT_MOST_16_KIB_COUNT = 478
export const AT_MOST_64_KIB_COUNT = 568
export const MAXIMUM_FILE_SIZE_BYTES = 121_863
export const MAXIMUM_DIRECTORY_DEPTH = 8

const SMALL_FILE_LIMIT_BYTES = 16 * 1_024
const MEDIUM_FILE_LIMIT_BYTES = 64 * 1_024
const SHUFFLE_SEED = 0x5f3759df

function sha256(value) {
  return createHash('sha256').update(value).digest('hex')
}

function buildReferenceSizes() {
  const sorted = []
  for (let index = 0; index < EMPTY_FILE_COUNT; index += 1) sorted.push(0)
  for (let index = 0; index < 259; index += 1) {
    sorted.push(Math.round(128 + ((3_960 - 128) * index) / 258))
  }
  sorted.push(MEDIAN_FILE_SIZE_BYTES, MEDIAN_FILE_SIZE_BYTES)
  for (let index = 0; index < 186; index += 1) {
    sorted.push(Math.round(4_096 + ((SMALL_FILE_LIMIT_BYTES - 4_096) * index) / 185))
  }
  for (let index = 0; index < 90; index += 1) {
    sorted.push(Math.round(16_385 + ((49_152 - 16_385) * index) / 89))
  }
  for (let index = 0; index < 13; index += 1) sorted.push(90_000 + index * 1_024)
  sorted.push(TOTAL_BYTES - sorted.reduce((total, size) => total + size, 0))

  // The fixed shuffle prevents large files from becoming an artificial final scheduling wave.
  const shuffled = [...sorted]
  let state = SHUFFLE_SEED
  for (let index = shuffled.length - 1; index > 0; index -= 1) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
    const swapIndex = state % (index + 1)
    ;[shuffled[index], shuffled[swapIndex]] = [shuffled[swapIndex], shuffled[index]]
  }
  return shuffled
}

function buildDirectoryPaths() {
  const paths = []
  for (let index = 0; index < DIRECTORY_COUNT; index += 1) {
    const name = `d${index.toString().padStart(3, '0')}`
    if (index === 0) paths.push(name)
    else if (index < MAXIMUM_DIRECTORY_DEPTH) paths.push(`${paths[index - 1]}/${name}`)
    else {
      const parent = paths[(index - MAXIMUM_DIRECTORY_DEPTH) % (MAXIMUM_DIRECTORY_DEPTH - 1)]
      paths.push(`${parent}/${name}`)
    }
  }
  return paths
}

function lineDigest(lines) {
  return sha256(`${lines.join('\n')}\n`)
}

export function buildCanonicalWorkload() {
  const sizes = buildReferenceSizes()
  const directories = buildDirectoryPaths()
  const files = sizes.map((sizeBytes, ordinal) => ({
    ordinal,
    path: `${directories[(ordinal * 37) % directories.length]}/f${ordinal.toString().padStart(4, '0')}.bin`,
    sizeBytes,
    contentSha256: sha256(contentBytes(ordinal, sizeBytes)),
  }))
  const sortedSizes = [...sizes].sort((left, right) => left - right)
  const facts = {
    fileCount: files.length,
    directoryCount: directories.length,
    totalBytes: sizes.reduce((total, size) => total + size, 0),
    emptyFileCount: sizes.filter((size) => size === 0).length,
    medianFileSizeBytes: (sortedSizes[FILE_COUNT / 2 - 1] + sortedSizes[FILE_COUNT / 2]) / 2,
    atMost16KiBCount: sizes.filter((size) => size <= SMALL_FILE_LIMIT_BYTES).length,
    atMost64KiBCount: sizes.filter((size) => size <= MEDIUM_FILE_LIMIT_BYTES).length,
    maximumFileSizeBytes: Math.max(...sizes),
    maximumDirectoryDepth: Math.max(...directories.map((path) => path.split('/').length)),
  }
  const digests = {
    pathsSha256: lineDigest([
      ...directories.map((path) => `D\0${path}`),
      ...files.map((file) => `F\0${file.path}`),
    ]),
    sizesSha256: lineDigest(files.map((file) => `${file.ordinal}\0${file.sizeBytes}`)),
    contentsSha256: lineDigest(files.map((file) => `${file.ordinal}\0${file.contentSha256}`)),
  }
  return {
    schema: WORKLOAD_SCHEMA,
    contentAlgorithm: CONTENT_ALGORITHM,
    facts,
    digests,
    directories,
    files,
  }
}

export function serializeCanonicalWorkload(workload = buildCanonicalWorkload()) {
  return `${JSON.stringify(workload, null, 2)}\n`
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const serialized = serializeCanonicalWorkload()
  if (process.argv.includes('--write')) {
    const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../../..')
    const workloadPath = resolve(repositoryRoot, 'testdata/browser-evidence/v1/fsa-small-file-workload.json')
    const digestPath = resolve(repositoryRoot, 'testdata/browser-evidence/v1/fsa-small-file-workload.sha256')
    await writeFile(workloadPath, serialized, 'utf8')
    await writeFile(digestPath, `${sha256(serialized)}  fsa-small-file-workload.json\n`, 'utf8')
  } else {
    process.stdout.write(serialized)
  }
}
