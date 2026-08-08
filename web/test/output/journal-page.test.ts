import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  OUTPUT_JOURNAL_PAGE_RECORD_LIMIT,
  fileRecord,
  outputRecordKey,
  validateOutputJournalPage,
} from '../../src/output/persistence/journal'

const identity = Object.freeze({
  backend: 'page-test',
  outputSessionId: 'page-session',
  transferIntentDigest: fixedIdentity(0x10, 32),
  rootIdentity: fixedIdentity(0x40, 32),
})

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

  it('keeps runtime session identity out of durable records and requires the exact namespace', () => {
    const persisted = record(7)
    const legacyRuntimeBound = { ...persisted, outputSessionId: 'legacy-run' } as typeof persisted
    expect(persisted).not.toHaveProperty('outputSessionId')
    expect(() => validateOutputJournalPage(
      { records: [legacyRuntimeBound] },
      { kind: 'file', direction: 'ascending' },
      identity,
    )).toThrow('runtime session identity')
    expect(() => validateOutputJournalPage(
      { records: [persisted] },
      { kind: 'file', direction: 'ascending' },
      { ...identity, rootIdentity: fixedIdentity(0x60, 32) },
    )).toThrow('escaped its namespace')
  })
})

function record(index: number) {
  const name = `f-${index.toString().padStart(6, '0')}`
  return fileRecord(
    identity,
    {
      ...identity,
      canonicalPath: [name],
      ownedFileIdentity: indexedIdentity(0x50, 32, index),
    },
    {
      source: {
        shareInstance: 'share',
        fileId: indexedIdentity(0x20, 16, index),
        fileRevision: fixedIdentity(0x30, 16),
      },
      path: [name],
      exactSize: 0n,
    },
    [],
    true,
    1n,
  )
}

function fixedIdentity(first: number, length: number): string {
  return encodeBase64Url(Uint8Array.from(
    { length },
    (_, index) => (first + index) & 0xff,
  ))
}

function indexedIdentity(first: number, length: number, value: number): string {
  const bytes = Uint8Array.from(
    { length },
    (_, index) => (first + index) & 0xff,
  )
  new DataView(bytes.buffer).setUint32(length - 4, value, false)
  return encodeBase64Url(bytes)
}
