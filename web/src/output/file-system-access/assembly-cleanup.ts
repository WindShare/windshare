import { recordOutputException, type OutputDiagnosticsPorts } from '../diagnostics'
import type { FSAFileCheckpointRepository } from './checkpoint-repository'
import type { CompatibleNamePathAuthority } from './compatible-name/coordinator'
import type { PersistentMaterializationPort } from '../persistent-tree/contracts'
import { outputTrace } from './session-diagnostics'

/** Close physical mutation first, retaining its native failure classification while releasing metadata authorities. */
export async function closeFSAOutputAuthorities(input: {
  readonly materialization: Pick<PersistentMaterializationPort, 'close'>
  readonly checkpoints: Pick<FSAFileCheckpointRepository, 'close'>
  readonly compatibleNames: Pick<CompatibleNamePathAuthority, 'close'>
  readonly diagnostics: OutputDiagnosticsPorts | undefined
}): Promise<void> {
  const failures: unknown[] = []
  try { await input.materialization.close() } catch (error) { failures.push(error) }
  for (const close of [() => input.checkpoints.close(), () => input.compatibleNames.close()]) {
    try { close() } catch (error) {
      failures.push(error)
      recordOutputException(input.diagnostics?.failures?.cleanup, error)
    }
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
  compatibleNames: CompatibleNamePathAuthority,
  diagnostics: OutputDiagnosticsPorts | undefined,
): unknown {
  const failures: unknown[] = []
  for (const close of [
    () => checkpoints?.close(),
    () => compatibleNames.close(),
  ]) {
    try {
      close()
    } catch (error) {
      failures.push(error)
      recordOutputException(diagnostics?.failures?.cleanup, error)
    }
  }
  if (failures.length === 0) return undefined
  if (failures.length === 1) return failures[0]
  return new AggregateError(
    failures,
    'FSA assembly cleanup could not close all compatible-name authorities',
  )
}
