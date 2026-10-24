import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { ThroughputController, type ThroughputObservation } from '../../src/connectivity/peer-set/throughput'

const cases = JSON.parse(readFileSync(new URL('../../../testdata/connectivity/webrtc/throughput-control.json', import.meta.url), 'utf8')) as
  readonly { name: string; maximumPaths: number; samples: readonly (ThroughputObservation & { at: number; expected: string })[] }[]

describe('shared aggregate throughput decisions', () => {
  for (const scenario of cases) it(scenario.name, () => {
    const controller = new ThroughputController()
    for (const sample of scenario.samples) expect(controller.step(sample.at, sample, scenario.maximumPaths).action).toBe(sample.expected)
  })
})
