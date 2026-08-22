import { CANDIDATE_SCHEMA, LIVE_OWNED_CAP_BYTES, OPTIONAL_RAW_BYTES } from './constants.mjs'

export function deriveCandidate(cases) {
  const ordered = [...cases].sort((left, right) => (
    BigInt(left.rawBytes) === BigInt(right.rawBytes)
      ? left.repetition - right.repetition
      : BigInt(left.rawBytes) < BigInt(right.rawBytes) ? -1 : 1
  ))
  const groups = new Map()
  for (const item of ordered) {
    const key = item.rawBytes
    const group = groups.get(key) ?? []
    group.push(item)
    groups.set(key, group)
  }
  const included = []
  for (const [rawBytes, repetitions] of groups) {
    if (repetitions.length !== 3 || repetitions.some(item => !caseSupportsCandidate(item))) break
    included.push({ rawBytes, repetitions })
  }
  if (included.length === 0) return insufficiency('no-scale-has-three-complete-repetitions')
  const largest = included.at(-1)
  const largestRawBytes = BigInt(largest.rawBytes)
  const observedPeak = BigInt(largest.repetitions[0].cost.peakOwnedBytes)
  if (largest.repetitions.some(item => BigInt(item.cost.peakOwnedBytes) !== observedPeak)) {
    return insufficiency('canonical-cost-changed-between-repetitions')
  }
  const nextRawBytes = nextScale(largestRawBytes)
  const nextGroup = groups.get(nextRawBytes.toString())
  const modeledNextPeak = nextGroup?.[0] === undefined
    ? extrapolatePeak(largestRawBytes, observedPeak, nextRawBytes)
    : BigInt(nextGroup[0].cost.peakOwnedBytes)
  const spaceBoundaryLocated = modeledNextPeak > LIVE_OWNED_CAP_BYTES
  const responsivenessBoundaryLocated = hasResponsivenessKink(included)
  if (!spaceBoundaryLocated && !responsivenessBoundaryLocated && largestRawBytes < OPTIONAL_RAW_BYTES) {
    return insufficiency('boundary-not-located-run-optional-1gib')
  }
  return Object.freeze({
    kind: 'candidate',
    schema: CANDIDATE_SCHEMA,
    reviewStatus: 'unreviewed',
    workspacePeakBytesThreshold: observedPeak.toString(),
    largestIncludedRawBytes: largestRawBytes.toString(),
    includedRawBytes: Object.freeze(included.map(group => group.rawBytes)),
    boundary: Object.freeze({
      kind: spaceBoundaryLocated ? 'evidence-live-owned-cap' : 'measured-responsiveness-kink',
      nextRawBytes: nextRawBytes.toString(),
      nextModeledPeakOwnedBytes: modeledNextPeak.toString(),
      liveOwnedCapBytes: LIVE_OWNED_CAP_BYTES.toString(),
    }),
    rationale: spaceBoundaryLocated
      ? 'All included scales completed three process-cut recoveries and cleanups; the next scale exceeds the predeclared live-owned safety cap under the same exact checked cost model.'
      : 'All included scales completed three process-cut recoveries and cleanups; the next measured scale introduced a repeatable responsiveness discontinuity.',
  })
}

export function caseSupportsCandidate(item) {
  return item.status === 'passed' && item.recovery?.sameOrigin === true &&
    item.recovery?.sameProfile === true && item.recovery?.fullReadback === true &&
    item.cleanup?.productionState === 'discarded' && item.cleanup?.ownedStorageEntryCount === 0 &&
    BigInt(item.sampler?.peakTotalOwnedBytes ?? -1) <= LIVE_OWNED_CAP_BYTES
}

function nextScale(rawBytes) {
  return rawBytes < OPTIONAL_RAW_BYTES ? rawBytes * 2n : rawBytes + OPTIONAL_RAW_BYTES
}

function extrapolatePeak(rawBytes, peakBytes, nextRawBytes) {
  if (rawBytes <= 0n || nextRawBytes <= rawBytes) throw new Error('Candidate scale order is invalid')
  return peakBytes * nextRawBytes / rawBytes
}

function hasResponsivenessKink(included) {
  if (included.length < 2) return false
  const current = included.at(-1).repetitions.map(item => item.responsiveness.commandP99Milliseconds)
  const previous = included.at(-2).repetitions.map(item => item.responsiveness.commandP99Milliseconds)
  return median(current) >= median(previous) * 2 && median(current) - median(previous) >= 100
}

function median(values) {
  const ordered = [...values].sort((left, right) => left - right)
  return ordered[Math.floor(ordered.length / 2)]
}

function insufficiency(reason) {
  return Object.freeze({ kind: 'insufficient', schema: CANDIDATE_SCHEMA, reviewStatus: 'unreviewed', reason })
}
