import assert from 'node:assert/strict'
import test from 'node:test'
import { readFile } from 'node:fs/promises'
import { buildCanonicalWorkload, serializeCanonicalWorkload } from '../generate-workload.mjs'
import {
  loadCanonicalWorkload,
  validateWorkload,
  WORKLOAD_PATH,
  WORKLOAD_SCHEMA_PATH,
} from '../workload.mjs'

test('checked workload is the exact deterministic 582-file construction', async () => {
  const canonical = await loadCanonicalWorkload()
  assert.equal(serializeCanonicalWorkload(canonical.workload), serializeCanonicalWorkload(buildCanonicalWorkload()))
  assert.deepEqual(canonical.workload.facts, {
    fileCount: 582,
    directoryCount: 105,
    totalBytes: 6_762_858,
    emptyFileCount: 31,
    medianFileSizeBytes: 3_961,
    atMost16KiBCount: 478,
    atMost64KiBCount: 568,
    maximumFileSizeBytes: 121_863,
    maximumDirectoryDepth: 8,
  })
  assert.equal(canonical.workload.files.filter((file) => file.sizeBytes === 0).length, 31)
  assert.equal(new Set(canonical.workload.directories).size, 105)
  assert.equal(Math.max(...canonical.workload.directories.map((path) => path.split('/').length)), 8)
})

test('workload schema and SHA-256 sidecar are repository-owned', async () => {
  const [canonical, raw, schema] = await Promise.all([
    loadCanonicalWorkload(),
    readFile(WORKLOAD_PATH, 'utf8'),
    readFile(WORKLOAD_SCHEMA_PATH, 'utf8').then(JSON.parse),
  ])
  assert.equal(schema.$id, canonical.workload.schema)
  assert.equal(JSON.parse(raw).digests.pathsSha256, canonical.workload.digests.pathsSha256)
  assert.match(canonical.sha256, /^[0-9a-f]{64}$/)
})

test('manifest validation rejects changed path, size, or content truth', () => {
  const canonical = buildCanonicalWorkload()
  for (const mutate of [
    (copy) => { copy.files[0].path = `${copy.directories[0]}/changed.bin` },
    (copy) => { copy.files[0].sizeBytes += 1 },
    (copy) => { copy.files[0].contentSha256 = '0'.repeat(64) },
  ]) {
    const copy = structuredClone(canonical)
    mutate(copy)
    assert.throws(() => validateWorkload(copy))
  }
})
