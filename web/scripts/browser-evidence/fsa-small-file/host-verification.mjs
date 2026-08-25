import { createReadStream } from 'node:fs'
import { readdir, stat } from 'node:fs/promises'
import { resolve } from 'node:path'
import { createHash } from 'node:crypto'
import { DIRECTORY_COUNT, EMPTY_FILE_COUNT, FILE_COUNT, TOTAL_BYTES } from './generate-workload.mjs'

export const HOST_VERIFICATION_SCHEMA = 'windshare/fsa-small-file-host-verification/v1'

async function fileSha256(path) {
  const hash = createHash('sha256')
  for await (const chunk of createReadStream(path)) hash.update(chunk)
  return hash.digest('hex')
}

async function inventory(rootPath, relative = '', entries = { directories: [], files: [] }) {
  const current = relative === '' ? rootPath : resolve(rootPath, ...relative.split('/'))
  const children = await readdir(current, { withFileTypes: true })
  children.sort((left, right) => left.name.localeCompare(right.name, 'en'))
  for (const child of children) {
    const childRelative = relative === '' ? child.name : `${relative}/${child.name}`
    if (child.isSymbolicLink()) throw new Error(`Host tree contains a symbolic link: ${childRelative}`)
    if (child.isDirectory()) {
      entries.directories.push(childRelative)
      await inventory(rootPath, childRelative, entries)
    } else if (child.isFile()) {
      entries.files.push(childRelative)
    } else {
      throw new Error(`Host tree contains an unsupported entry: ${childRelative}`)
    }
  }
  return entries
}

function comparePaths(actual, expected, kind) {
  const actualSet = new Set(actual)
  const expectedSet = new Set(expected)
  const missing = expected.filter((path) => !actualSet.has(path))
  const unexpected = actual.filter((path) => !expectedSet.has(path))
  if (missing.length > 0 || unexpected.length > 0) {
    throw new Error(`${kind} topology mismatch: missing=${missing.slice(0, 3).join(',') || 'none'} unexpected=${unexpected.slice(0, 3).join(',') || 'none'}`)
  }
}

export async function verifyHostTree({ rootPath, workload, workloadSha256, now = () => new Date() }) {
  const absoluteRoot = resolve(rootPath)
  const rootStat = await stat(absoluteRoot)
  if (!rootStat.isDirectory()) throw new Error(`Host verification root is not a directory: ${absoluteRoot}`)
  const observed = await inventory(absoluteRoot)
  comparePaths(observed.directories, workload.directories, 'Directory')
  comparePaths(observed.files, workload.files.map((file) => file.path), 'File')

  let totalBytes = 0
  let emptyFileCount = 0
  for (const expected of workload.files) {
    const path = resolve(absoluteRoot, ...expected.path.split('/'))
    const metadata = await stat(path)
    if (metadata.size !== expected.sizeBytes) {
      throw new Error(`Host file size mismatch: ${expected.path} observed=${metadata.size} expected=${expected.sizeBytes}`)
    }
    const digest = await fileSha256(path)
    if (digest !== expected.contentSha256) throw new Error(`Host file digest mismatch: ${expected.path}`)
    totalBytes += metadata.size
    if (metadata.size === 0) emptyFileCount += 1
  }
  if (observed.files.length !== FILE_COUNT || observed.directories.length !== DIRECTORY_COUNT || totalBytes !== TOTAL_BYTES || emptyFileCount !== EMPTY_FILE_COUNT) {
    throw new Error('Host verification aggregate invariant failed after per-path verification')
  }
  return Object.freeze({
    schema: HOST_VERIFICATION_SCHEMA,
    status: 'verified',
    workloadSha256,
    rootPath: absoluteRoot,
    verifiedAt: now().toISOString(),
    fileCount: observed.files.length,
    directoryCount: observed.directories.length,
    totalBytes,
    emptyFileCount,
    mismatchCount: 0,
  })
}
