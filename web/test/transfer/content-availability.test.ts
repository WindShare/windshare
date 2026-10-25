import { describe, expect, it } from 'vitest'
import {
  assertContentAvailabilityAdvance, reusableContentBytes, reusableContentPrefix,
  snapshotContentAvailability, type ContentAvailability,
} from '../../src/transfer/output/content-availability'
import { contentReplica } from './content-replica-fixture'

const source = { shareInstance: 'share', fileId: 'file', fileRevision: 'revision' }
const content = (overrides: Partial<ContentAvailability> = {}) => snapshotContentAvailability({
  source, exactSize: 8n, replica: contentReplica(), ranges: [{ start: 0n, end: 2n }], ...overrides,
})

describe('reusable content evidence', () => {
  it('canonicalizes CRC segments without counting unwritten gaps', () => {
    const evidence = content({ ranges: [{ start: 0n, end: 2n }, { start: 2n, end: 4n }, { start: 6n, end: 8n }] })
    expect(evidence.ranges).toEqual([{ start: 0n, end: 4n }, { start: 6n, end: 8n }])
    expect(reusableContentBytes(evidence)).toBe(6n)
    expect(reusableContentPrefix(evidence, source)).toBe(4n)
  })

  it.each(['shareInstance', 'fileId', 'fileRevision'] as const)('never reuses another %s', key => {
    expect(reusableContentPrefix(content(), { ...source, [key]: 'other' })).toBe(0n)
  })

  it('requires an accessible replica and complete authenticated source identity', () => {
    expect(() => content({ replica: null })).toThrow(/replica/)
    expect(() => content({ source: { ...source, shareInstance: '' } })).toThrow(/identity/)
    expect(reusableContentBytes(content({ replica: null, ranges: [] }))).toBe(0n)
  })

  it('binds every range advance to the same replica and a new commit generation', () => {
    const previous = content({ replica: contentReplica(2n) })
    expect(() => assertContentAvailabilityAdvance(previous, content())).toThrow(/generation/)
    expect(() => assertContentAvailabilityAdvance(previous, content({
      replica: contentReplica(2n), ranges: [{ start: 0n, end: 4n }],
    }))).toThrow(/generation/)
    expect(() => assertContentAvailabilityAdvance(previous, content({
      replica: { ...contentReplica(3n), objectId: 'replaced-object' },
    }))).toThrow(/replica/)
    expect(() => assertContentAvailabilityAdvance(previous, content({
      replica: contentReplica(3n), ranges: [{ start: 0n, end: 4n }],
    }))).not.toThrow()
  })
})
