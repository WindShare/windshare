import { createHash } from 'node:crypto'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { afterEach, describe, expect, it } from 'vitest'

import { readStableRegularFileSnapshot } from '../../scripts/browser-evidence/filesystem/snapshot.ts'

const workspaces: string[] = []

afterEach(async () => {
  await Promise.all(workspaces.splice(0).map((workspace) => rm(workspace, { recursive: true, force: true })))
})

describe('stable regular-file snapshots', () => {
  it('returns the exact bounded bytes and digest from one regular-file revision', async () => {
    const workspace = await trackedWorkspace()
    const path = join(workspace, 'contract.json')
    const bytes = Buffer.from('{"schemaVersion":1}', 'utf8')
    await writeFile(path, bytes)
    const snapshot = await readStableRegularFileSnapshot(path, bytes.byteLength, 'synthetic contract')
    expect(snapshot.bytes).toEqual(bytes)
    expect(snapshot.sha256).toBe(createHash('sha256').update(bytes).digest('hex'))
  })

  it('rejects both oversized inputs and non-file authorities before parsing', async () => {
    const workspace = await trackedWorkspace()
    const oversized = join(workspace, 'oversized.json')
    await writeFile(oversized, '12345', 'utf8')
    await expect(readStableRegularFileSnapshot(oversized, 4, 'synthetic contract'))
      .rejects.toThrow(/frozen contract byte limit/u)

    const directory = join(workspace, 'directory.json')
    await mkdir(directory)
    await expect(readStableRegularFileSnapshot(directory, 4, 'synthetic contract'))
      .rejects.toThrow(/not a regular file/u)
  })
})

async function trackedWorkspace(): Promise<string> {
  const workspace = await mkdtemp(join(tmpdir(), 'windshare-file-snapshot-'))
  workspaces.push(workspace)
  return workspace
}
