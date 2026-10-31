import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { ThroughputController, type ThroughputObservation } from '../../src/connectivity/peer-set/throughput'

const cases = JSON.parse(readFileSync(new URL('../../../testdata/connectivity/webrtc/throughput-control.json', import.meta.url), 'utf8')) as
  readonly { name: string; maximumPaths: number; samples: readonly (ThroughputObservation & { at: number; expected: string; expectedReason?: string; expectedLaneEpoch?: number; expectedRetryAfterMilliseconds?: number })[] }[]

describe('shared aggregate throughput decisions', () => {
  for (const scenario of cases) it(scenario.name, () => {
    const controller = new ThroughputController()
    for (const sample of scenario.samples) {
      const decision = controller.step(sample.at, sample, scenario.maximumPaths)
      expect(decision.action, 'at ' + sample.at).toBe(sample.expected)
      for (const value of Object.values(decision.work ?? {})) expect(value).toBeGreaterThanOrEqual(0)
      if (sample.expectedReason !== undefined) expect(decision.reason).toBe(sample.expectedReason)
      if (sample.expectedLaneEpoch !== undefined) expect(decision.path?.laneEpoch).toBe(sample.expectedLaneEpoch)
      if (sample.expectedRetryAfterMilliseconds !== undefined) expect(decision.retryAfterMilliseconds).toBe(sample.expectedRetryAfterMilliseconds)
    }
  })
})
