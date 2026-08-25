import assert from 'node:assert/strict'
import test from 'node:test'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { contentBytes } from '../content.mjs'
import { verifyHostTree } from '../host-verification.mjs'
import { loadCanonicalWorkload } from '../workload.mjs'

test('host verification proves every canonical path, byte count, digest, empty file, and directory', async () => {
  const root = await mkdtemp(join(tmpdir(), 'windshare-fsa-small-file-'))
  try {
    const canonical = await loadCanonicalWorkload()
    for (const directory of canonical.workload.directories) await mkdir(resolve(root, ...directory.split('/')))
    await Promise.all(canonical.workload.files.map(async (file) => {
      const path = resolve(root, ...file.path.split('/'))
      await mkdir(dirname(path), { recursive: true })
      await writeFile(path, contentBytes(file.ordinal, file.sizeBytes))
    }))
    const verified = await verifyHostTree({
      rootPath: root,
      workload: canonical.workload,
      workloadSha256: canonical.sha256,
      now: () => new Date('2026-08-24T00:00:00.000Z'),
    })
    assert.deepEqual({
      status: verified.status,
      fileCount: verified.fileCount,
      directoryCount: verified.directoryCount,
      totalBytes: verified.totalBytes,
      emptyFileCount: verified.emptyFileCount,
      mismatchCount: verified.mismatchCount,
    }, {
      status: 'verified',
      fileCount: 582,
      directoryCount: 105,
      totalBytes: 6_762_858,
      emptyFileCount: 31,
      mismatchCount: 0,
    })

    const nonEmpty = canonical.workload.files.find((file) => file.sizeBytes > 0)
    const changed = contentBytes(nonEmpty.ordinal, nonEmpty.sizeBytes)
    changed[0] ^= 1
    await writeFile(resolve(root, ...nonEmpty.path.split('/')), changed)
    await assert.rejects(
      verifyHostTree({ rootPath: root, workload: canonical.workload, workloadSha256: canonical.sha256 }),
      /Host file digest mismatch/,
    )
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})

test('host verification rejects extra topology before accepting aggregate-equivalent output', async () => {
  const root = await mkdtemp(join(tmpdir(), 'windshare-fsa-small-file-topology-'))
  try {
    const canonical = await loadCanonicalWorkload()
    for (const directory of canonical.workload.directories) await mkdir(resolve(root, ...directory.split('/')))
    await mkdir(resolve(root, 'unexpected'))
    await assert.rejects(
      verifyHostTree({ rootPath: root, workload: canonical.workload, workloadSha256: canonical.sha256 }),
      /Directory topology mismatch/,
    )
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})
