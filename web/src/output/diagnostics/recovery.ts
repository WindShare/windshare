import type { RevisionMetadataComparison } from '../../content/revision-comparison'
import type { CheckpointLineageDecision, InitialCheckpointCASResult } from '../persistence/journal'
import type { FileCheckpointV2 } from '../persistence/checkpoint-model'
import { emitOutputTrace, outputTraceEvent } from './trace'
import type { OutputDiagnosticsPorts } from './ports'

export function traceSourceComparison(
  diagnostics: OutputDiagnosticsPorts | undefined,
  previous: Pick<FileCheckpointV2, 'operationId' | 'fileId' | 'fileRevision' | 'verifiedRanges'>,
  currentRevision: string,
  comparison: RevisionMetadataComparison,
  stage: 'accepted' | 'retained',
): void {
  emitOutputTrace(diagnostics?.trace, () => {
    const bytes = previous.verifiedRanges.reduce((total, range) => total + range.end - range.start, 0n)
    return outputTraceEvent('checkpoint', {
      backend: diagnostics?.backend === 'file_system_access' ? 'file_system_access' : 'origin_private',
      transition: 'source_comparison', operation_id: previous.operationId, file_id: previous.fileId,
      previous_revision: previous.fileRevision, current_revision: currentRevision,
      stage, comparison: comparison.kind, reason: comparison.reason,
      reused_bytes: (stage === 'accepted' ? bytes : 0n).toString(),
      retained_bytes: (stage === 'retained' ? bytes : 0n).toString(),
    })
  })
}

export function traceRetainedCheckpointSource(
  diagnostics: OutputDiagnosticsPorts | undefined,
  decision: CheckpointLineageDecision | InitialCheckpointCASResult,
  currentRevision: string,
): void {
  if (decision.kind === 'revision-conflict' || decision.kind === 'revision-evidence-unavailable') {
    traceSourceComparison(diagnostics, decision.previous, currentRevision, decision.comparison, 'retained')
  }
}
