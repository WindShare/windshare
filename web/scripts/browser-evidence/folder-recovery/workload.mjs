import { createHash } from 'node:crypto'
import { contentBytes } from '../fsa-small-file/content.mjs'

export const MIB = 1024 * 1024
export const STAGED_FILE_BYTES = 24 * MIB
export const COPY_FAILURE_BYTES = 4 * MIB
export const CHUNK_BYTES = MIB
export const SMALL_FILE_COUNT = 32
export const SMALL_FILE_BYTES = 4 * 1024
export const MAX_CASE_BYTES = 64 * MIB
export const SCHEMA = 'windshare/folder-recovery-evidence/v1'

export function file(ordinal, sizeBytes, placement) {
  return Object.freeze({
    ordinal, sizeBytes, placement,
    path: `group-${ordinal % 2}/file-${ordinal}.bin`,
    sha256: createHash('sha256').update(contentBytes(ordinal, sizeBytes)).digest('hex'),
  })
}

export function workloads() {
  const small = Array.from({ length: SMALL_FILE_COUNT }, (_, ordinal) =>
    file(ordinal, SMALL_FILE_BYTES, 'direct'))
  const mixed = [
    file(0, SMALL_FILE_BYTES, 'direct'), file(1, STAGED_FILE_BYTES, 'staged'),
    file(2, SMALL_FILE_BYTES, 'direct'), file(3, STAGED_FILE_BYTES, 'staged'),
  ]
  return { small: validateFiles(small), mixed: validateFiles(mixed) }
}

export function validateFiles(files) {
  if (!Array.isArray(files) || files.length === 0 || files.length > 128) throw new Error('Invalid file count')
  const paths = new Set()
  let bytes = 0
  for (const entry of files) {
    if (!Number.isSafeInteger(entry.sizeBytes) || entry.sizeBytes < 0) throw new Error('Invalid file size')
    if (!Number.isSafeInteger(entry.ordinal) || entry.ordinal < 0) throw new Error('Invalid ordinal')
    if (!/^[a-z0-9-]+\/[a-z0-9.-]+$/u.test(entry.path) || entry.path.includes('..')) throw new Error('Unsafe workload path')
    if (paths.has(entry.path)) throw new Error('Duplicate workload path')
    if (!['direct', 'staged'].includes(entry.placement)) throw new Error('Invalid placement')
    if (!/^[0-9a-f]{64}$/u.test(entry.sha256)) throw new Error('Invalid digest')
    paths.add(entry.path)
    bytes += entry.sizeBytes
  }
  if (!Number.isSafeInteger(bytes) || bytes > MAX_CASE_BYTES) throw new Error('Workload exceeds bounded case size')
  return Object.freeze(files)
}
