import { recordOutputException, type OutputDiagnosticsPorts } from '../diagnostics'
import type { FSAFileCheckpointRepository } from './checkpoint-repository'
import type { PersistentMaterializationPort } from '../persistent-tree/contracts'
import { outputTrace } from './session-diagnostics'

/** Close physical mutation first, retaining its native failure classification while releasing metadata authorities. */
export async function closeFSAOutputAuthorities(input: {
  readonly materialization: Pick<PersistentMaterializationPort, 'close'>
  readonly checkpoints: Pick<FSAFileCheckpointRepository, 'close'>
  readonly diagnostics: OutputDiagnosticsPorts | undefined
}): Promise<void> {
  const failures: unknown[] = []
  try { await input.materialization.close() } catch (error) { failures.push(error) }
  try {
    input.checkpoints.close()
  } catch (error) {
    failures.push(error)
    recordOutputException(input.diagnostics?.failures?.cleanup, error)
  }
  if (failures.length !== 0) {
    outputTrace(input.diagnostics, { eventName: 'cleanup', transition: 'failed' })
    if (failures.length === 1) throw failures[0]
    throw new AggregateError(failures, 'FSA output repositories did not close cleanly')
  }
  outputTrace(input.diagnostics, { eventName: 'cleanup', transition: 'completed' })
}

export function closeFailedFSAAssembly(
  checkpoints: FSAFileCheckpointRepository | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
): unknown {
  try {
    checkpoints?.close()
    return undefined
  } catch (error) {
    recordOutputException(diagnostics?.failures?.cleanup, error)
    return error
  }
}
