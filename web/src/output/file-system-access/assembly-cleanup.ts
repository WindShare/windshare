import { recordOutputException, type OutputDiagnosticsPorts } from '../diagnostics'
import type { FSAFileCheckpointRepository } from './checkpoint-repository'
import type { CompatibleNamePathAuthority } from './compatible-name/coordinator'

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
