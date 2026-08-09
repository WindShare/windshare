import { advanceV2DirectoryFrame } from '../discovery/v2-directory-stack'
import {
  DirectorySettlementKind,
  validateDirectorySettlement,
  type DirectoryAdmission,
  type DirectorySettlement,
} from '../directory-admission'
import type { IncrementalDirectoryOutput } from '../output-session'
import type { DirectoryWork, PendingFile } from './contract'
import type { AsyncBoundedQueue } from './scheduler'

export interface V2DirectoryTransferRunner {
  readonly signal: AbortSignal
  readonly discoverDirectory: (
    work: DirectoryWork,
    files: AsyncBoundedQueue<PendingFile>,
  ) => AsyncGenerator<DirectoryWork, void>
  readonly isolateDirectory: (directoryId: string, error: unknown) => void
}

export async function runV2DirectoryTransferWorker(
  directories: AsyncBoundedQueue<DirectoryWork>,
  files: AsyncBoundedQueue<PendingFile>,
  runner: V2DirectoryTransferRunner,
): Promise<void> {
  while (true) {
    const work = await directories.pop(runner.signal)
    if (work === undefined) return
    try {
      await runDirectoryStack(work, directories, files, runner)
    } finally {
      directories.taskDone()
    }
  }
}

async function runDirectoryStack(
  initial: DirectoryWork,
  directories: AsyncBoundedQueue<DirectoryWork>,
  files: AsyncBoundedQueue<PendingFile>,
  runner: V2DirectoryTransferRunner,
): Promise<void> {
  const stack: Array<{
    readonly work: DirectoryWork
    readonly discovery: AsyncGenerator<DirectoryWork, void>
  }> = [{ work: initial, discovery: runner.discoverDirectory(initial, files) }]
  try {
    while (stack.length > 0) {
      const frame = stack[stack.length - 1]
      if (frame === undefined) throw new Error('Directory discovery stack lost its active frame')
      const next = await advanceV2DirectoryFrame(frame, runner.isolateDirectory)
      if (next === undefined) {
        stack.pop()
        continue
      }
      if (next.done) {
        stack.pop()
        continue
      }
      if (directories.tryPush(next.value, runner.signal)) continue

      // A worker must not wait for a peer to consume its own output. The depth
      // stack retains a bounded breadth queue without recursion or starvation.
      stack.push({
        work: next.value,
        discovery: runner.discoverDirectory(next.value, files),
      })
    }
  } finally {
    while (stack.length > 0) await stack.pop()?.discovery.return(undefined)
  }
}

export interface V2DirectoryFinalization {
  readonly admissions: readonly DirectoryAdmission[]
  readonly output: IncrementalDirectoryOutput
  readonly signal: AbortSignal
  readonly settled: (
    admission: DirectoryAdmission,
    settlement: DirectorySettlement,
  ) => void
  readonly failed: (admission: DirectoryAdmission, error: unknown) => void
}

export async function finalizeV2Directories(finalization: V2DirectoryFinalization): Promise<void> {
  const ordered = [...finalization.admissions].sort((left, right) =>
    right.path.length - left.path.length)
  for (const admission of ordered) {
    let settlement: DirectorySettlement
    try {
      finalization.signal.throwIfAborted()
      settlement = validateDirectorySettlement(
        admission,
        await finalization.output.finalizeDirectory(admission, finalization.signal),
      )
    } catch (error) {
      finalization.failed(admission, error)
      continue
    }
    finalization.settled(admission, settlement)
  }
}

export function directorySettlementDecision(
  settlement: DirectorySettlement,
): 'accepted' | 'isolated-failure' {
  return settlement.kind === DirectorySettlementKind.Finalized
    ? 'accepted'
    : 'isolated-failure'
}
