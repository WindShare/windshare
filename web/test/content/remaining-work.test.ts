import { expect, it } from 'vitest'
import { V2LaneSet } from '../../src/content/v2-lane-set'

it('counts a job once across parallel ranges and keeps progressive discovery unknown', () => {
  const lanes = new V2LaneSet()
  let complete = false
  const job = () => ({ bytes: 500, known: complete })
  const first = lanes.observeRemainingWork(job)
  const parallel = lanes.observeRemainingWork(job)
  const other = lanes.observeRemainingWork(() => ({ bytes: 100, known: true }))
  expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 600, remainingKnown: false })
  complete = true
  expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 600, remainingKnown: true })
  first()
  expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 600, remainingKnown: true })
  parallel()
  expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 100, remainingKnown: true })
  other()
  expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 0, remainingKnown: false })
})
