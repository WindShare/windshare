import { describe, expect, it } from 'vitest'
import {
  receiveInterruptionControls, receiveInterruptionReason, receiveStorageSettlement,
  TransferPauseRequestedError, TransferStopRequestedError,
} from '../../src/transfer/lifecycle/interruption'

const identity = { operationId: 'operation', receiveIntentDigest: 'intent', generation: 1n }

describe('receive interruption semantics', () => {
  it('exposes only the controls that each storage contract can settle', () => {
    expect(receiveInterruptionControls('direct-tree')).toEqual(['pause', 'stop'])
    expect(receiveInterruptionControls('direct-resumable-zip')).toEqual(['pause'])
    expect(receiveInterruptionControls('workspace-then-publish')).toEqual(['pause'])
    expect(receiveInterruptionControls('portable-handoff')).toEqual(['stop'])
    expect(receiveInterruptionReason('pause')).toBeInstanceOf(TransferPauseRequestedError)
    expect(receiveInterruptionReason('stop')).toBeInstanceOf(TransferStopRequestedError)
  })

  it('retains completed results after stopping and keeps publication cleanup retryable', () => {
    expect(receiveStorageSettlement({ ...identity, kind: 'partial-directory', reason: 'stopped',
      successCount: 1n, failureCount: 0n, receiptDigest: 'receipt' })).toEqual({
      task: 'ended', retention: 'complete-results', cleanup: 'settled',
    })
    expect(receiveStorageSettlement({ ...identity, kind: 'published', receiptDigest: 'receipt',
      cleanupState: 'cleanup-pending' })).toEqual({
      task: 'ended', retention: 'delivered-results', cleanup: 'retryable',
    })
  })

  it('distinguishes retained saveable payload from a browser handoff without a saved-file receipt', () => {
    expect(receiveStorageSettlement({ ...identity, kind: 'waiting-to-save', packageDigest: 'package' }).retention)
      .toBe('complete-results')
    expect(receiveStorageSettlement({ ...identity, kind: 'download-started', attemptKind: 'portable', attemptId: 'attempt' }).retention)
      .toBe('browser-handoff')
  })
})
