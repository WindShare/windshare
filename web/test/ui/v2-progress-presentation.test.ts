import { describe, expect, it } from 'vitest'

import { presentDirectZipProgress } from '../../src/ui/progress-presentation'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import type { DirectZipProgressSnapshot } from '../../src/application/receive/contracts'

describe('v2 progress presentation', () => {
  it('keeps a complete direct ZIP below 100% until closing and verification have published it', () => {
    const progress = directZipProgress({
      phase: 'closing',
      receivedSelectedBytes: 1_024n,
      writtenSelectedBytes: 1_024n,
      safeResumeBytes: 768n,
      resumeTemporarySpaceUpperBound: 2_048n,
    })
    const closing = presentDirectZipProgress({
      progress,
      selectedBytes: { kind: 'exact', bytes: 1_024n },
      lifecycle: lifecycle('receiving'),
    })
    const published = presentDirectZipProgress({
      progress: { ...progress, phase: 'verifying', generation: 2n },
      selectedBytes: { kind: 'exact', bytes: 1_024n },
      lifecycle: lifecycle('published'),
    })

    expect(closing).toMatchObject({ percentage: 99n })
    expect(closing.primary).toContain('closing the ZIP')
    expect(closing.safeResume).toContain('768 B')
    expect(closing.temporarySpace).toContain('2.0 KiB')
    expect(published).toMatchObject({ percentage: 100n })
    expect(published.primary).toContain('saved and verified')
  })

  it('labels a lower-bound direct ZIP projection without inventing a percentage', () => {
    const presented = presentDirectZipProgress({
      progress: directZipProgress({
        receivedSelectedBytes: 512n, writtenSelectedBytes: 384n, safeResumeBytes: 256n,
      }),
      selectedBytes: { kind: 'estimated-lower-bound', bytes: 1_024n },
      lifecycle: lifecycle('receiving'),
    })

    expect(presented.primary).toContain('512 B received')
    expect(presented.written).toBe('384 B written or reused in the ZIP.')
    expect(presented.safeResume).toContain('256 B')
    expect(presented.percentage).toBeNull()
    expect(presented.primary).toContain('estimated selection is at least 1.0 KiB')
    expect(presented.primary).not.toContain('%')
  })
})

function directZipProgress(
  overrides: Partial<DirectZipProgressSnapshot>,
): DirectZipProgressSnapshot {
  return Object.freeze({
    kind: 'direct-zip',
    operationId: 'operation',
    generation: 1n,
    phase: 'receiving',
    receivedSelectedBytes: 0n,
    writtenSelectedBytes: 0n,
    safeResumeBytes: 0n,
    ...overrides,
  })
}

function lifecycle(kind: 'receiving' | 'published'): ReceiveLifecycleState {
  return kind === 'receiving'
    ? Object.freeze({
        kind,
        operationId: 'operation',
        receiveIntentDigest: 'intent',
        generation: 1n,
        activeLeaseId: 'lease',
      })
    : Object.freeze({
        kind,
        operationId: 'operation',
        receiveIntentDigest: 'intent',
        generation: 2n,
        receiptDigest: 'receipt',
        cleanupState: 'clean',
      })
}
