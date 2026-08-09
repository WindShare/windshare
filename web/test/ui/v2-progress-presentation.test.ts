import { describe, expect, it } from 'vitest'

import type { V2ReceiverProgress } from '../../src/ui/v2-model'
import {
  completionProgressDescription,
  discoveryProgressDescription,
} from '../../src/ui/v2-progress-presentation'

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
})

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
