import { decodePreparationAdmissionAuthority } from '../receipts'
import { RECEIVE_RECORD_RECEIPT } from '../records'
import { nextReceiveLifecycleState, type ReceiveLifecycleState } from '../state'
import type { WorkspaceStageRuntime } from './runtime'

type ResumableStart = Extract<ReceiveLifecycleState, { kind: 'resumable-start' }>

/** An unsuccessful admission retires an attempt, not the user's immutable receive intent. */
export class WorkspaceStartupStages {
  readonly runtime: WorkspaceStageRuntime

  constructor(runtime: WorkspaceStageRuntime) { this.runtime = runtime }

  async retain(reason: ResumableStart['reason']): Promise<ResumableStart> {
    const state = await this.runtime.lifecycle()
    if (state.kind === 'resumable-start') return state
    if (state.kind !== 'intent-frozen' && state.kind !== 'receiving') {
      throw new TypeError('Only an unstarted workspace execution can retain startup')
    }
    this.runtime.requireZeroContentRequests()
    const records = await this.runtime.repository.listRecords(this.runtime.intent.operationId, RECEIVE_RECORD_RECEIPT)
    const admissionIds: string[] = []
    for (const record of records) {
      const admission = await decodePreparationAdmissionAuthority(record, this.runtime.intent)
      if (admission !== undefined) admissionIds.push(record.id)
    }
    const next = nextReceiveLifecycleState(state, { kind: 'resumable-start', reason, contentWarning: null }) as ResumableStart
    // A later attempt must not see multiple capacity admissions for the same task.
    await this.runtime.repository.commitTransition({
      operationId: state.operationId, expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId, deleteRecordIds: admissionIds, lifecycle: next,
    })
    this.runtime.emit({ name: 'receive.start.interrupted', operation_id: state.operationId,
      receive_intent_digest: state.receiveIntentDigest, prior_state: state.kind, reason })
    return next
  }

  async retry(): Promise<ReceiveLifecycleState> {
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'resumable-start') throw new TypeError('Workspace has no retained startup')
    const next = nextReceiveLifecycleState(state, { kind: 'intent-frozen', contentWarning: null })
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({ name: 'receive.start.retried', operation_id: state.operationId,
      receive_intent_digest: state.receiveIntentDigest })
    return next
  }
}
