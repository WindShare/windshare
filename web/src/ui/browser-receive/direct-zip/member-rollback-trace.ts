import type { DirectZipMemberRollbackPayloadV1 } from '../../../diagnostics/trace/model'
import { emitOutputTrace, outputTraceEvent, type OutputTraceSource } from '../../../output/diagnostics'
import type { DirectZipMemberResumeDecisionV1 } from '../../../output/direct-zip/writer/model'

export interface DirectZipMemberRollbackTraceInput {
  readonly operationId: string
  readonly sessionId: string
  readonly candidateId: string
  readonly phase: DirectZipMemberRollbackPayloadV1['phase']
  readonly oldCommittedLength: bigint
  readonly newCommittedLength: bigint
  readonly retainedSelectedPayloadBytes: bigint
  readonly memberOrdinal: bigint
  readonly sourceChangeReason?: Extract<
    DirectZipMemberResumeDecisionV1, { readonly kind: 'rollback-member' }
  >['reason']
  readonly error?: unknown
}

export function traceDirectZipMemberRollback(
  trace: OutputTraceSource | undefined,
  input: DirectZipMemberRollbackTraceInput,
): void {
  emitOutputTrace(trace, () => outputTraceEvent('direct_zip_member_rollback', {
    operation_id: input.operationId,
    session_id: input.sessionId,
    candidate_id: input.candidateId,
    phase: input.phase,
    old_committed_length: input.oldCommittedLength.toString(),
    new_committed_length: input.newCommittedLength.toString(),
    retained_selected_payload_bytes: input.retainedSelectedPayloadBytes.toString(),
    member_ordinal: input.memberOrdinal.toString(),
    ...(input.sourceChangeReason === undefined ? {} : {
      source_change_reason: input.sourceChangeReason.replaceAll('-', '_') as
        NonNullable<DirectZipMemberRollbackPayloadV1['source_change_reason']>,
    }),
    ...(input.error instanceof Error ? { native_error_name: input.error.name } : {}),
  }))
}
