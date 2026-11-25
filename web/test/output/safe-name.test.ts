import { describe, expect, it } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import {
  SafeNameRegistry,
  decideCollisionName,
  deterministicSafeCandidates,
} from '../../src/output/backends/safe-name'
import { isPortableOutputName } from '../../src/output/names/policy'
import {
  completedTuplesBytes,
  fileRecordId,
  fromRangeTuples,
  maxRangeEnd,
  toRangeTuples,
} from '../../src/output/store/types'

const TEXT_ENCODER = new TextEncoder()

describe('safe names and store range helpers', () => {
  it('allocates predictable collision names for files, dotfiles, and directories', () => {
    expect(decideCollisionName('report.pdf', 'file', 0)).toBe('report.pdf')
    expect(decideCollisionName('report.pdf', 'file', 1)).toBe('report (1).pdf')
    expect(decideCollisionName('.env', 'file', 1)).toBe('.env (1)')
    expect(decideCollisionName('Photos', 'directory', 2)).toBe('Photos (2)')
  })

  it('generates per-task deterministic ordered safe candidates isolated across tasks and collision indices', async () => {
    const first = await deterministicSafeCandidates({ taskId: 'task-a', logicalPath: ['dir', 'CON.pdf'], entryKind: 'file' })
    const repeat = await deterministicSafeCandidates({ taskId: 'task-a', logicalPath: ['dir', 'CON.pdf'], entryKind: 'file' })
    const otherPath = await deterministicSafeCandidates({ taskId: 'task-a', logicalPath: ['dir2', 'CON.pdf'], entryKind: 'file' })
    const otherTask = await deterministicSafeCandidates({ taskId: 'task-b', logicalPath: ['dir', 'CON.pdf'], entryKind: 'file' })
    const nextCollision = await deterministicSafeCandidates({
      taskId: 'task-a', logicalPath: ['dir', 'CON.pdf'], entryKind: 'file', collisionIndex: 1,
    })

    expect(first).toEqual(repeat)
    expect(first).not.toEqual(otherPath)
    expect(first).not.toEqual(otherTask)
    expect(first).not.toEqual(nextCollision)
    expect(first).toHaveLength(2)
    expect(first[0]).toMatch(/^entry-CON\.windshare-[0-9a-f]{16}\.pdf$/u)
    expect(first[1]).toMatch(/^CON-pdf\.windshare-[0-9a-f]{16}$/u)
    expect(first.every(candidate => isPortableOutputName(candidate))).toBe(true)

    await expect(deterministicSafeCandidates({ taskId: '', logicalPath: ['CON.txt'], entryKind: 'file' })).rejects.toThrow(TypeError)

    const registry = new SafeNameRegistry()
    registry.record(['dir', 'CON.pdf'], 'file', first[0])
    expect(registry.physicalComponent(['dir', 'CON.pdf'], 'file')).toBe(first[0])
    expect(registry.summary()).toMatchObject({ count: 1 })

    expect(registry.claimDirectory(['CON'], ['entry-CON.windshare-0123456789abcdef'])).toBe('claimed')
    expect(registry.claimDirectory(['CON'], ['entry-CON.windshare-0123456789abcdef'])).toBe('owned')
    expect(registry.claimDirectory(['entry-CON.windshare-0123456789abcdef'], ['entry-CON.windshare-0123456789abcdef'])).toBe('conflict')
    registry.releaseDirectoryClaim(['entry-CON.windshare-0123456789abcdef'], ['entry-CON.windshare-0123456789abcdef'])
    expect(registry.claimDirectory(['CON'], ['entry-CON.windshare-0123456789abcdef'])).toBe('owned')
    registry.releaseDirectoryClaim(['CON'], ['entry-CON.windshare-0123456789abcdef'])
    expect(registry.claimDirectory(['entry-CON.windshare-0123456789abcdef'], ['entry-CON.windshare-0123456789abcdef'])).toBe('claimed')
  })

  it('preserves compatible file extensions only within portable limits and never for directories or dotfiles', async () => {
    // Illegal character in stem with portable extension -> [withExt, withoutExt] sharing one hash.
    const notes = await deterministicSafeCandidates({ taskId: 't1', logicalPath: ['notes:2026.pdf'], entryKind: 'file' })
    expect(notes).toHaveLength(2)
    expect(notes[0]).toMatch(/^notes-2026\.windshare-([0-9a-f]{16})\.pdf$/u)
    expect(notes[1]).toMatch(/^notes-2026-pdf\.windshare-([0-9a-f]{16})$/u)
    const token = notes[0].match(/\.windshare-([0-9a-f]{16})/u)![1]
    expect(notes[1]).toBe(`notes-2026-pdf.windshare-${token}`)

    // Dotfiles, trailing dot/space, invalid extension chars, and directories produce 1 extensionless candidate.
    for (const [leaf, entryKind] of [
      ['.env', 'file'],
      ['.gitignore', 'file'],
      ['report.', 'file'],
      ['report.pdf ', 'file'],
      ['a.b:c', 'file'],
      ['empty.cfg', 'directory'],
      ['CON.pdf', 'directory'],
    ] as const) {
      const candidates = await deterministicSafeCandidates({ taskId: 't1', logicalPath: [leaf], entryKind })
      expect(candidates, `${entryKind}:${leaf}`).toHaveLength(1)
      expect(candidates[0]).toMatch(/\.windshare-[0-9a-f]{16}$/u)
      expect(isPortableOutputName(candidates[0])).toBe(true)
    }

    // Extension byte budget: 32 UTF-8 bytes allowed, 33 bytes (ASCII or multibyte) dropped.
    const exactExt = `.${'x'.repeat(31)}`
    const exact = await deterministicSafeCandidates({ taskId: 't1', logicalPath: [`doc:${exactExt}`], entryKind: 'file' })
    expect(exact).toHaveLength(2)
    expect(exact[0].endsWith(exactExt)).toBe(true)

    const tooLongAscii = await deterministicSafeCandidates({
      taskId: 't1', logicalPath: [`doc:.${'x'.repeat(32)}`], entryKind: 'file',
    })
    expect(tooLongAscii).toHaveLength(1)

    const tooLongUtf8 = await deterministicSafeCandidates({
      taskId: 't1', logicalPath: ['doc:.中文扩展名超三十二字节'], entryKind: 'file',
    })
    expect(tooLongUtf8).toHaveLength(1)

    // Overlong filename (>255 UTF-8 bytes) truncates prefix while keeping portable extension under 255 bytes.
    const longCjk = `${'报告'.repeat(100)}.pdf`
    expect(TEXT_ENCODER.encode(longCjk).byteLength).toBeGreaterThan(255)
    const longCandidates = await deterministicSafeCandidates({ taskId: 't1', logicalPath: [longCjk], entryKind: 'file' })
    expect(longCandidates).toHaveLength(2)
    expect(longCandidates[0].endsWith('.pdf')).toBe(true)
    for (const candidate of longCandidates) {
      expect(TEXT_ENCODER.encode(candidate).byteLength).toBeLessThanOrEqual(255)
      expect(isPortableOutputName(candidate)).toBe(true)
    }
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
