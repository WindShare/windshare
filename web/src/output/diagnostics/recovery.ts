import { compareRevisionMetadata, type RevisionSourceEvidence } from '../../content/revision-comparison'
import type { CheckpointLineageDecision, InitialCheckpointCASResult } from '../persistence/journal'
import type { FileCheckpointV2 } from '../persistence/checkpoint-model'
import { emitOutputTrace, outputTraceEvent } from './trace'
import type { OutputDiagnosticsPorts } from './ports'

export function traceSourceComparison(
  diagnostics: OutputDiagnosticsPorts | undefined,
  previous: FileCheckpointV2,
  current: RevisionSourceEvidence,
  stage: 'accepted' | 'retained',
): void {
  emitOutputTrace(diagnostics?.trace, () => {
    const comparison = compareRevisionMetadata(previous, current)
    const bytes = previous.verifiedRanges.reduce((total, range) => total + range.end - range.start, 0n)
    return outputTraceEvent('checkpoint', {
      backend: diagnostics?.backend === 'file_system_access' ? 'file_system_access' : 'origin_private',
      transition: 'source_comparison', operation_id: previous.operationId, file_id: previous.fileId,
      previous_revision: previous.fileRevision, current_revision: current.fileRevision,
      stage, comparison: comparison.kind, reason: comparison.reason,
      reused_bytes: (stage === 'accepted' ? bytes : 0n).toString(),
      retained_bytes: (stage === 'retained' ? bytes : 0n).toString(),
    })
  })
}

export function traceRetainedCandidates(
  diagnostics: OutputDiagnosticsPorts | undefined,
  decision: CheckpointLineageDecision | InitialCheckpointCASResult,
  current: RevisionSourceEvidence,
): void {
  if (decision.kind !== 'revision-conflict') return
  for (const previous of decision.records) traceSourceComparison(diagnostics, previous, current, 'retained')
}
