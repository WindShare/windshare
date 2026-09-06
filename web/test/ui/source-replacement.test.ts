import { describe, expect, it, vi } from 'vitest'
import { findReplacementFile } from '../../src/ui/source-replacement/selection'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import { V2JoinedBrowserShare, type V2BrowsePage } from '../../src/ui/v2-gateway'
import { FakeJoinedShare, identityText } from './v2-receiver-orchestration-fixture'

function fixture() {
  const joined = new FakeJoinedShare(true)
  const failure = { entryId: 'entry', path: ['export', 'file'], sourcePath: ['file'] }
  const operation = {
    sourceRevisionFailures: { shareInstance: joined.descriptor.shareInstanceId, count: 1n, files: [failure] },
  } as unknown as V2RetainedReceiveOperation
  const page = vi.fn(async (directory: ReturnType<FakeJoinedShare['rootDirectory']>, pageIndex: number) => {
    const original = await joined.page(directory)
    return { ...original, pageIndex, pageCount: 2, entries: pageIndex === 0 ? [] :
      original.entries.map(entry => ({ ...entry, name: 'file' })) } as V2BrowsePage
  })
  const share = { descriptor: joined.descriptor as V2JoinedBrowserShare['descriptor'],
    rootDirectory: () => joined.rootDirectory(), page,
    childDirectory: () => { throw new Error('No nested directories expected') },
  }
  return { share, failure, operation }
}

describe('explicit replacement download selection', () => {
  it('resolves the current authenticated path across pages and leaves the original descriptor untouched', async () => {
    const { share, failure, operation } = fixture()
    const original = structuredClone(operation)
    const found = await findReplacementFile(share, operation, failure, new AbortController().signal)
    expect(found.entry.name).toBe('file')
    expect(share.page).toHaveBeenCalledTimes(2)
    expect(share.page.mock.calls.map(call => call[1])).toEqual([0, 1])
    expect(operation).toEqual(original)
    expect(found.page.pageIndex).toBe(1)
  })

  it('rejects another share and copied failure tokens before contacting the sender', async () => {
    const { share, failure, operation } = fixture()
    const signal = new AbortController().signal
    await expect(findReplacementFile(share, operation, { ...failure }, signal)).rejects.toThrow('matching share')
    await expect(findReplacementFile({ ...share, descriptor: {
      ...share.descriptor, shareInstanceId: identityText(99),
    } }, operation, failure, signal)).rejects.toThrow('matching share')
    expect(share.page).not.toHaveBeenCalled()
  })

  it('does not fall back to a directory or another path when the file has disappeared', async () => {
    const { share, failure, operation } = fixture()
    share.page.mockImplementation(async directory => ({
      directory, pageIndex: 0, pageCount: 1, entryCount: 0, omittedCount: 0n, entries: [],
    }))
    await expect(findReplacementFile(share, operation, failure, new AbortController().signal))
      .rejects.toThrow('no longer shared')
  })
})
