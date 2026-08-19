import { describe, expect, it } from 'vitest'

import {
  CheckpointLineageDecisionError,
  DestinationCollisionError,
} from '../../src/output/persistent-tree/errors'
import {
  transferFileOutcomeEvidence,
  V2FileRevisionChangedError,
} from '../../src/transfer/job/failures'
import { projectTransferFileOutcome } from '../../src/transfer/outcome'

describe('v2 file outcome handoff', () => {
  it.each([
    ['revision-conflict', new CheckpointLineageDecisionError('revision-conflict')],
    ['owned-object-unknown', new CheckpointLineageDecisionError('ownership-conflict')],
    ['checkpoint-invalid', new CheckpointLineageDecisionError('invalid')],
    ['destination-collision', new DestinationCollisionError()],
    ['source-drift', new V2FileRevisionChangedError('authenticated revision changed')],
  ] as const)('projects %s from the typed collaborator decision', (expected, decision) => {
    const evidence = transferFileOutcomeEvidence(new Error('adapter boundary', { cause: decision }))
    expect(evidence).toBeDefined()
    expect(projectTransferFileOutcome(evidence!)).toBe(expected)
  })
})
