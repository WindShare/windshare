import { describe, expect, it } from 'vitest'

import {
  OUTPUT_JOURNAL_PAGE_RECORD_LIMIT,
  fileRecord,
  outputRecordKey,
  validateOutputJournalPage,
} from '../../src/output/persistence/journal'

const identity = Object.freeze({ backend: 'page-test', outputSessionId: 'page-session' })

describe('output journal page validation', () => {
  it('rejects oversized pages and a full page that truncates continuation', () => {
    const oversized = Array.from(
      { length: OUTPUT_JOURNAL_PAGE_RECORD_LIMIT + 1 },
      (_, index) => record(index),
    )
    expect(() => validateOutputJournalPage(
      { records: oversized },
      { kind: 'file', direction: 'ascending' },
      identity,
    )).toThrow('fixed record limit')
    expect(() => validateOutputJournalPage(
      { records: oversized.slice(0, OUTPUT_JOURNAL_PAGE_RECORD_LIMIT) },
      { kind: 'file', direction: 'ascending' },
      identity,
    )).toThrow('omitted its continuation')
  })

  it('rejects repeated, out-of-order, and forged tail cursors', () => {
    const first = record(1)
    const second = record(2)
    const firstKey = outputRecordKey(first)
    expect(() => validateOutputJournalPage(
      { records: [first] },
      { kind: 'file', direction: 'ascending', cursor: firstKey },
      identity,
    )).toThrow('did not advance')
    expect(() => validateOutputJournalPage(
      { records: [second, first] },
      { kind: 'file', direction: 'ascending' },
      identity,
    )).toThrow('did not advance')
    expect(() => validateOutputJournalPage(
      { records: [first], nextCursor: outputRecordKey(second) },
      { kind: 'file', direction: 'ascending' },
      identity,
    )).toThrow('tail')
  })

  it('enforces the same strict cursor order while descending', () => {
    const first = record(1)
    const second = record(2)
    expect(validateOutputJournalPage(
      { records: [second, first] },
      { kind: 'file', direction: 'descending' },
      identity,
    ).records).toHaveLength(2)
    expect(() => validateOutputJournalPage(
      { records: [first, second] },
      { kind: 'file', direction: 'descending' },
      identity,
    )).toThrow('did not advance')
  })
})

function record(index: number) {
  const name = `f-${index.toString().padStart(6, '0')}`
  return fileRecord(
    identity,
    { ...identity, canonicalPath: [name], ownedFileIdentity: `owned-${name}` },
    {
      source: {
        shareInstance: 'share',
        fileId: name,
        fileRevision: 'revision',
      },
      path: [name],
      exactSize: 0n,
    },
    [],
    true,
    1n,
  )
}
