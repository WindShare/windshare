import { describe, expect, it } from 'vitest'
import { fitsReadAhead, estimateCompletion, estimateQueueMilliseconds, type DeliveryEstimate, type DeliveryLoad, type DeliveryProgress } from '../../src/content/scheduling/completion'
import { rescueDecision } from '../../src/content/scheduling/exploration'
import { loadVectorFile } from '../vectors'

type SchedulingCase = { name: string } & (
  | { kind: 'estimate'; load: DeliveryLoad; bytes: number; queueMs: number; expected: DeliveryEstimate }
  | { kind: 'read-ahead'; estimate: DeliveryEstimate; bytes: number; distance: number; aggregateRate: number; fits: boolean }
  | { kind: 'rescue'; progress: DeliveryProgress; initial: DeliveryEstimate; alternative: DeliveryEstimate; reason: string }
)

const vectors = loadVectorFile(new URL('../../../core/testvectors/content-scheduling-v1.json', import.meta.url))
if (vectors.kind !== 'content-scheduling-v1' || vectors.cases.length === 0) throw new Error('Missing scheduling behavior contract')

describe('shared delivery scheduling behavior', () => {
  it.each(vectors.cases as unknown as SchedulingCase[])('$name', vector => {
    if (vector.kind === 'estimate') {
      const estimate = estimateCompletion(vector.load, vector.bytes)
      expect(estimate.evidence).toBe(vector.expected.evidence)
      expect(estimate.milliseconds).toBeCloseTo(vector.expected.milliseconds, 6)
      expect(estimateQueueMilliseconds(vector.load)).toBeCloseTo(vector.queueMs, 6)
    } else if (vector.kind === 'read-ahead') {
      expect(fitsReadAhead(vector.estimate, vector.bytes, BigInt(vector.distance), vector.aggregateRate)).toBe(vector.fits)
    } else {
      expect(rescueDecision(vector.progress, vector.initial, vector.alternative) ?? '').toBe(vector.reason)
    }
  })
})
