import assert from 'node:assert/strict'
import test from 'node:test'
import {
  BYTES_PER_MEGABIT,
  GIB,
  MAX_CONNECTIONS,
  MAX_DAILY_GIB,
  MAX_RATE_MBPS,
  draftFromPolicy,
  formatCeilingLimit,
  parseContributionDraft,
} from '../relay/contribution-policy.ts'

test('contribution policy draft round-trips decimals, zero daily budget, and optional uplink', () => {
  const initial = {
    enabled: true,
    daily_bytes: 10 * GIB,
    bytes_per_second: 25 * BYTES_PER_MEGABIT,
    connections: 64,
    host_uplink: null,
  }
  const draft = draftFromPolicy(initial)
  assert.deepEqual(draft, {
    enabled: true,
    daily_bytes: '10',
    bytes_per_second: '25',
    connections: '64',
    host_uplink: '',
  })
  assert.deepEqual(parseContributionDraft(draft), initial)

  const fractional = parseContributionDraft({
    enabled: false,
    daily_bytes: '0.5',
    bytes_per_second: '2.5',
    connections: '1',
    host_uplink: '100.5',
  })
  assert.deepEqual(fractional, {
    enabled: false,
    daily_bytes: GIB / 2,
    bytes_per_second: 312_500,
    connections: 1,
    host_uplink: 12_562_500,
  })

  const zeroDaily = parseContributionDraft({
    ...draft,
    daily_bytes: '0',
    host_uplink: '   ',
  })
  assert.deepEqual(zeroDaily, { ...initial, daily_bytes: 0, host_uplink: null })
})

test('contribution policy parser enforces safe integer bounds and rejects invalid drafts', () => {
  const valid = {
    enabled: true,
    daily_bytes: '10',
    bytes_per_second: '25',
    connections: '64',
    host_uplink: '',
  }

  const maxValid = parseContributionDraft({
    enabled: true,
    daily_bytes: String(MAX_DAILY_GIB),
    bytes_per_second: String(MAX_RATE_MBPS),
    connections: String(MAX_CONNECTIONS),
    host_uplink: String(MAX_RATE_MBPS),
  })
  assert.ok(maxValid)
  assert.equal(Number.isSafeInteger(maxValid.daily_bytes), true)
  assert.equal(Number.isSafeInteger(maxValid.bytes_per_second), true)
  assert.equal(Number.isSafeInteger(maxValid.connections), true)
  assert.equal(Number.isSafeInteger(maxValid.host_uplink), true)

  const invalidCases = [
    { ...valid, daily_bytes: '' },
    { ...valid, daily_bytes: '-1' },
    { ...valid, daily_bytes: String(MAX_DAILY_GIB + 1) },
    { ...valid, daily_bytes: '1e20' },
    { ...valid, bytes_per_second: '0' },
    { ...valid, bytes_per_second: '0.000003' },
    { ...valid, bytes_per_second: String(MAX_RATE_MBPS + 1) },
    { ...valid, connections: '0' },
    { ...valid, connections: '1.5' },
    { ...valid, connections: String(MAX_CONNECTIONS + 2) },
    { ...valid, host_uplink: '0' },
    { ...valid, host_uplink: '-5' },
    { ...valid, host_uplink: String(MAX_RATE_MBPS + 1) },
  ]
  for (const draft of invalidCases) {
    assert.equal(parseContributionDraft(draft), null, JSON.stringify(draft))
  }
})

test('formatCeilingLimit rounds fractional ceilings with units and preserves non-zero floors', () => {
  assert.equal(formatCeilingLimit(500_000_000_000, GIB, 'GiB', 'en'), '465.66 GiB')
  assert.equal(formatCeilingLimit(10 * GIB, GIB, 'GiB', 'en'), '10 GiB')
  assert.equal(formatCeilingLimit(0, GIB, 'GiB', 'en'), '0 GiB')
  assert.equal(formatCeilingLimit(1024, GIB, 'GiB', 'en'), '< 0.01 GiB')
  assert.equal(formatCeilingLimit(12_500_000, BYTES_PER_MEGABIT, 'Mbps', 'en'), '100 Mbps')
  assert.equal(formatCeilingLimit(10_485_760, BYTES_PER_MEGABIT, 'Mbps', 'en'), '83.89 Mbps')
  assert.equal(formatCeilingLimit(100, BYTES_PER_MEGABIT, 'Mbps', 'en'), '< 0.01 Mbps')
  assert.equal(formatCeilingLimit(64, 1, undefined, 'en'), '64')
  assert.equal(formatCeilingLimit(1024, 1, undefined, 'en'), '1,024')
})
