import type { V2BoundReceiveOperation, V2DirectZipProgressSnapshot } from '../../../src/ui/v2-receive-runtime'
import { presentDirectZipProgress } from '../../../src/ui/v2-progress-presentation'
import type { ReceiveLifecycleState } from '../../../src/output/workspace/state'

export function observeProductionDirectZipProgress(selectedBytes: bigint) {
  const samples: Record<string, ReturnType<typeof capture>> = {}
  const notifications: ReturnType<typeof counters>[][] = []
  let unsubscribe: (() => void) | undefined

  function capture(operation: V2BoundReceiveOperation, lifecycle: ReceiveLifecycleState = operation.lifecycle) {
    const progress = operation.outputProgress?.getSnapshot()
    if (progress?.kind !== 'direct-zip') throw new Error('Production operation lost Direct ZIP progress')
    const presentation = presentDirectZipProgress({
      progress, selectedBytes: { kind: 'exact', bytes: selectedBytes }, lifecycle,
    })
    return { ...counters(progress), primary: presentation.primary,
      percentage: presentation.percentage?.toString(), safeResume: presentation.safeResume }
  }

  return {
    bind: (operation: V2BoundReceiveOperation) => {
      unsubscribe?.()
      const events: ReturnType<typeof counters>[] = []
      notifications.push(events)
      unsubscribe = operation.outputProgress?.subscribe(progress => {
        if (progress.kind !== 'direct-zip') throw new Error('Production progress changed its output kind')
        events.push(counters(progress))
      })
    },
    sample: (name: string, operation: V2BoundReceiveOperation, lifecycle?: ReceiveLifecycleState) => {
      samples[name] = capture(operation, lifecycle)
    },
    result: () => ({ samples, notifications }),
    close: () => unsubscribe?.(),
  }
}

function counters(progress: V2DirectZipProgressSnapshot) {
  return { operationId: progress.operationId, generation: progress.generation.toString(),
    received: progress.receivedSelectedBytes.toString(), written: progress.writtenSelectedBytes.toString(),
    safe: progress.safeResumeBytes.toString() }
}
