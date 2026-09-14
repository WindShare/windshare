import { describe, expect, it } from 'vitest'
import { V2RemoteRevisionError } from '../../src/content/v2-session-services'
import { createFailureIdentity } from '../../src/diagnostics/incident'
import { CheckpointLineageDecisionError } from '../../src/output/persistent-tree/errors'
import { normalizeV2FileTransferFailure } from '../../src/transfer/job/failures'
import { TransferPauseRequestedError, type PlanPauseRequest } from '../../src/transfer/output-session'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../src/transfer/outcome'
import { persistentWorkspaceInterruption } from '../../src/transfer/settlement/persistent-evidence'

const SOURCE_STALE = 0x3001
const SOURCE_DRIFT = 0x3007
const SOURCE_UNREADABLE = 0x3003

describe('persistent workspace interruption semantics', () => {
  it.each([SOURCE_STALE, SOURCE_DRIFT])('ends an invalidated source revision (0x%s) without advertising continuation', code => {
    const source = revisionFailure(code)
    const normalized = normalizeV2FileTransferFailure(source)
    expect(normalized.kind).toBe('fault')
    expect(persistentWorkspaceInterruption(request(source))).toEqual({ kind: 'source-invalidated' })
    expect(persistentWorkspaceInterruption(request(normalized.diagnostic))).toEqual({ kind: 'source-invalidated' })
  })

  it.each([
    new TransferPauseRequestedError(),
    new DOMException('network interrupted', 'NetworkError'),
    new DOMException('disk full', 'QuotaExceededError'),
    new CheckpointLineageDecisionError('revision-conflict'),
    revisionFailure(SOURCE_UNREADABLE),
  ])('preserves ordinary interruption and local recovery semantics: %s', reason => {
    const interruption = request(reason)
    expect(persistentWorkspaceInterruption(interruption)).toEqual({
      kind: 'resumable', selectionFacts: interruption.selectionFacts,
    })
  })
})

function request(reason: unknown): PlanPauseRequest {
  return {
    worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
    materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 5n, discovery: 'complete' },
    reason,
  }
}

function revisionFailure(code: number): V2RemoteRevisionError {
  return new V2RemoteRevisionError({
    requestKind: 'open_revisions',
    content: { scope: 'revision', code, retryable: false },
    correlation: {
      protocolSessionId: createFailureIdentity('protocol_session', new Uint8Array(16).fill(1)),
      protocolOperationId: createFailureIdentity('protocol_operation', new Uint8Array(16).fill(2)),
      lane: { id: 1, epoch: 0 },
    },
  })
}
