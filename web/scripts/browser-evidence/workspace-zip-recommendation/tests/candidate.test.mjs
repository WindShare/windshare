import assert from 'node:assert/strict'
import test from 'node:test'
import { deriveCandidate } from '../candidate.mjs'

const MIB = 1_048_576n

test('candidate stops at the largest three-repeat scale below the declared space boundary', () => {
  const cases = [32n, 128n, 256n, 512n].flatMap(raw => [1, 2, 3].map(repetition => successfulCase(raw, repetition)))
  const candidate = deriveCandidate(cases)
  assert.equal(candidate.kind, 'candidate')
  assert.equal(candidate.largestIncludedRawBytes, (512n * MIB).toString())
  assert.equal(candidate.workspacePeakBytesThreshold, (1_024n * MIB + 100n).toString())
  assert.equal(candidate.boundary.kind, 'evidence-live-owned-cap')
})

test('derives the exact independently reviewed workspace threshold and next-scale boundary', () => {
  const reviewedPeak = 1_073_744_986n
  const cases = [32n, 128n, 256n, 512n].flatMap(raw => [1, 2, 3].map(repetition => (
    successfulCase(raw, repetition, raw === 512n ? reviewedPeak : raw * MIB * 2n + 100n)
  )))

  const candidate = deriveCandidate(cases)

  assert.equal(candidate.kind, 'candidate')
  assert.equal(candidate.workspacePeakBytesThreshold, '1073744986')
  assert.equal(candidate.boundary.nextModeledPeakOwnedBytes, '2147489972')
  assert.equal(candidate.boundary.kind, 'evidence-live-owned-cap')
})

test('candidate remains insufficient when a scale lacks complete recovery and no earlier boundary exists', () => {
  const cases = [32n, 128n, 256n].flatMap(raw => [1, 2, 3].map(repetition => successfulCase(raw, repetition)))
  cases.push({ ...successfulCase(512n, 1), recovery: { sameOrigin: false } })
  const candidate = deriveCandidate(cases)
  assert.deepEqual(candidate, {
    kind: 'insufficient',
    schema: 'windshare/workspace-zip-recommendation-candidate/v1',
    reviewStatus: 'unreviewed',
    reason: 'boundary-not-located-run-optional-1gib',
  })
})

function successfulCase(rawMib, repetition, measuredPeak = undefined) {
  const raw = rawMib * MIB
  const peak = measuredPeak ?? raw * 2n + 100n
  return {
    rawBytes: raw.toString(),
    repetition,
    status: 'passed',
    cost: { peakOwnedBytes: peak.toString() },
    responsiveness: { commandP99Milliseconds: 5 },
    recovery: { sameOrigin: true, sameProfile: true, fullReadback: true },
    cleanup: { productionState: 'discarded', ownedStorageEntryCount: 0 },
    sampler: { peakTotalOwnedBytes: peak.toString() },
  }
}
