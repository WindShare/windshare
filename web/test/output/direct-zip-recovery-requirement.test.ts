import { describe, expect, it } from 'vitest'
import type { DirectZipJournalRepository } from '../../src/output/direct-zip/journal/repository'
import { readDirectZipRecoveryRequirement } from '../../src/output/resume/direct-zip-checkpoint'
import { ReceiveOperationResumeAuthority } from '../../src/output/resume/authority'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'

const lifecycle: ReceiveLifecycleState = {
  kind: 'receiving', operationId: 'operation', receiveIntentDigest: 'intent', generation: 2n, activeLeaseId: 'lease',
}

function journal(completed: boolean, candidateKind?: 'epoch' | 'closing', predecessor = 'checkpoint') {
  return {
    readState: async () => ({ checkpointDigest: 'checkpoint', checkpoint: {
      receiveIntentDigest: 'intent', ...(completed ? { closingReplay: { completion: {} } } : {}),
    } }),
    readOperationCandidate: async () => candidateKind === undefined ? undefined :
      ({ kind: candidateKind, predecessorCheckpointDigest: predecessor }),
  } as unknown as Pick<DirectZipJournalRepository, 'readState' | 'readOperationCandidate'>
}

describe('Direct ZIP local completion recovery inventory', () => {
  it.each([
    [false, undefined, 'receive'], [false, 'epoch', 'receive'],
    [false, 'closing', 'verify-completion'], [true, undefined, 'verify-completion'],
  ] as const)('projects committed=%s candidate=%s as %s', async (completed, kind, requirement) => {
    await expect(readDirectZipRecoveryRequirement(lifecycle, journal(completed, kind))).resolves.toBe(requirement)
  })

  it('rejects final candidate lineage that no longer belongs to the committed target', async () => {
    await expect(readDirectZipRecoveryRequirement(lifecycle, journal(false, 'closing', 'foreign')))
      .rejects.toThrow('lost its committed predecessor')
  })

  it('offers local verification without promoting lifecycle truth to published', async () => {
    const source = { listLifecycleStates: async () => [lifecycle],
      readDirectZipRequirement: () => readDirectZipRecoveryRequirement(lifecycle, journal(false, 'closing')) }
    const authority = new ReceiveOperationResumeAuthority({ source, mutations: {
      resume: async () => undefined, cleanup: async () => undefined,
      discard: async () => ({ kind: 'already-absent' as const }),
    } })
    const inventory = await authority.listResumeState()
    expect(inventory.operations[0]?.descriptor).toMatchObject({
      continuation: 'verify-direct-zip-completion', lifecycle: { kind: 'receiving' },
    })
  })
})
