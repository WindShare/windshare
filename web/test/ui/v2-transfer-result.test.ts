import { describe, expect, it } from 'vitest'

import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import type { TransferWorkerSettlement } from '../../src/transfer/outcome'
import { presentTransferResult } from '../../src/ui/v2-transfer-result'

describe('transfer result presentation', () => {
  it('uses source, local checkpoint, collision, then residual headline priority without losing counts', () => {
    const presentation = presentTransferResult(worker({
      sourceDriftFiles: 1,
      revisionConflictFiles: 2,
      checkpointInvalidFiles: 3,
      ownedObjectUnknownFiles: 4,
      collisionFiles: 5,
      failedFiles: 6,
    }))
    expect(presentation.title).toBe('Source content changed')
    expect(presentation.lines.join(' ')).toMatch(/authenticated source content changed/u)
    expect(presentation.lines.join(' ')).toMatch(/local resume data belongs to another source revision/u)
    expect(presentation.lines.join(' ')).toMatch(/invalid local resume checkpoint/u)
    expect(presentation.lines.join(' ')).toMatch(/owned destination object/u)
    expect(presentation.lines.join(' ')).toMatch(/existing destinations prevented files from completing/u)
  })

  it.each([
    ['revision conflict', { revisionConflictFiles: 1 }, 'Resume revision conflict', /another source revision/u],
    ['invalid checkpoint', { checkpointInvalidFiles: 1 }, 'Invalid resume checkpoint', /invalid local resume checkpoint/u],
    ['owned object conflict', { ownedObjectUnknownFiles: 1 }, 'Resume ownership conflict', /owned destination object/u],
    ['destination collision', { collisionFiles: 1 }, 'Existing destinations prevented completion', /existing destination prevented/u],
  ] as const)('presents %s without source-change or needs-attention wording', (_name, patch, title, copy) => {
    const presentation = presentTransferResult(worker(patch))
    const text = `${presentation.title} ${presentation.lines.join(' ')}`
    expect(presentation.title).toBe(title)
    expect(text).toMatch(copy)
    expect(text).not.toMatch(/source content changed|needs attention/iu)
  })

  it('qualifies completed and partial terminal results without hiding ordinary failure evidence', () => {
    const completed = presentTransferResult(successfulWorker(), repairSummary('completed'))
    const partial = presentTransferResult(
      worker({ collisionFiles: 1 }),
      repairSummary('failed'),
    )

    expect(completed).toMatchObject({
      title: 'Completed with compatible names',
      tone: 'success',
    })
    expect(partial).toMatchObject({
      title: 'Partial with compatible names',
      tone: 'warning',
    })
    expect(partial.lines.join(' ')).toMatch(/existing destination prevented/u)
    expect(partial.lines.join(' ')).toContain('restore-names.windshare-abc234.ps1')
    expect(partial.lines.join(' ')).toContain('remain compatible')
  })

  it('does not publish terminal completion before sidecar catch-up validates', () => {
    const presentation = presentTransferResult(
      successfulWorker(),
      repairSummary('completed', true),
    )

    expect(presentation.title).toBe('Compatible-name restoration catch-up required')
    expect(presentation.tone).toBe('warning')
  })

  it('does not qualify a terminal summary with no committed replacement', () => {
    const empty = Object.freeze({
      ...repairSummary('completed'),
      committedCount: 0,
      logicalPathSample: Object.freeze([]),
      latestObservedFooter: Object.freeze({ committedCount: 0, state: 'completed' as const }),
    })

    expect(presentTransferResult(successfulWorker(), empty)).toMatchObject({
      title: 'Compatible-name restoration catch-up required',
      tone: 'warning',
    })
  })
})

function worker(
  patch: Partial<TransferWorkerSettlement['fileOutcomes']>,
): TransferWorkerSettlement {
  const fileOutcomes = {
    sourceDriftFiles: 0,
    revisionConflictFiles: 0,
    checkpointInvalidFiles: 0,
    ownedObjectUnknownFiles: 0,
    collisionFiles: 0,
    failedFiles: 0,
    ...patch,
  }
  const fileFailureCount = Object.values(fileOutcomes).reduce((sum, count) => sum + count, 0)
  return Object.freeze({
    status: 'CompletedWithErrors',
    failures: Object.freeze([]),
    failureCount: fileFailureCount,
    fileFailureCount,
    omittedFailureCount: fileFailureCount,
    fileOutcomes: Object.freeze(fileOutcomes),
  })
}

function successfulWorker(): TransferWorkerSettlement {
  return Object.freeze({
    status: 'Succeeded',
    failures: Object.freeze([]),
    failureCount: 0,
    fileFailureCount: 0,
    omittedFailureCount: 0,
    fileOutcomes: Object.freeze({
      sourceDriftFiles: 0,
      revisionConflictFiles: 0,
      checkpointInvalidFiles: 0,
      ownedObjectUnknownFiles: 0,
      collisionFiles: 0,
      failedFiles: 0,
    }),
  })
}

function repairSummary(
  state: NonNullable<CompatibleNameRepairSummary['latestObservedFooter']>['state'],
  pendingCatchUp = false,
): CompatibleNameRepairSummary {
  return Object.freeze({
    committedCount: 1,
    logicalPathSample: Object.freeze([Object.freeze(['pyvenv.cfg'])]),
    pairDisplayNames: Object.freeze({
      script: 'restore-names.windshare-abc234.ps1',
      sidecar: 'restore-names.windshare-abc234.tsv',
    }),
    placement: 'inside-logical-root',
    runCommand: 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\\restore-names.windshare-abc234.ps1"',
    latestObservedFooter: Object.freeze({ committedCount: 1, state }),
    pendingCatchUp,
  })
}
