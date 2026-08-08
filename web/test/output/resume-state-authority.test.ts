import { describe, expect, it } from 'vitest'

import {
  ResumeStateInventory,
  ResumeStateRef,
  type ResumeStateReferenceOwner,
} from '../../src/output/resume/authority'
import type { PausedTaskDescriptorV1 } from '../../src/output/resume/descriptor'

describe('resume-state references', () => {
  it('are one-use authority rather than replayable task identifiers', () => {
    const owner: ResumeStateReferenceOwner = { open: true }
    const pin = Object.freeze({ generation: 7 })
    const reference = new ResumeStateRef(owner, descriptor(), 0, pin)

    expect(reference.consume(owner)).toBe(pin)
    expect(() => reference.consume(owner)).toThrowError(
      expect.objectContaining({ name: 'InvalidStateError' }),
    )
  })

  it('are invalidated when their inventory closes', () => {
    const owner: ResumeStateReferenceOwner = { open: true }
    const reference = new ResumeStateRef(owner, descriptor(), 0, {})
    const inventory = new ResumeStateInventory(owner, [reference])

    inventory.close()

    expect(() => reference.consume(owner)).toThrowError(
      expect.objectContaining({ name: 'InvalidStateError' }),
    )
  })
})

function descriptor(): PausedTaskDescriptorV1 {
  return {
    intent: {
      output: { backend: 'file-system-access' },
    },
  } as unknown as PausedTaskDescriptorV1
}
