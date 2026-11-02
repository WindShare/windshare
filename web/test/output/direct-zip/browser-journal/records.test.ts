import { describe, expect, it, vi } from 'vitest'

import { planDirectZipEntryV2 } from '../../../../src/output/direct-zip/format'
import { encodeLayout, replayLayoutPlan } from '../../../../src/output/direct-zip/browser-journal/records'

const MODIFIED = BigInt(Date.UTC(2026, 10, 2, 1, 30))

function committedLayout() {
  const plan = planDirectZipEntryV2({
    ordinal: 1n,
    localHeaderOffset: 64n,
    entry: { kind: 'file', path: ['root', 'a.txt'], exactSize: 3n, modifiedTimeMilliseconds: MODIFIED },
  })
  return { plan, bytes: encodeLayout({ plan, layoutEvidence: Uint8Array.of(1), discoveryEvidence: Uint8Array.of(2) }) }
}

describe('Direct ZIP layout replay', () => {
  it('replays a member committed in UTC after resuming in another time zone', () => {
    // A journal written before local-time encoding, or before the zone changed.
    vi.stubEnv('TZ', 'UTC')
    const committed = committedLayout()
    vi.stubEnv('TZ', 'Asia/Singapore')
    expect(committedLayout().plan.zipEntry.dosTime).not.toBe(committed.plan.zipEntry.dosTime)

    expect(replayLayoutPlan(committed.bytes, 1n, { kind: 'file', path: ['root', 'a.txt'], exactSize: 3n }))
      .toEqual(committed.plan)
  })

  it('still rejects a replay whose authenticated layout changed', () => {
    const { bytes } = committedLayout()

    expect(() => replayLayoutPlan(bytes, 1n, { kind: 'file', path: ['root', 'b.txt'], exactSize: 3n }))
      .toThrow('Direct ZIP replay changed its committed entry plan')
    expect(() => replayLayoutPlan(bytes, 1n, { kind: 'directory', path: ['root', 'a.txt'] }))
      .toThrow('Direct ZIP replay changed its committed entry plan')
    expect(() => replayLayoutPlan(bytes, 2n, { kind: 'file', path: ['root', 'a.txt'], exactSize: 3n }))
      .toThrow('Direct ZIP replay changed its committed entry plan')
  })
})
