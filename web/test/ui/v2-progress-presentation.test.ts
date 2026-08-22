import { describe, expect, it } from 'vitest'

import type { V2ReceiverProgress } from '../../src/ui/v2-model'
import {
  completionProgressDescription,
  discoveryProgressDescription,
  presentDirectZipProgress,
} from '../../src/ui/v2-progress-presentation'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import type { V2DirectZipProgressSnapshot } from '../../src/ui/v2-receive-runtime'

describe('v2 progress presentation', () => {
  it.each(['open', 'failed'] as const)('keeps %s discovery and completion totals as lower bounds', (discovery) => {
    const progress = receiverProgress({ discovery, failedDirectories: discovery === 'failed' ? 1 : 0 })

    expect(discoveryProgressDescription(progress)).toContain('At least 2 file(s), 1.0 KiB discovered')
    expect(completionProgressDescription(progress)).toBe(
      '512 B received · 1 file(s) completed (512 B committed; final total unknown)',
    )
    expect(completionProgressDescription(progress)).not.toContain('/2')
    expect(completionProgressDescription(progress)).not.toContain('%')
  })

  it('shows exact completed denominators and percentage only after discovery completes', () => {
    const progress = receiverProgress({ discovery: 'complete' })

    expect(discoveryProgressDescription(progress)).toBe('2 file(s), 1.0 KiB total')
    expect(completionProgressDescription(progress)).toBe(
      '512 B received · 1/2 file(s) completed · 512 B/1.0 KiB committed (50%)',
    )
  })

  it('keeps a complete direct ZIP below 100% until closing and verification have published it', () => {
    const progress = directZipProgress({
      phase: 'closing',
      receivedSelectedBytes: 1_024n,
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
      progress: directZipProgress({ receivedSelectedBytes: 512n, safeResumeBytes: 256n }),
      selectedBytes: { kind: 'estimated-lower-bound', bytes: 1_024n },
      lifecycle: lifecycle('receiving'),
    })

    expect(presented.percentage).toBeNull()
    expect(presented.primary).toContain('estimated selection is at least 1.0 KiB')
    expect(presented.primary).not.toContain('%')
  })
})

function directZipProgress(
  overrides: Partial<V2DirectZipProgressSnapshot>,
): V2DirectZipProgressSnapshot {
  return Object.freeze({
    kind: 'direct-zip',
    operationId: 'operation',
    generation: 1n,
    phase: 'receiving',
    receivedSelectedBytes: 0n,
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

function receiverProgress(overrides: Partial<V2ReceiverProgress>): V2ReceiverProgress {
  return Object.freeze({
    discoveredFiles: 2,
    discoveredBytes: 1_024n,
    writtenBytes: 512n,
    completedFiles: 1,
    completedBytes: 512n,
    fileErrors: 0,
    selectionErrors: 0,
    contentLanes: 1,
    discovery: 'open',
    failedDirectories: 0,
    transferJobId: 'job',
    ...overrides,
  })
}
