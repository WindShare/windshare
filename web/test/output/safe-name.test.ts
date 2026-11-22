import { describe, expect, it } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import {
  SafeNameRegistry,
  decideCollisionName,
  deterministicSafeComponent,
} from '../../src/output/backends/safe-name'
import {
  completedTuplesBytes,
  fileRecordId,
  fromRangeTuples,
  maxRangeEnd,
  toRangeTuples,
} from '../../src/output/store/types'

describe('safe names and store range helpers', () => {
  it('allocates predictable collision names for files, dotfiles, and directories', () => {
    expect(decideCollisionName('report.pdf', 'file', 0)).toBe('report.pdf')
    expect(decideCollisionName('report.pdf', 'file', 1)).toBe('report (1).pdf')
    expect(decideCollisionName('.env', 'file', 1)).toBe('.env (1)')
    expect(decideCollisionName('Photos', 'directory', 2)).toBe('Photos (2)')
  })

  it('generates deterministic OS-safe component names without collisions', async () => {
    const first = await deterministicSafeComponent({ logicalPath: ['dir', 'CON.txt'], entryKind: 'file' })
    const repeat = await deterministicSafeComponent({ logicalPath: ['dir', 'CON.txt'], entryKind: 'file' })
    const other = await deterministicSafeComponent({ logicalPath: ['dir2', 'CON.txt'], entryKind: 'file' })
    expect(first).toBe(repeat)
    expect(first).not.toBe(other)
    expect(first).toMatch(/\.windshare-[0-9a-f]{8}$/u)

    const registry = new SafeNameRegistry()
    registry.record(['dir', 'CON.txt'], 'file', first)
    expect(registry.physicalComponent(['dir', 'CON.txt'], 'file')).toBe(first)
    expect(registry.summary()).toMatchObject({ count: 1 })
  })

  it('converts byte ranges and measures completed coverage accurately', () => {
    const tuples = toRangeTuples([byteRange(0n, 100n), byteRange(200n, 350n)])
    expect(tuples).toEqual([[0n, 100n], [200n, 350n]])
    expect(completedTuplesBytes(tuples)).toBe(250n)
    expect(maxRangeEnd(tuples)).toBe(350n)
    expect(fromRangeTuples(tuples)).toEqual([byteRange(0n, 100n), byteRange(200n, 350n)])
    expect(fileRecordId('task-1', 'file-1')).toBe('task-1:file-1')
  })
})
